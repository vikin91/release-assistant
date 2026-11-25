package app

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"release-assistant/internal/apilog"
	"release-assistant/internal/dispatch"
	"release-assistant/internal/githubread"
	"release-assistant/internal/konflux"
	"release-assistant/internal/release"
)

type (
	// Options controls polling and bounds each source's work.
	Options struct {
		RefreshIntervals                     RefreshIntervals
		Poll, Discovery, BackoffCap, Timeout time.Duration
		Initial                              release.Config
		Cache                                DiscoveryCache
		GitHubReads                          *githubread.Transport
		CIPlanner                            RetryPlanner
		// KonfluxProbe reads service health independently of tracked release builds.
		KonfluxProbe func(context.Context) konflux.Health
		// ReleaseState persists the user's PR, tag, and pinning choices across sessions.
		ReleaseState *ReleaseState
		// PinningPROverride indicates that the initial pinning PR was explicitly supplied.
		PinningPROverride bool
		// Profiles enables saved release navigation over the session.
		Profiles bool
	}
	// Cycle retains observed builds and images when a confirmed component tag changes.
	Cycle struct {
		Component, Tag, SHA string
		At                  time.Time
		Images              []release.Image
		Plan                release.Plan
	}
	// Snapshot is a copied display state; service credentials are never included.
	Snapshot struct {
		RefreshIntervals             RefreshIntervals
		Config                       release.Config
		Choices                      release.Choices
		Repositories                 []release.Repository
		Inventory                    release.Inventory
		Plans                        []release.Plan
		ManualRetries                []release.ManualRetryReport
		PRRetryChecks                map[release.Target]PRRetryCheck
		RetestQueue                  []RetestItem
		RetestLog                    []RetestDecision
		Assessment                   release.Assessment
		Authority                    []dispatch.View
		Interactive, RequirePRLabel  bool
		CacheAvailable, CacheEnabled bool
		StartupReadLimit             int
		OngoingReadLimit             int
		GitHubReadBudget             githubread.Budget
		GitHubReadStats              []githubread.ReadStats
		ScheduledCIStats             []ScheduledCIStats
		// WritesEnabled reports startup permission; target grants remain independent.
		WritesEnabled   bool
		Error, LogError string
		Generation      uint64
		History         []Cycle
		LogEntries      []Record
		// KonfluxReadIssues reports endpoint reachability independently of release readiness.
		KonfluxReadIssues []konflux.ReadIssue
		// KonfluxHealth holds the latest independent health observation, when enabled.
		KonfluxHealth *konflux.Health
		// PendingTags holds saved selections awaiting current GitHub evidence.
		PendingTags map[string]string
		// RefreshingTags names components with an outstanding manual inventory refresh.
		RefreshingTags map[string]bool
		// PendingPinningPR holds the saved PR number until discovery verifies its branch.
		PendingPinningPR int
		// Loading keeps the dashboard refreshing during startup and while included ACS PRs await CI results.
		Loading bool
		// ImagesRefreshing keeps the dashboard updating while registry results arrive.
		ImagesRefreshing bool
		// Monitoring reports whether this snapshot comes from the active release.
		Monitoring bool
		// PinningPreview reflects the current tags, images, and duplicate evidence.
		PinningPreview PinningPreview
		// CreatedPinningPR restores the last successful creation for the release.
		CreatedPinningPR dispatch.PinningReceipt
		// ResumedAt identifies the latest activation of previously observed work.
		ResumedAt time.Time
		// RefreshingResumed reports overdue observations that survived a release switch.
		RefreshingResumed bool
	}
	// Action is a typed browser or CLI session decision.
	Action struct {
		RefreshIntervals        RefreshIntervals
		Kind                    string
		Config                  release.Config
		Target                  release.Target
		SHA, ID, Component, Tag string
		Choice                  release.Choice
		Enabled                 bool
		// Nil settings are omitted from the submitted form.
		Interactive, RequirePRLabel, CacheEnabled *bool
		StartupReadLimit                          *int
		OngoingReadLimit                          *int
		Generation                                uint64
		QueueID                                   uint64
		ProfileID                                 string
	}
	// Session owns release observations. Dispatcher alone owns write grants.
	Session struct {
		mu sync.Mutex
		*releaseWork
		profiles      map[string]*releaseWork
		activeProfile string
		generation    uint64
		workRevision  uint64
		pollChanged   chan struct{}
		ciRetryJobs   map[release.Target]scheduledPRRetry
		ciRunning     scheduledPRRetry
		startupUntil  time.Time
		konfluxHealth *konflux.Health
		discovery     Discoverer
		planner       RetryPlanner
		images        Inspector
		dispatcher    *dispatch.Dispatcher
		log           *Log
		options       Options
		changed       chan struct{}
		monitorCancel context.CancelFunc

		dashboards     dashboardFile
		dashboardDirty bool
	}
	releaseWork struct {
		config           release.Config
		choices          release.Choices
		repos            map[string]release.Repository
		plans            map[release.Target]release.Plan
		prRefreshHeads   map[release.Target]string
		tagChecks        map[string]tagCheckObservation
		manualRetries    map[release.Target]release.ManualRetryReport
		prRetryChecks    map[release.Target]PRRetryCheck
		pendingPRChecks  map[release.Target]bool
		retestQueue      []RetestItem
		retestLog        []RetestDecision
		nextRetestID     uint64
		inventory        release.Inventory
		imageRefresh     uint64
		imageReads       int
		confirmedSHAs    map[string]string
		pendingTags      map[string]savedComponentTag
		tagInitialized   map[string]bool
		polls            map[pollID]pollRecord
		initiallyLoaded  map[string]bool
		priorPinChoice   release.Choice
		readErrors       map[string]string
		errorText        string
		history          []Cycle
		pendingPinningPR int
		resumedAt        time.Time

		creatingPinningPR bool
		createdPinningPR  dispatch.PinningReceipt
	}
)

// New constructs an unconfigured session with safe restart defaults.
func New(d Discoverer, p RetryPlanner, i Inspector, dispatcher *dispatch.Dispatcher, log *Log, o Options) *Session {
	intervals := DefaultRefreshIntervals(o.Poll, o.Discovery)
	for key := range intervals {
		if d := o.RefreshIntervals[key]; d > 0 {
			intervals[key] = d
		}
	}
	o.RefreshIntervals = intervals
	if o.Poll <= 0 {
		o.Poll = time.Minute
	}
	if o.BackoffCap <= 0 {
		o.BackoffCap = 5 * time.Minute
	}
	if o.Timeout <= 0 {
		o.Timeout = 45 * time.Second
	}
	s := &Session{discovery: d, planner: p, images: i, dispatcher: dispatcher, log: log, options: o, changed: make(chan struct{}, 1), pollChanged: make(chan struct{})}
	if o.KonfluxProbe != nil {
		s.konfluxHealth = &konflux.Health{}
	}
	s.releaseWork = newReleaseWork()
	s.generation++
	s.workRevision++
	if o.Profiles && o.ReleaseState != nil {
		if log != nil {
			log.mu.Lock()
			log.releaseLabels = true
			for i := range log.recent {
				log.recent[i].labeled = true
			}
			log.mu.Unlock()
		}
		s.profiles = make(map[string]*releaseWork)
		s.loadDashboards()
		s.activateProfile(o.ReleaseState.file.ActiveProfile)
		return s
	}
	s.options.Profiles = false
	s.config = o.Initial
	s.config.Components = slices.Clone(o.Initial.Components)
	if o.Initial.Version != "" {
		if err := o.Initial.Validate(); err != nil {
			s.errorText = err.Error()
		} else {
			s.restorePRChoices()
			s.restoreComponentTags(s.options.ReleaseState)
			if !o.PinningPROverride && s.config.PinningPR == 0 && !s.config.DetectPinningPR {
				s.pendingPinningPR = o.ReleaseState.restorePinningChoice(&s.config)
			}
			s.includePinningPR()
		}
	}
	return s
}
func newReleaseWork() *releaseWork {
	return &releaseWork{
		choices:         release.Choices{PRs: map[release.Target]release.Choice{}, Tags: map[string]string{}, Acknowledged: map[string]bool{}},
		repos:           map[string]release.Repository{},
		plans:           map[release.Target]release.Plan{},
		prRefreshHeads:  map[release.Target]string{},
		manualRetries:   map[release.Target]release.ManualRetryReport{},
		prRetryChecks:   map[release.Target]PRRetryCheck{},
		pendingPRChecks: map[release.Target]bool{},
		tagChecks:       make(map[string]tagCheckObservation),
		confirmedSHAs:   map[string]string{},
		pendingTags:     map[string]savedComponentTag{},
		tagInitialized:  map[string]bool{},
		polls:           make(map[pollID]pollRecord),
		initiallyLoaded: map[string]bool{},
		readErrors:      map[string]string{},
		inventory:       release.Inventory{},
	}
}
func (s *Session) record(target release.Target, sha, decision, payload, result, origin string, message ...string) {
	if s.log != nil {
		r := Record{At: time.Now(), Release: s.config.Version, Target: target, SHA: sha, Decision: decision, Payload: payload, Result: result, Origin: origin}
		if len(message) > 0 {
			r.message = message[0]
		}
		s.log.Append(r)
	}
}
func (s *Session) drain() {
	for _, e := range s.dispatcher.Events() {
		if s.log != nil {
			s.log.Append(Record{At: e.At, Release: cmp.Or(e.Release, s.config.Version), Target: e.Target, SHA: e.SHA, Decision: e.Decision, Payload: e.Payload, Result: e.Result, Origin: e.Origin})
		}
	}
}

// recordReadResult logs changed failures and recovery without repeating unchanged polls.
func (s *Session) recordReadResult(target release.Target, operation, message string, description ...string) {
	key := operation + " " + target.String()
	previous := s.readErrors[key]
	if message == previous {
		return
	}
	if message == "" {
		delete(s.readErrors, key)
		console := ""
		if len(description) > 0 {
			console = fmt.Sprintf("Reading %s is working again.", description[0])
		}
		s.record(target, "", operation+"-recovered", "", "Reads recovered.", "system", console)
		return
	}
	s.readErrors[key] = message
	console := ""
	if len(description) > 0 {
		console = fmt.Sprintf("Could not read %s. %s", description[0], message)
	}
	s.record(target, "", operation+"-error", "", message, "system", console)
}

// Apply validates configuration and enforces session actions independently of the UI.
func (s *Session) Apply(ctx context.Context, a Action) error {
	if s.options.Profiles && (a.Kind == "add-release" || a.Kind == "remove-release" || a.Kind == "open-release" || a.Kind == "activate-release" || a.Kind == "configure") {
		return s.applyProfile(a)
	}
	if a.Kind == "create-pinning-pr" {
		return s.createPinningPR(ctx, a)
	}
	if a.Kind == "use-pinning-pr" {
		return s.confirmPinningPR(ctx, a)
	}
	if a.Kind == "check-pr-retries" {
		return s.checkPRRetries(ctx, a)
	}
	s.mu.Lock()
	if a.Kind != "configure" && a.Generation != s.generation {
		s.mu.Unlock()
		return fmt.Errorf("page belongs to an earlier configuration; reload")
	}
	shared := a.Kind == "clear-cache" || (a.Kind == "application-settings" && a.Interactive == nil && a.RequirePRLabel == nil)
	if !s.targetsActiveProfile(a.ProfileID) && !shared {
		s.mu.Unlock()
		return fmt.Errorf("activate this release to continue")
	}
	ctx = apilog.WithRelease(ctx, s.config.Version)
	if a.Kind == "queue-send" || a.Kind == "queue-skip" {
		return s.applyRetestDecision(ctx, a)
	}
	defer s.mu.Unlock()
	var err error
	switch a.Kind {
	case "refresh-tags":
		if !slices.ContainsFunc(s.config.Components, func(source release.Source) bool {
			return source.Name == a.Component
		}) {
			return fmt.Errorf("unknown component")
		}
		id := pollID{a.Component, pollTags}
		record := s.polls[id]
		record.tagRefresh = rand.Text()
		s.polls[id] = record
		s.wakePolls()
		return nil
	case "refresh-images":
		if err := s.config.Validate(); err != nil {
			return err
		}
		s.imageRefresh++
		s.wakePolls()
		return nil
	case "configure":
		if err = a.Config.Validate(); err != nil {
			return err
		}
		pendingPinningPR, err := s.configurePinningChoice(&a.Config)
		if err != nil {
			return err
		}
		sameContext := s.config.Version == a.Config.Version && s.config.Branch == a.Config.Branch && slices.Equal(s.config.Components, a.Config.Components)
		pinChanged := !sameContext || s.config.PinningPR != a.Config.PinningPR || s.pendingPinningPR != pendingPinningPR
		if sameContext {
			if pinChanged {
				s.clearPinningSelection()
			}
			s.generation++
		} else {
			s.dispatcher.Reset()
			s.releaseWork = newReleaseWork()
			s.generation++
		}
		s.config = a.Config
		s.pendingPinningPR = pendingPinningPR
		if !sameContext {
			s.restorePRChoices()
			s.restoreComponentTags(s.options.ReleaseState)
		}
		if pinChanged {
			s.includePinningPR()
		}
		s.config.Components = slices.Clone(a.Config.Components)
		s.polls = make(map[pollID]pollRecord)
		s.initiallyLoaded = map[string]bool{}
		s.workRevision++
		s.errorText = ""
		s.record(release.Target{}, "", "configure", "", "new selection context", "user")
		select {
		case s.changed <- struct{}{}:
		default:
		}
	case "choice", "component-pr-choices", "acs-pr-choices":
		switch a.Choice {
		case release.Included, release.Excluded, release.Ignored:
		case "":
			if a.Kind != "choice" {
				return fmt.Errorf("bulk selection requires an inclusion choice")
			}
		default:
			return fmt.Errorf("invalid inclusion choice")
		}
		var targets map[release.Target]string
		if a.Kind == "choice" {
			if a.Target.PR < 1 || a.Target.Tag != "" {
				return fmt.Errorf("invalid pull request target")
			}
			if a.Target == s.config.PinningTarget() {
				return fmt.Errorf("the confirmed pinning PR is always included")
			}
			if s.choices.PRs[a.Target] != release.Ignored && !s.hasTarget(a.Target) {
				return fmt.Errorf("unknown target")
			}
			targets = map[release.Target]string{a.Target: a.SHA}
		} else {
			source := s.config.ACS()
			if a.Kind == "component-pr-choices" {
				index := slices.IndexFunc(s.config.Components, func(source release.Source) bool {
					return source.Name == a.Component
				})
				if index < 0 {
					return fmt.Errorf("unknown component for bulk PR selection")
				}
				source = s.config.Components[index]
			}
			targets = s.bulkPRTargets(source, a.Choice)
			if len(targets) == 0 {
				return nil
			}
		}
		if err := s.options.ReleaseState.savePRChoices(s.config.Version, targets, a.Choice); err != nil {
			return err
		}
		for target, sha := range targets {
			s.setPRChoice(target, sha, a.Choice)
		}
		s.workRevision++
		select {
		case s.changed <- struct{}{}:
		default:
		}
	case "grant":
		rc := s.repos["acs"].Candidate
		isRC := a.Target.Tag != "" && a.Target == rc.Target
		if (isRC && (!s.rcRetriesReady() || s.plans[a.Target].SHA != rc.SHA)) || (!isRC && !s.hasTarget(a.Target)) {
			return fmt.Errorf("unknown target")
		}
		err = s.dispatcher.Grant(a.Target, a.SHA, a.Enabled)
	case "application-settings":
		if err := a.RefreshIntervals.Validate(); err != nil {
			return err
		}
		if a.CacheEnabled != nil && s.options.Cache == nil {
			return fmt.Errorf("discovery cache is unavailable")
		}
		if (a.Interactive != nil || a.RequirePRLabel != nil) && (!s.dispatcher.WritesEnabled() || s.config.Validate() != nil) {
			return fmt.Errorf("GitHub write settings are unavailable")
		}
		if a.StartupReadLimit != nil || a.OngoingReadLimit != nil {
			if s.options.GitHubReads == nil {
				return fmt.Errorf("GitHub read settings are unavailable")
			}
			startup, ongoing := s.options.GitHubReads.ReadLimits()
			if err := s.options.GitHubReads.SetReadLimits(a.StartupReadLimit, a.OngoingReadLimit); err != nil {
				return err
			}
			recordSettingChange(s, "Concurrent GitHub reads (first 5 minutes)", startup, a.StartupReadLimit)
			recordSettingChange(s, "Concurrent GitHub reads (after 5 minutes)", ongoing, a.OngoingReadLimit)
		}
		s.dispatcher.SetWriteMode(a.Interactive, a.RequirePRLabel)
		if a.CacheEnabled != nil {
			previous := s.options.Cache.Enabled()
			s.options.Cache.SetEnabled(*a.CacheEnabled)
			recordSettingChange(s, "Reuse GitHub commit history in memory", previous, a.CacheEnabled)
		}
		changed := false
		for key, interval := range a.RefreshIntervals {
			if previous := s.options.RefreshIntervals[key]; previous != interval {
				s.options.RefreshIntervals[key] = interval
				s.record(release.Target{}, "", "setting-changed", "", fmt.Sprintf("Refresh interval %s changed from %s to %s.", key, previous, interval), "user")
				changed = true
			}
		}
		if changed {
			s.wakePolls()
		}
		s.drain()
		return nil
	case "interactive":
		s.dispatcher.Interactive(a.Enabled)
	case "cache-enabled", "clear-cache":
		if s.options.Cache == nil {
			return fmt.Errorf("discovery cache is unavailable")
		}
		if a.Kind == "clear-cache" {
			s.options.Cache.Clear()
		} else {
			s.options.Cache.SetEnabled(a.Enabled)
		}
		return nil
	case "tag":
		r, ok := s.repos[a.Component]
		configuredSource, configured := release.Source{}, false
		for _, source := range s.config.Components {
			if source.Name == a.Component {
				configuredSource = source
				configured = true
				break
			}
		}
		major, minor, _, final := release.Version(a.Tag)
		if !configured || !final || fmt.Sprintf("%d.%d", major, minor) != configuredSource.Stream {
			return fmt.Errorf("tag must be a final version in the configured component stream")
		}
		if !ok {
			return fmt.Errorf("unknown component")
		}
		var found *release.Tag
		for _, tag := range r.Tags {
			if tag.Name == a.Tag && tag.SHA != "" && tag.SHA == a.SHA && tag.Belongs == release.Yes && tag.Evidence.Fresh() {
				found = &tag
				break
			}
		}
		if found == nil {
			return fmt.Errorf("tag evidence changed or eligibility unknown; reload")
		}
		if err := s.options.ReleaseState.saveComponentTag(s.config.Version, configuredSource, a.Tag, a.SHA); err != nil {
			return err
		}
		delete(s.pendingTags, a.Component)
		if old := s.choices.Tags[a.Component]; old != "" && old != a.Tag {
			s.archiveTag(a.Component, r.Source.Repo)
		}
		s.workRevision++
		s.selectComponentTag(configuredSource, a.Tag, a.SHA)
		s.dispatcher.Ignore(release.Target{Repo: r.Source.Repo, Tag: a.Tag}, false)
		s.record(release.Target{Repo: r.Source.Repo, Tag: a.Tag}, a.SHA, "tag-confirmed", "", a.Tag, "user")
		select {
		case s.changed <- struct{}{}:
		default:
		}
	case "clear-tag":
		var source release.Source
		for _, candidate := range s.config.Components {
			if candidate.Name == a.Component {
				source = candidate
				break
			}
		}
		if source.Name == "" {
			return fmt.Errorf("unknown component")
		}
		if err := s.options.ReleaseState.saveComponentTag(s.config.Version, source, "", ""); err != nil {
			return err
		}
		s.archiveTag(a.Component, source.Repo)
		delete(s.choices.Tags, a.Component)
		delete(s.confirmedSHAs, a.Component)
		delete(s.pendingTags, a.Component)
		s.tagInitialized[a.Component] = true
		s.workRevision++
		s.record(release.Target{Repo: source.Repo}, "", "tag-cleared", "", "Preparing a new component tag.", "user", fmt.Sprintf("Preparing a new %s tag. Pull request monitoring continues.", a.Component))
		select {
		case s.changed <- struct{}{}:
		default:
		}
	case "acknowledge":
		found := false
		for _, p := range s.plans {
			for _, v := range p.Advisories {
				if v.ID == a.ID {
					found = true
				}
			}
		}
		if !found {
			return fmt.Errorf("advisory no longer present")
		}
		s.choices.Acknowledged[a.ID] = a.Enabled
		s.record(a.Target, a.SHA, "conforma-acknowledged", "", fmt.Sprint(a.Enabled), "user")
	default:
		return fmt.Errorf("unknown action")
	}
	s.revalidateRCPlan()
	s.drain()
	return err
}

// archiveTag retains a selected cycle and revokes authority when its tag is left.
// The caller holds the session lock.
func (s *Session) archiveTag(component, repo string) {
	tag := s.choices.Tags[component]
	if tag == "" {
		return
	}
	target := release.Target{Repo: repo, Tag: tag}
	cycle := Cycle{Component: component, Tag: tag, SHA: s.confirmedSHAs[component], At: time.Now(), Plan: clonePlan(s.plans[target])}
	for _, image := range s.inventory.Images {
		if image.Component == component {
			cycle.Images = append(cycle.Images, image)
		}
	}
	s.history = append(s.history, cycle)
	s.dispatcher.Ignore(target, true)
	delete(s.plans, target)
	delete(s.manualRetries, target)
	delete(s.tagChecks, component)
}

// selectComponentTag schedules initial coverage, checks, and builds for the selected commit.
func (s *Session) selectComponentTag(source release.Source, tag, sha string) {
	if s.choices.Tags[source.Name] != tag || s.confirmedSHAs[source.Name] != sha {
		for _, kind := range []pollKind{pollPRs, pollTagChecks, pollBuilds} {
			delete(s.polls, pollID{source.Repo, kind})
		}
	}
	s.choices.Tags[source.Name] = tag
	s.confirmedSHAs[source.Name] = sha
	s.tagInitialized[source.Name] = true
}

func (s *Session) setPRChoice(target release.Target, sha string, choice release.Choice) {
	if choice == release.Included && s.choices.PRs[target] != release.Included {
		s.pendingPRChecks[target] = true
		delete(s.polls, pollID{target.Repo, pollChecks})
		delete(s.polls, pollID{target.Repo, pollBuilds})
	}
	if choice != release.Included {
		delete(s.prRetryChecks, target)
		delete(s.pendingPRChecks, target)
	}
	if choice == "" {
		delete(s.choices.PRs, target)
	} else {
		s.choices.PRs[target] = choice
	}
	ignored := choice != release.Included
	s.dispatcher.Ignore(target, ignored)
	if ignored {
		delete(s.plans, target)
		delete(s.manualRetries, target)
	}
	s.record(target, sha, "inclusion", "", string(choice), "user")
}

func (s *Session) bulkPRTargets(source release.Source, choice release.Choice) map[release.Target]string {
	targets := make(map[release.Target]string)
	pinning := s.config.PinningTarget()
	for _, pr := range s.repos[source.Name].PRs {
		current := s.choices.PRs[pr.Target]
		if pr.Target.Repo != source.Repo || pr.Target == pinning || current == choice || !pr.Selectable(current) {
			continue
		}
		targets[pr.Target] = pr.SHA
	}
	return targets
}

func (s *Session) hasTarget(t release.Target) bool {
	for _, r := range s.repos {
		for _, p := range r.PRs {
			if p.Target == t {
				return true
			}
		}
		if t.Repo == r.Source.Repo && t.Tag != "" && s.choices.Tags[r.Source.Name] == t.Tag {
			return true
		}
	}
	return false
}

// Snapshot derives readiness on demand, including immediately after inclusion edits.
func (s *Session) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot(s.releaseWork, true)
}

func (s *Session) snapshot(work *releaseWork, active bool) Snapshot {
	profileID := ""
	if work == s.releaseWork {
		profileID = s.activeProfile
	}
	return s.profileSnapshot(work, active, profileID)
}

func (s *Session) profileSnapshot(work *releaseWork, active bool, profileID string) Snapshot {
	c := work.config
	c.Components = slices.Clone(c.Components)
	choices := work.choices
	choices.PRs = maps.Clone(choices.PRs)
	choices.Tags = maps.Clone(choices.Tags)
	choices.Acknowledged = maps.Clone(choices.Acknowledged)
	x := Snapshot{
		RefreshIntervals: maps.Clone(s.options.RefreshIntervals), Config: c, Choices: choices, Error: work.errorText, Generation: s.generation,
		PendingPinningPR: work.pendingPinningPR, ResumedAt: work.resumedAt,
		Monitoring: active, Interactive: true, PRRetryChecks: make(map[release.Target]PRRetryCheck, len(work.prRetryChecks)),
	}
	if active {
		x.Interactive, x.RequirePRLabel = s.dispatcher.IsInteractive(), s.dispatcher.RequiresPRLabel()
		x.WritesEnabled, x.Authority = s.dispatcher.WritesEnabled(), s.dispatcher.Views()
		x.RefreshingResumed = s.hasOverdueObservations()
		x.ImagesRefreshing = work.imageReads > 0 || work.imageRefresh > work.polls[pollID{c.ACS().Repo, pollImages}].imageRefresh
		for _, source := range c.Components {
			if work.polls[pollID{source.Name, pollTags}].tagRefresh != "" {
				if x.RefreshingTags == nil {
					x.RefreshingTags = make(map[string]bool)
				}
				x.RefreshingTags[source.Name] = true
				x.Loading = true
			}
		}
	}
	if len(work.pendingTags) > 0 {
		x.PendingTags = make(map[string]string, len(work.pendingTags))
		for component, saved := range work.pendingTags {
			x.PendingTags[component] = saved.Tag
		}
	}
	for target, check := range work.prRetryChecks {
		check.Konflux = clonePlan(check.Konflux)
		check.CI = cloneManualRetryReport(check.CI)
		x.PRRetryChecks[target] = check
	}
	s.fillSharedSnapshot(&x)
	if active && c.Validate() == nil {
		for _, source := range append([]release.Source{c.ACS()}, c.Components...) {
			if !work.initiallyLoaded[source.Name] || (work.repos[source.Name].TagsPending && work.choices.Tags[source.Name] == "" && s.duringStartup()) {
				x.Loading = true
				break
			}
		}
	}
	for _, cycle := range work.history {
		cycle.Images = slices.Clone(cycle.Images)
		cycle.Plan = clonePlan(cycle.Plan)
		x.History = append(x.History, cycle)
	}
	for _, r := range work.repos {
		x.ManualRetries = append(x.ManualRetries, work.currentManualRetries(r, active && s.options.CIPlanner != nil)...)
		x.Repositories = append(x.Repositories, cloneRepository(r))
		for _, pr := range r.PRs {
			if active && work.pendingPRChecks[pr.Target] && pr.State == "open" && pr.Evidence.Fresh() {
				x.Loading = true
			}
		}
	}
	slices.SortFunc(x.ManualRetries, func(a, b release.ManualRetryReport) int { return strings.Compare(a.Target.String(), b.Target.String()) })
	if active {
		s.syncRetestQueue(x.Authority, x.ManualRetries)
	}
	x.RetestQueue = slices.Clone(work.retestQueue)
	if !active {
		for i := range x.RetestQueue {
			x.RetestQueue[i].Sending = false
			x.RetestQueue[i].Blocked = "Activate this release to send retries."
		}
	}
	x.RetestLog = slices.Clone(work.retestLog)
	x.Loading = x.Loading || slices.ContainsFunc(x.ManualRetries, func(r release.ManualRetryReport) bool { return r.Pending })
	x.Plans = work.plansForDisplay()
	x.Inventory = work.inventory
	x.Inventory.Versions = maps.Clone(work.inventory.Versions)
	x.Inventory.Images = slices.Clone(work.inventory.Images)
	x.Inventory.ReleaseImages = slices.Clone(work.inventory.ReleaseImages)
	s.applyDashboard(&x, work, profileID)
	slices.SortFunc(x.Repositories, func(a, b release.Repository) int {
		if a.Source.Name < b.Source.Name {
			return -1
		}
		return 1
	})
	slices.SortFunc(x.Plans, func(a, b release.Plan) int {
		if a.Target.String() < b.Target.String() {
			return -1
		}
		return 1
	})
	x.Assessment = release.Assess(release.Input{Config: c, Choices: choices, Repositories: x.Repositories, Inventory: x.Inventory})
	x.PinningPreview = pinningPreview(x, s.dispatcher.PinningAvailable(), work.creatingPinningPR)
	if work.createdPinningPR.Number != 0 {
		x.CreatedPinningPR = work.createdPinningPR
	}
	x.CreatedPinningPR.Changes = slices.Clone(x.CreatedPinningPR.Changes)
	return x
}
func (s *Session) fillSharedSnapshot(x *Snapshot) {
	if s.options.GitHubReads != nil {
		x.StartupReadLimit, x.OngoingReadLimit = s.options.GitHubReads.ReadLimits()
		x.GitHubReadBudget = s.options.GitHubReads.Budget()
		x.GitHubReadStats = s.options.GitHubReads.Stats()
		x.ScheduledCIStats = s.scheduledCIStats(x.GitHubReadStats)
	}
	if planner, ok := s.planner.(interface{ ReadIssues() []konflux.ReadIssue }); ok {
		x.KonfluxReadIssues = planner.ReadIssues()
	}
	if s.konfluxHealth != nil {
		health := *s.konfluxHealth
		x.KonfluxHealth = &health
	}
	if s.options.Cache != nil {
		x.CacheAvailable = true
		x.CacheEnabled = s.options.Cache.Enabled()
	}
	if s.log != nil {
		x.LogError = s.log.Error()
		x.LogEntries = s.log.Recent()
	}
}

func cloneRepository(r release.Repository) release.Repository {
	r.Candidate.PRs = slices.Clone(r.Candidate.PRs)
	r.PRs = slices.Clone(r.PRs)
	for i := range r.PRs {
		r.PRs[i].Checks = slices.Clone(r.PRs[i].Checks)
	}
	r.Tags = slices.Clone(r.Tags)
	for i := range r.Tags {
		r.Tags[i].Contains = maps.Clone(r.Tags[i].Contains)
	}
	r.Pins.Versions = maps.Clone(r.Pins.Versions)
	r.Pins.Contents = maps.Clone(r.Pins.Contents)
	r.Pins.Paths = maps.Clone(r.Pins.Paths)
	r.Pins.Failures = maps.Clone(r.Pins.Failures)
	r.PreviousPins.Versions = maps.Clone(r.PreviousPins.Versions)
	r.PreviousPins.Contents = maps.Clone(r.PreviousPins.Contents)
	r.PreviousPins.Paths = maps.Clone(r.PreviousPins.Paths)
	r.PreviousPins.Failures = maps.Clone(r.PreviousPins.Failures)
	r.PinDiscovery.Files = maps.Clone(r.PinDiscovery.Files)
	r.PinningPins.Versions = maps.Clone(r.PinningPins.Versions)
	r.PinningPins.Contents = maps.Clone(r.PinningPins.Contents)
	r.PinningPins.Paths = maps.Clone(r.PinningPins.Paths)
	r.PinningPins.Failures = maps.Clone(r.PinningPins.Failures)
	if r.PinningPR != nil {
		p := *r.PinningPR
		p.Checks = slices.Clone(p.Checks)
		r.PinningPR = &p
	}
	return r
}

// Run keeps monitoring independently of browser connections and cancels old contexts.
func (s *Session) Run(ctx context.Context) {
	var background sync.WaitGroup
	defer background.Wait()
	if s.options.Profiles {
		stop := make(chan struct{})
		background.Go(func() { s.runDashboardWriter(stop) })
		defer close(stop)
	}
	if !s.options.Profiles && s.options.KonfluxProbe != nil {
		background.Go(func() { s.monitorKonfluxHealth(ctx) })
	}
	defer func() { s.dispatcher.Stop(); s.mu.Lock(); s.drain(); s.mu.Unlock() }()
	for {
		select {
		case <-s.changed:
		default:
		}
		s.mu.Lock()
		c := s.config
		c.Components = slices.Clone(c.Components)
		gen := s.generation
		valid := c.Validate() == nil
		child, cancel := context.WithCancel(ctx)
		s.monitorCancel = cancel
		s.mu.Unlock()
		var wg sync.WaitGroup
		if valid {
			wg.Go(func() { s.monitorRelease(apilog.WithRelease(child, c.Version), gen, c) })
			if s.options.Profiles && s.options.KonfluxProbe != nil {
				wg.Go(func() { s.monitorKonfluxHealth(child) })
			}
		}
		select {
		case <-ctx.Done():
		case <-s.changed:
		}
		cancel()
		wg.Wait()
		if ctx.Err() != nil {
			return
		}
	}
}

// Refresh performs an explicit full source cycle independently of browser navigation.
func (s *Session) Refresh(ctx context.Context, generation uint64, source release.Source, discover bool) error {
	return s.refresh(ctx, generation, source, discover, sourceWork{scope: ReadAll, plans: true, images: source.Name == "acs"})
}

func (s *Session) refresh(ctx context.Context, generation uint64, source release.Source, discover bool, work sourceWork) error {
	scoped := work.scope != ReadAll || work.scheduled
	ctx, cancel := context.WithTimeout(ctx, s.options.Timeout)
	ctx = konflux.WithReadCycle(ctx)
	var imageReads sync.WaitGroup
	defer func() {
		cancel()
		imageReads.Wait()
	}()
	s.mu.Lock()
	if generation != s.generation || (scoped && work.revision != s.workRevision) {
		s.mu.Unlock()
		return nil
	}
	c := s.config
	ctx = s.dispatcher.WithScope(apilog.WithRelease(ctx, c.Version))
	earlyComponentImages := work.images && !s.rcReady()
	request := DiscoveryRequest{Source: source, ConfirmedTag: s.choices.Tags[source.Name], Version: c.Version, Scope: work.scope}
	initial := discover && request.Reads(ReadPRs) && !s.initiallyLoaded[source.Name]
	if request.Reads(ReadCoverage) && !request.Reads(ReadTags) {
		old := s.repos[source.Name]
		for _, tag := range old.Tags {
			if tag.Name == request.ConfirmedTag && tag.SHA == s.confirmedSHAs[source.Name] && tag.Evidence.Fresh() {
				request.CoverageTag, request.CoverageHead = tag, old.Head
				break
			}
		}
	}
	if request.Reads(ReadPRs) {
		for _, p := range s.repos[source.Name].PRs {
			if p.State == "open" && p.Base == source.Branch {
				request.PreviouslyOpen = append(request.PreviouslyOpen, p.Target.PR)
			}
		}
		if saved := s.dashboards.Profiles[s.activeProfile]; sameDashboardConfig(saved.Config, c) {
			for _, pr := range saved.Repositories[source.Name].PRs {
				if pr.State == "open" && pr.Base == source.Branch && !slices.Contains(request.PreviouslyOpen, pr.Target.PR) {
					request.PreviouslyOpen = append(request.PreviouslyOpen, pr.Target.PR)
				}
			}
		}
	}
	if source.Name == "acs" {
		request.PinningPR = max(c.PinningPR, s.pendingPinningPR)
		request.PreviousCandidate = s.repos["acs"].Candidate
		if request.Reads(ReadPRs) {
			request.DetectPinningPR = c.PinningPR == 0
			request.ComparePinDiffs = s.dispatcher.PinningAvailable()
			if request.DetectPinningPR || request.ComparePinDiffs {
				request.PreviousPinFiles = s.previousPinFiles(s.repos["acs"])
			}
		}
	}
	if source.Name == "acs" && request.Reads(ReadRC) && scoped && s.duringStartup() && s.repos[source.Name].Candidate.Target.Tag == "" {
		request.CandidateProgress = func(head string, candidate release.Candidate) {
			s.mu.Lock()
			defer s.mu.Unlock()
			old := s.repos[source.Name]
			if generation == s.generation && work.revision == s.workRevision && ctx.Err() == nil && old.Candidate.Target.Tag == "" && (old.Head == "" || old.Head == head) {
				old.Source, old.Head, old.Candidate = source, head, candidate
				s.repos[source.Name] = old
				s.observeDashboardRepo(old, false, "")
				s.wakePolls()
			}
		}
	}
	tagStartHead := s.repos[source.Name].Head
	if work.tagInventory {
		request.ConfirmedTag = ""
		request.TagRefresh = work.tagRefresh
		if s.duringStartup() && s.choices.Tags[source.Name] == "" {
			request.Progress = func(partial release.Repository) {
				s.mu.Lock()
				defer s.mu.Unlock()
				if generation == s.generation && work.revision == s.workRevision && ctx.Err() == nil {
					s.mergeTagProgress(source, partial)
				}
			}
		}
	} else if work.scope == ReadTags {
		if saved := s.pendingTags[source.Name]; saved.Tag != "" {
			request.ConfirmedTag = saved.Tag
		}
	}
	revision := s.workRevision
	for t, choice := range s.choices.PRs {
		if t.PR > 0 && t.PR == request.PinningPR && t.Repo == source.Repo && source.Name == "acs" && request.Reads(ReadPins) {
			continue
		}
		if t.Repo == source.Repo {
			if choice == release.Ignored || choice == release.Excluded {
				request.Ignored = append(request.Ignored, t)
			} else if choice == release.Included || !scoped {
				request.Tracked = append(request.Tracked, t)
			}
		}
	}
	s.prioritizeNewPRs(request.Tracked)
	if initial {
		message := fmt.Sprintf("Checking %s on %s.", discoverySubject(request), consoleBranch(source))
		s.record(release.Target{Repo: source.Repo}, "", "github-start", "", message, "system", message)
	} else if discover && work.scope&ReadPRs != 0 {
		message := fmt.Sprintf("Checking GitHub for pull request updates on %s.", consoleBranch(source))
		s.record(release.Target{Repo: source.Repo}, "", "github-start", "", message, "system", message)
	}
	s.mu.Unlock()
	var imageErr, componentImageErr error
	if earlyComponentImages {
		imageReads.Go(func() {
			componentImageErr = s.refreshImagesScoped(ctx, generation, initial, false, scoped, false)
		})
	}
	var errs []error
	if discover {
		r, err := s.discovery.Discover(ctx, request)
		if scoped {
			switch {
			case request.Reads(ReadChecks):
				for _, p := range r.PRs {
					if p.ChecksEvidence.Error != "" {
						err = errors.Join(err, fmt.Errorf("%s: %s", p.Target, p.ChecksEvidence.Error))
					}
				}
			case err == nil && request.Reads(ReadRC) && r.Candidate.Evidence.Error != "":
				err = errors.New(r.Candidate.Evidence.Error)
			case request.Reads(ReadPRs) && r.Candidate.PRError != "":
				err = errors.Join(err, errors.New(r.Candidate.PRError))
			}
		}
		if err != nil && errors.Is(ctx.Err(), context.Canceled) {
			return fmt.Errorf("reading %s on %s: %w", discoverySubject(request), consoleBranch(source), ctx.Err())
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("reading %s on %s: %w", discoverySubject(request), consoleBranch(source), err))
		}
		s.mu.Lock()
		if generation != s.generation || (scoped && revision != s.workRevision) {
			s.mu.Unlock()
			return nil
		}
		message := ""
		if err != nil {
			message = err.Error()
		}
		operation := "github"
		switch {
		case work.tagInventory || work.scope == ReadTags:
			operation = "tags"
		case scoped && request.Reads(ReadRC) && !request.Reads(ReadPRs):
			operation = "acs-tags"
		case scoped && request.Reads(ReadChecks) && !request.Reads(ReadPRs):
			operation = "pr-checks"
		}
		s.recordReadResult(release.Target{Repo: source.Repo}, operation, message, discoverySubject(request)+" on "+consoleBranch(source))
		if initial {
			message := discoverySummary(request, r, err != nil || r.TagsPending)
			s.record(release.Target{Repo: source.Repo}, r.Head, "github-complete", "", message, "system", message)
		}
		old := s.repos[source.Name]
		if work.tagInventory && r.Head != "" && old.Head != tagStartHead && old.Head != r.Head {
			old.TagsPending = true
			s.repos[source.Name] = old
			s.mu.Unlock()
			return errors.Join(errs...)
		}
		if err != nil && r.Head == "" {
			if !scoped {
				r = retainDiscoveryFailure(old, r, err)
			} else {
				// A failed branch read establishes no change of head. Merge
				// only data requested in this poll with the last observation.
				r.Head = old.Head
				if r.Evidence.At.IsZero() {
					r.Evidence = release.Evidence{At: time.Now(), Error: err.Error()}
				}
				if request.Reads(ReadPins) {
					retained := retainDiscoveryFailure(old, r, err)
					r.Pins, r.PinningPR, r.PinningPins, r.PinningEvidence = retained.Pins, retained.PinningPR, retained.PinningPins, retained.PinningEvidence
				}
			}
		}
		if scoped {
			r = mergeDiscovery(old, r, request)
		}
		r.Source = source
		if request.DetectPinningPR && request.Reads(ReadPRs) {
			if err != nil && r.PinDiscovery.Evidence.At.IsZero() {
				r.PinDiscovery.Evidence = release.Evidence{At: time.Now(), Error: err.Error()}
			}
			s.logPinDiscovery(old.PinDiscovery, r.PinDiscovery)
		}
		if err != nil {
			if source.Name != "acs" && (work.tagInventory || !scoped) {
				r.TagsPending = true
			}
			for _, previous := range old.Tags {
				if !slices.ContainsFunc(r.Tags, func(current release.Tag) bool { return current.Name == previous.Name }) {
					previous.Evidence.Known = false
					previous.Evidence.Error = "last observed tag; current inventory incomplete"
					r.Tags = append(r.Tags, previous)
				}
			}
		}
		for _, p := range old.PRs {
			if slices.ContainsFunc(r.PRs, func(v release.PR) bool { return v.Target == p.Target }) {
				continue
			}
			// Keep ignored PRs that closed without merging available for restoration.
			if source.Name == "acs" && s.choices.PRs[p.Target] == release.Ignored && (p.State != "closed" || p.Merged) {
				continue
			}
			if p.State == "open" || slices.Contains(request.Tracked, p.Target) {
				p.Evidence.Known = false
				p.Evidence.Error = "not returned by latest discovery"
				p.ChecksEvidence.Known = false
				p.CheckResultsEvidence.Known = false
			}
			r.PRs = append(r.PRs, p)
		}
		if request.Reads(ReadPreviousPins) && source.Name == "acs" {
			s.recordReadResult(release.Target{Repo: source.Repo}, "previous-pins", r.PreviousPins.Evidence.Error, "component versions from the previous ACS release")
		}
		if !initial {
			s.recordDiscoveryChanges(old, r, request)
		}
		if source.Name == "acs" {
			if !r.Candidate.Evidence.Fresh() && old.Candidate.Target.Valid() && old.Candidate.SHA != "" {
				evidence := r.Candidate.Evidence
				if request.Reads(ReadRC) && err != nil && evidence.Error == "" {
					evidence.Error = err.Error()
				}
				r.Candidate = old.Candidate
				r.Candidate.Evidence = evidence
			}
			candidateChanged := old.Candidate.Target != r.Candidate.Target || old.Candidate.SHA != r.Candidate.SHA
			if candidateChanged {
				s.clearRCPlan()
				delete(s.tagChecks, source.Name)
				s.inventory.ReleaseImages = nil
				delete(s.inventory.Versions, "acs")
			}
			if candidateChanged || old.Candidate.Evidence.Fresh() != r.Candidate.Evidence.Fresh() {
				s.wakePolls()
			}
		}
		s.repos[source.Name] = r
		completeTags := !r.TagsPending && err == nil &&
			(request.Reads(ReadRC) && source.Name == "acs" ||
				request.Reads(ReadTags) && (work.tagInventory || !scoped))
		problem := ""
		if err != nil {
			problem = err.Error()
		}
		s.observeDashboardRepo(r, completeTags, problem)
		for _, p := range r.PRs {
			if request.Reads(ReadChecks) && p.State == "open" && s.choices.PRs[p.Target] == release.Included {
				var before checkActivity
				if index := slices.IndexFunc(old.PRs, func(previous release.PR) bool { return previous.Target == p.Target }); index >= 0 {
					before = prCheckActivity(old.PRs[index])
				}
				s.recordCheckChange(p.Target, p.SHA, before, prCheckActivity(p))
			}
			if plan, exists := s.plans[p.Target]; exists && (!p.Evidence.Fresh() || p.SHA != plan.SHA) {
				plan.StartFailures = staleStartFailures(plan.StartFailures, "Current PR identity does not confirm the commit for these earlier check observations.")
				s.plans[p.Target] = plan
			}
			s.dispatcher.ObserveIdentity(dispatch.Identity{Target: p.Target, SHA: p.SHA, Base: p.Base, Open: p.State == "open" && p.Base == source.Branch, OptOut: p.OptOut, CommentLabel: p.CommentLabel, Evidence: p.Evidence})
			if p.State != "open" {
				delete(s.pendingPRChecks, p.Target)
				if plan, exists := s.plans[p.Target]; exists {
					plan.Proposals = nil
					plan.Evidence.Known = false
					plan.Evidence.Error = "PR is no longer open; retained build observations"
					plan.StartFailures = staleStartFailures(plan.StartFailures, plan.Evidence.Error)
					s.plans[p.Target] = plan
					s.dispatcher.ObservePlan(plan)
				}
			}
		}
		if confirmed := s.choices.Tags[source.Name]; confirmed != "" {
			if !slices.ContainsFunc(r.Tags, func(tag release.Tag) bool { return tag.Name == confirmed }) {
				target := release.Target{Repo: source.Repo, Tag: confirmed}
				if plan, exists := s.plans[target]; exists {
					plan.StartFailures = staleStartFailures(plan.StartFailures, "Current tag identity is unavailable for these earlier check observations.")
					s.plans[target] = plan
				}
				s.dispatcher.ObserveIdentity(dispatch.Identity{Target: release.Target{Repo: source.Repo, Tag: confirmed}, SHA: s.confirmedSHAs[source.Name], Evidence: release.Evidence{At: time.Now(), Error: "confirmed tag was not returned by current discovery"}})
			}
			for _, tag := range r.Tags {
				if tag.Name == confirmed {
					t := release.Target{Repo: source.Repo, Tag: tag.Name}
					if plan, exists := s.plans[t]; exists && (!tag.Evidence.Fresh() || tag.SHA != plan.SHA) {
						plan.StartFailures = staleStartFailures(plan.StartFailures, "Current tag identity does not confirm the commit for these earlier check observations.")
						s.plans[t] = plan
					}
					s.dispatcher.ObserveIdentity(dispatch.Identity{Target: t, SHA: tag.SHA, Open: true, Evidence: tag.Evidence})
					if tag.SHA != "" && tag.SHA != s.confirmedSHAs[source.Name] {
						delete(s.choices.Tags, source.Name)
						delete(s.confirmedSHAs, source.Name)
						s.workRevision++
						select {
						case s.changed <- struct{}{}:
						default:
						}
						s.record(t, tag.SHA, "tag-invalidated", "", "resolved commit changed", "system")
					}
				}
			}
		}
		restored := s.verifySavedTag(source, r.Tags)
		pinRestored := source.Name == "acs" && request.Reads(ReadPins) && s.verifySavedPinningPR(request.PinningPR, r.PinningPR)
		if s.initializePinnedTags() || restored || pinRestored {
			select {
			case s.changed <- struct{}{}:
			default:
			}
		}
		s.revalidateRCPlan()
		if old.Head != r.Head || old.Evidence.Fresh() != r.Evidence.Fresh() {
			message := ""
			if !initial && old.Head != r.Head && r.Head != "" {
				message = fmt.Sprintf("Branch head of %s is %s.", consoleBranch(source), r.Head)
				if old.Head != "" {
					message = fmt.Sprintf("Branch head of %s changed from %s to %s.", consoleBranch(source), old.Head, r.Head)
				}
			}
			s.record(release.Target{Repo: source.Repo}, r.Head, "observation", "", fmt.Sprintf("current=%t", r.Evidence.Fresh()), "system", message)
		}
		s.drain()
		s.mu.Unlock()
	}
	s.mu.Lock()
	if generation != s.generation || (scoped && revision != s.workRevision) {
		s.mu.Unlock()
		return errors.Join(errs...)
	}
	r := s.repos[source.Name]
	var targets []release.Target
	for _, p := range r.PRs {
		if work.plans && s.choices.PRs[p.Target] != release.Ignored && s.choices.PRs[p.Target] != release.Excluded && (!scoped || s.choices.PRs[p.Target] == release.Included || p.Target == c.PinningTarget()) && p.State == "open" {
			targets = append(targets, p.Target)
		}
	}
	if tag := s.choices.Tags[source.Name]; work.plans && tag != "" {
		targets = append(targets, release.Target{Repo: source.Repo, Tag: tag})
	}
	rc := r.Candidate
	includeRC := source.Name == "acs" && s.rcReady()
	if work.plans && includeRC {
		targets = append(targets, rc.Target)
	}
	s.prioritizeNewPRs(targets)
	s.mu.Unlock()
	if work.images && (includeRC || !earlyComponentImages) {
		imageReads.Go(func() { imageErr = s.refreshImagesScoped(ctx, generation, initial, includeRC, true, false) })
	}
	for _, target := range targets {
		isRC := target == rc.Target
		s.mu.Lock()
		ignore := generation != s.generation || (scoped && revision != s.workRevision) || (s.choices.PRs[target] == release.Ignored || s.choices.PRs[target] == release.Excluded)
		if isRC && (revision != s.workRevision || !s.rcReady()) {
			ignore = true
		}
		if initial && !ignore {
			s.record(target, "", "checks-start", "", "Reading build observations.", "system", fmt.Sprintf("Checking build results in %s.", consoleTarget(target)))
		}
		s.mu.Unlock()
		if ignore {
			continue
		}
		readPlan := s.planKonflux
		if isRC && !work.scheduled {
			readPlan = s.readRCPlan
		}
		plan, err := readPlan(ctx, target)
		if target.PR > 0 && plan.SHA != "" {
			s.refreshChangedPRHead(ctx, generation, revision, source, target, plan.SHA)
		}
		if imageErr := s.inspectPinningImages(ctx, generation, &plan); imageErr != nil {
			errs = append(errs, fmt.Errorf("checking built images for %s: %w", consoleRevision(target, plan.SHA), imageErr))
		}
		if canceled(ctx) {
			return ctx.Err()
		}
		// Keep the normal cadence when an unsupported retag contract is the only error.
		if err != nil && (!isRC || work.scheduled) && (!work.scheduled || !konflux.IsUnsupportedRetagContract(err)) {
			errs = append(errs, fmt.Errorf("reading build results for %s: %w", consoleRevision(target, plan.SHA), err))
		}
		s.mu.Lock()
		if generation != s.generation || (scoped && revision != s.workRevision) || (s.choices.PRs[target] == release.Ignored || s.choices.PRs[target] == release.Excluded) {
			s.mu.Unlock()
			continue
		}
		message := ""
		if err != nil {
			message = plan.Evidence.Error
			if message == "" {
				message = err.Error()
			}
			if errors.Is(err, context.DeadlineExceeded) {
				message += " The request timed out."
			}
		}
		s.recordReadResult(target, "checks", message, "build results for "+consoleRevision(target, plan.SHA))
		if isRC {
			current := s.repos["acs"].Candidate
			if revision == s.workRevision && s.rcReady() && current.Target == target && current.SHA == rc.SHA {
				if plan.SHA != "" && plan.SHA != rc.SHA {
					plan = release.Plan{Target: target, Evidence: release.Evidence{
						At: plan.Evidence.At, Error: "The RC tag changed during this refresh.",
					}}
				}
				if err != nil && plan.Evidence.Error == "" {
					plan.Evidence.Error = message
				}
				s.observeRCPlan(target, plan)
				s.drain()
			}
			s.mu.Unlock()
			continue
		}
		expected := ""
		for _, p := range s.repos[source.Name].PRs {
			if p.Target == target && p.State == "open" {
				expected = p.SHA
			}
		}
		if target.Tag != "" {
			expected = s.confirmedSHAs[source.Name]
		}
		if err != nil && plan.SHA == "" {
			old := clonePlan(s.plans[target])
			delete(s.pendingPRChecks, target)
			old.Proposals = nil
			old.Evidence.Known = false
			old.Target = target
			old.Evidence.Error = message
			if old.Evidence.At.IsZero() {
				old.SHA, old.Evidence.At = expected, plan.Evidence.At
			}
			old.Evidence.AttemptAt = plan.Evidence.At
			old.StartFailures = staleStartFailures(old.StartFailures, message)
			for i := range old.Runs {
				if old.Runs[i].Image.Reference == "" && old.Runs[i].Image.Evidence.Error == "" {
					continue
				}
				old.Runs[i].Image.Evidence.Known = false
				old.Runs[i].Image.Evidence.Error = message
			}
			s.plans[target] = old
			s.observeDashboardPlan(old)
			s.dispatcher.ObservePlan(release.Plan{Target: target, SHA: expected})
		}
		if plan.SHA == expected && expected != "" {
			delete(s.pendingPRChecks, target)
			oldPlan := s.plans[target]
			retainVerifiedBuilds(oldPlan, &plan)
			if err != nil && len(plan.StartFailures) == 0 && oldPlan.SHA == plan.SHA {
				plan.StartFailures = staleStartFailures(oldPlan.StartFailures, message)
			}
			retainSupersededStartFailures(oldPlan, &plan)
			s.recordStartFailures(oldPlan, plan)
			for _, proposal := range plan.Proposals {
				if !slices.ContainsFunc(oldPlan.Proposals, func(old release.Proposal) bool { return dispatch.ID(old) == dispatch.ID(proposal) }) {
					if proposal.StartsMissingPRBuild() {
						s.record(target, proposal.SHA, "proposal", proposal.Payload, "missing build", "system", fmt.Sprintf("A start comment is available for %s in %s.", proposal.Job, consoleRevision(target, proposal.SHA)))
					} else {
						s.record(target, proposal.SHA, "proposal", proposal.Payload, "run "+proposal.RunID, "system", fmt.Sprintf("A retry is available for %s in %s (run %s).", proposal.Job, consoleRevision(target, proposal.SHA), proposal.RunID))
					}
				}
			}
			if oldPlan.Action != plan.Action || oldPlan.Evidence.Fresh() != plan.Evidence.Fresh() {
				summary := plan.Message
				if summary == "" {
					switch plan.Action {
					case "None":
						summary = "No build retries are needed."
					case "Wait":
						summary = "Waiting for builds or their required inputs."
					case "Retest":
						summary = "Build retries are available."
					case "Investigate missing images":
						summary = "Build images are missing and need investigation."
					default:
						if plan.Action != "" {
							summary = "Retry recommendation: " + plan.Action + "."
						}
					}
				}
				s.record(target, plan.SHA, "checks", "", plan.Action, "system", strings.TrimSpace(fmt.Sprintf("Checked build results in %s. %s", consoleRevision(target, plan.SHA), summary)))
			}
			s.plans[target] = plan
			s.observeDashboardPlan(plan)
			s.dispatcher.ObservePlan(plan)
		}
		s.drain()
		s.mu.Unlock()
	}
	imageReads.Wait()
	if imageErr = errors.Join(componentImageErr, imageErr); imageErr != nil {
		errs = append(errs, fmt.Errorf("checking container images for release %s: %w", c.Version, imageErr))
	}

	// Automatic dispatch revalidates the proposal inside Send.
	s.mu.Lock()
	currentGeneration := s.generation == generation && (!scoped || revision == s.workRevision)
	s.mu.Unlock()
	if work.plans && currentGeneration && !canceled(ctx) && !s.dispatcher.IsInteractive() {
		for _, view := range s.dispatcher.Views() {
			if view.Target.Repo != source.Repo {
				continue
			}
			for _, offer := range view.Offers {
				if offer.CanApprove {
					_, err := s.dispatcher.Send(ctx, view.Target, offer.ID, false)
					if err != nil {
						errs = append(errs, fmt.Errorf("posting a retry comment for %s: %w", consoleRevision(view.Target, view.SHA), err))
					}
					break
				}
			}
		}
	}
	if work.plans && !work.scheduled {
		s.refreshManualRetries(ctx, generation, revision, source)
	}
	s.mu.Lock()
	if generation == s.generation && discover && request.Reads(ReadPRs) {
		s.initiallyLoaded[source.Name] = true
	}
	s.drain()
	s.mu.Unlock()
	return errors.Join(errs...)
}

func clonePlan(p release.Plan) release.Plan {
	p.Runs = slices.Clone(p.Runs)
	p.Proposals = slices.Clone(p.Proposals)
	p.Warnings = slices.Clone(p.Warnings)
	p.Advisories = slices.Clone(p.Advisories)
	p.StartFailures = slices.Clone(p.StartFailures)
	p.Validation.Checks = slices.Clone(p.Validation.Checks)
	return p
}

// unverifiedImages retains earlier values and timestamps without asserting current availability.
func unverifiedImages(previous []release.Image, reason string) []release.Image {
	images := slices.Clone(previous)
	for index := range images {
		images[index].Evidence.Known = false
		images[index].Evidence.Error = reason
		images[index].PublishingSince = time.Time{}
	}
	return images
}

// retainLastImagePresence preserves history without making a failed refresh current.
func retainLastImagePresence(current, previous []release.Image) []release.Image {
	images := slices.Clone(current)
	for index, image := range images {
		if image.Reference == "" || image.Evidence.Fresh() || image.Present != "" {
			continue
		}
		for _, old := range previous {
			if old.Reference == image.Reference && old.Component == image.Component && old.Family == image.Family && old.Present != "" {
				images[index].Present = old.Present
				images[index].Evidence.At = old.Evidence.At
				break
			}
		}
	}
	return images
}

// trackImagePublishing times verified absence once there is evidence that outputs are expected.
func trackImagePublishing(current, previous []release.Image, publishing func(release.Image) bool) {
	for i, image := range current {
		if !image.Evidence.Fresh() || image.Present != release.No || !publishing(image) {
			continue
		}
		current[i].PublishingSince = image.Evidence.At
		for _, old := range previous {
			if old.Reference == image.Reference && old.Component == image.Component && old.Family == image.Family &&
				old.Name == image.Name && !old.PublishingSince.IsZero() {
				current[i].PublishingSince = old.PublishingSince
				break
			}
		}
	}
}

// canceled reports whether shutdown or a superseding cycle ended the work, not a read failure.
func canceled(ctx context.Context) bool { return errors.Is(ctx.Err(), context.Canceled) }

// refreshImages commits registry evidence only while its release and tag choices remain current.
func (s *Session) refreshImages(ctx context.Context, generation uint64, initial bool) error {
	return s.refreshImagesScoped(ctx, generation, initial, true, false, false)
}

func (s *Session) refreshImagesScoped(ctx context.Context, generation uint64, initial, includeRC, scheduled, force bool) error {
	s.mu.Lock()
	if generation != s.generation {
		s.mu.Unlock()
		return nil
	}
	if scheduled && includeRC && !s.rcReady() {
		s.mu.Unlock()
		return nil
	}
	revision := s.workRevision
	c := s.config
	versions := maps.Clone(s.choices.Tags)
	rc := s.repos["acs"].Candidate
	includeRC = includeRC && rc.Target.Valid()
	work := s.releaseWork
	work.imageReads++
	defer func() {
		s.mu.Lock()
		work.imageReads--
		s.mu.Unlock()
	}()
	if initial {
		s.record(release.Target{Repo: c.ACS().Repo}, "", "images-start", "", "Checking registry images using saved naming patterns.", "system", fmt.Sprintf("Checking container images for release %s.", c.Version))
	}
	s.mu.Unlock()
	var inventory release.Inventory
	var err error
	tags := maps.Clone(versions)
	if includeRC {
		tags["acs"] = rc.Target.Tag
	}
	publish := func(inventory release.Inventory, err error, complete bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		current := s.repos["acs"].Candidate
		sameCandidate := rc.Target == current.Target && rc.SHA == current.SHA
		if canceled(ctx) || generation != s.generation || !maps.Equal(versions, s.choices.Tags) ||
			(scheduled && revision != s.workRevision) || (includeRC && (!sameCandidate || !s.rcReady())) {
			return
		}
		s.publishImageInventory(inventory, versions, includeRC, err, complete)
	}
	if inspector, ok := s.images.(ProgressiveInspector); ok {
		inventory, err = inspector.InspectImages(ctx, c, tags, force, func(partial release.Inventory) {
			publish(partial, nil, false)
		})
	} else if includeRC {
		inventory, err = s.images.Inspect(ctx, c, tags)
	} else if components, ok := s.images.(ComponentInspector); ok {
		inventory, err = components.InspectComponents(ctx, c, versions)
	} else {
		inventory, err = s.images.Inspect(ctx, c, versions)
	}
	if canceled(ctx) {
		return ctx.Err()
	}
	publish(inventory, err, true)
	return err
}

// publishImageInventory applies a batch or partial results under the session lock.
func (s *Session) publishImageInventory(inventory release.Inventory, versions map[string]string, includeRC bool, err error, complete bool) {
	c := s.config
	rc := s.repos["acs"].Candidate
	s.trackImageDigestChanges(inventory.Images)
	s.observeDashboardImages(inventory, includeRC, complete, err)
	if complete {
		message := ""
		if err != nil {
			message = err.Error()
		}
		s.recordReadResult(release.Target{Repo: c.ACS().Repo}, "images", message, "container images for release "+c.Version)
	}
	if err != nil && len(inventory.Images) == 0 && len(s.inventory.Images) > 0 {
		reason := inventory.Evidence.Error
		if reason == "" {
			reason = "The image check failed before returning new results; earlier results are unverified."
		}
		inventory.Images = unverifiedImages(s.inventory.Images, reason)
		inventory.ReleaseVersion = s.inventory.ReleaseVersion
		if inventory.Versions == nil {
			inventory.Versions = make(map[string]string)
		}
		for component, version := range s.inventory.Versions {
			if component != "acs" {
				inventory.Versions[component] = version
			}
		}
		inventory.Evidence.Known = false
		inventory.Evidence.Error = reason
	}
	if !includeRC {
		inventory.ReleaseImages = s.inventory.ReleaseImages
		if len(inventory.ReleaseImages) > 0 {
			if inventory.Versions == nil {
				inventory.Versions = map[string]string{}
			}
			inventory.Versions["acs"] = s.inventory.Versions["acs"]
		}
	} else if err != nil && len(inventory.ReleaseImages) == 0 {
		inventory.ReleaseImages = unverifiedImages(s.inventory.ReleaseImages, "The ACS RC image check returned no new results; earlier results are unverified.")
		if len(inventory.ReleaseImages) > 0 {
			if inventory.Versions == nil {
				inventory.Versions = map[string]string{}
			}
			inventory.Versions["acs"] = strings.TrimPrefix(rc.Target.Tag, "v")
		}
	}
	state := s.options.ReleaseState
	if s.activeProfile != "" {
		state = nil
	}
	state.restorePendingImages(inventory.Images, s.inventory.Images)
	state.restorePendingImages(inventory.ReleaseImages, s.inventory.ReleaseImages)
	inventory.Images = retainLastImagePresence(inventory.Images, s.inventory.Images)
	inventory.ReleaseImages = retainLastImagePresence(inventory.ReleaseImages, s.inventory.ReleaseImages)
	trackImagePublishing(inventory.Images, s.inventory.Images, func(image release.Image) bool {
		for _, source := range c.Components {
			tag := versions[source.Name]
			if source.Name != image.Component || tag == "" {
				continue
			}
			plan := s.plans[release.Target{Repo: source.Repo, Tag: tag}]
			result, _ := plan.BuildStatus()
			return result == release.Yes && plan.SHA != "" && plan.SHA == s.confirmedSHAs[source.Name]
		}
		return false
	})
	rcPublishing := slices.ContainsFunc(inventory.ReleaseImages, func(image release.Image) bool {
		return image.Evidence.Fresh() && image.Present == release.Yes
	})
	trackImagePublishing(inventory.ReleaseImages, s.inventory.ReleaseImages, func(release.Image) bool {
		return rcPublishing
	})
	previousImages := slices.Concat(s.inventory.Images, s.inventory.ReleaseImages)
	for _, current := range slices.Concat(inventory.Images, inventory.ReleaseImages) {
		oldPresent := release.Truth("")
		oldFresh := false
		for _, old := range previousImages {
			if old.Reference == current.Reference && old.Component == current.Component && old.Family == current.Family && old.Name == current.Name {
				oldPresent = old.Present
				oldFresh = old.Evidence.Fresh()
				break
			}
		}
		if oldPresent != current.Present || oldFresh != current.Evidence.Fresh() {
			reference := current.Reference
			if reference == "" {
				reference = current.Component + "/" + current.Name
			}
			message := fmt.Sprintf("Could not confirm whether image %s is available.", reference)
			if current.Evidence.Fresh() {
				switch current.Present {
				case release.Yes:
					message = fmt.Sprintf("Image %s is available.", current.Reference)
				case release.No:
					message = fmt.Sprintf("Image %s was not found.", current.Reference)
				}
			}
			s.record(release.Target{Repo: c.ACS().Repo}, "", "image", current.Reference, string(current.Present), "system", message)
		}
	}
	s.inventory = inventory
	if complete && s.activeProfile == "" {
		err := s.options.ReleaseState.saveImageObservations(inventory)
		message := ""
		if err != nil {
			message = err.Error()
		}
		s.recordReadResult(release.Target{Repo: c.ACS().Repo}, "image-state", message, "saved image observations")
	}
}
