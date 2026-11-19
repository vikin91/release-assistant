package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"release-assistant/internal/dispatch"
	"release-assistant/internal/konflux"
	"release-assistant/internal/release"
)

type (
	// Options controls polling and bounds each source's work.
	Options struct {
		Poll, Discovery, BackoffCap, Timeout time.Duration
		Initial                              release.Config
		Cache                                DiscoveryCache
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
		Config                       release.Config
		Choices                      release.Choices
		Repositories                 []release.Repository
		Inventory                    release.Inventory
		Plans                        []release.Plan
		Assessment                   release.Assessment
		Authority                    []dispatch.View
		Interactive                  bool
		CacheAvailable, CacheEnabled bool
		// WritesEnabled reports startup permission; target grants remain independent.
		WritesEnabled   bool
		Error, LogError string
		Generation      uint64
		History         []Cycle
		LogEntries      []Record
		// KonfluxReadIssues reports endpoint reachability independently of release readiness.
		KonfluxReadIssues []konflux.ReadIssue
		// NextSourceCheck records scheduled monitoring times by repository.
		NextSourceCheck map[string]time.Time
		// Loading remains true until every source completes its first refresh attempt.
		Loading bool
	}
	// Action is a typed browser or CLI session decision.
	Action struct {
		Kind                    string
		Config                  release.Config
		Target                  release.Target
		SHA, ID, Component, Tag string
		Choice                  release.Choice
		Enabled                 bool
		// Nil settings are omitted from the submitted form.
		Interactive, CacheEnabled *bool
		Generation                uint64
	}
	// Session owns release observations. Dispatcher alone owns write grants.
	Session struct {
		mu              sync.Mutex
		config          release.Config
		choices         release.Choices
		repos           map[string]release.Repository
		plans           map[release.Target]release.Plan
		inventory       release.Inventory
		confirmedSHAs   map[string]string
		tagInitialized  map[string]bool
		nextSourceCheck map[string]time.Time
		initiallyLoaded map[string]bool
		priorPinChoice  release.Choice
		workRevision    uint64
		readErrors      map[string]string
		generation      uint64
		errorText       string
		discovery       Discoverer
		planner         Planner
		images          Inspector
		dispatcher      *dispatch.Dispatcher
		log             *Log
		options         Options
		changed         chan struct{}
		history         []Cycle
	}
)

// New constructs an unconfigured session with safe restart defaults.
func New(d Discoverer, p Planner, i Inspector, dispatcher *dispatch.Dispatcher, log *Log, o Options) *Session {
	if o.Poll <= 0 {
		o.Poll = time.Minute
	}
	if o.Discovery <= 0 {
		o.Discovery = 5 * time.Minute
	}
	if o.BackoffCap <= 0 {
		o.BackoffCap = 5 * time.Minute
	}
	if o.Timeout <= 0 {
		o.Timeout = 45 * time.Second
	}
	s := &Session{discovery: d, planner: p, images: i, dispatcher: dispatcher, log: log, options: o, changed: make(chan struct{}, 1)}
	s.reset()
	s.config = o.Initial
	s.config.Components = slices.Clone(o.Initial.Components)
	if o.Initial.Version != "" {
		if err := o.Initial.Validate(); err != nil {
			s.errorText = err.Error()
		} else {
			s.includePinningPR()
		}
	}
	return s
}
func (s *Session) reset() {
	s.choices = release.Choices{PRs: map[release.Target]release.Choice{}, Tags: map[string]string{}, Acknowledged: map[string]bool{}}
	s.repos = map[string]release.Repository{}
	s.plans = map[release.Target]release.Plan{}
	s.confirmedSHAs = map[string]string{}
	s.tagInitialized = map[string]bool{}
	s.nextSourceCheck = map[string]time.Time{}
	s.initiallyLoaded = map[string]bool{}
	s.workRevision++
	s.readErrors = map[string]string{}
	s.inventory = release.Inventory{}
	s.history = nil
	s.generation++
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
		s.record(e.Target, e.SHA, e.Decision, e.Payload, e.Result, e.Origin)
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
	if a.Kind == "use-pinning-pr" {
		return s.confirmPinningPR(ctx, a)
	}
	s.mu.Lock()
	if a.Kind != "configure" && a.Generation != s.generation {
		s.mu.Unlock()
		return fmt.Errorf("page belongs to an earlier configuration; reload")
	}
	if a.Kind == "send" {
		rc := s.repos["acs"].Candidate
		if a.Target.Tag != "" && a.Target == rc.Target && (!s.rcRetriesReady() || s.plans[a.Target].SHA != rc.SHA) {
			s.mu.Unlock()
			return fmt.Errorf("release candidate prerequisites or build checks are no longer verified")
		}
		s.mu.Unlock()
		_, err := s.dispatcher.Send(ctx, a.Target, a.ID, true)
		s.mu.Lock()
		s.drain()
		s.mu.Unlock()
		return err
	}
	defer s.mu.Unlock()
	var err error
	switch a.Kind {
	case "configure":
		if err = a.Config.Validate(); err != nil {
			return err
		}
		sameContext := s.config.Version == a.Config.Version && s.config.Branch == a.Config.Branch && slices.Equal(s.config.Components, a.Config.Components)
		pinChanged := !sameContext || s.config.PinningPR != a.Config.PinningPR
		if sameContext {
			if pinChanged {
				s.clearPinningSelection()
			}
			s.generation++
		} else {
			s.dispatcher.Reset()
			s.reset()
		}
		s.config = a.Config
		if pinChanged {
			s.includePinningPR()
		}
		s.config.Components = slices.Clone(a.Config.Components)
		s.nextSourceCheck = map[string]time.Time{}
		s.initiallyLoaded = map[string]bool{}
		s.workRevision++
		s.errorText = ""
		s.record(release.Target{}, "", "configure", "", "new selection context", "user")
		select {
		case s.changed <- struct{}{}:
		default:
		}
	case "choice", "ignore-component-prs":
		if a.Kind == "choice" {
			if a.Choice != "" && a.Choice != release.Included && a.Choice != release.Excluded && a.Choice != release.Ignored {
				return fmt.Errorf("invalid inclusion choice")
			}
			if a.Target.PR > 0 && a.Target == s.config.PinningTarget() {
				return fmt.Errorf("the confirmed pinning PR is always included")
			}
			if !s.hasTarget(a.Target) {
				return fmt.Errorf("unknown target")
			}
			s.setPRChoice(a.Target, a.SHA, a.Choice)
		} else if !s.ignoreComponentPRs() {
			return nil
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
		if a.CacheEnabled != nil && s.options.Cache == nil {
			return fmt.Errorf("discovery cache is unavailable")
		}
		if a.Interactive != nil {
			if !s.dispatcher.WritesEnabled() || s.config.Validate() != nil {
				return fmt.Errorf("confirmation mode is unavailable")
			}
			if s.dispatcher.IsInteractive() != *a.Interactive {
				s.dispatcher.Interactive(*a.Interactive)
			}
		}
		if a.CacheEnabled != nil {
			s.options.Cache.SetEnabled(*a.CacheEnabled)
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
			if tag.Name == a.Tag && tag.SHA == a.SHA && tag.Belongs == release.Yes && tag.Evidence.Fresh() {
				found = &tag
				break
			}
		}
		if found == nil {
			return fmt.Errorf("tag evidence changed or eligibility unknown; reload")
		}
		if old := s.choices.Tags[a.Component]; old != "" && old != a.Tag {
			s.archiveTag(a.Component, r.Source.Repo)
		}
		s.workRevision++
		s.choices.Tags[a.Component] = a.Tag
		s.confirmedSHAs[a.Component] = a.SHA
		s.tagInitialized[a.Component] = true
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
		s.archiveTag(a.Component, source.Repo)
		delete(s.choices.Tags, a.Component)
		delete(s.confirmedSHAs, a.Component)
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
	case "reject":
		err = s.dispatcher.Reject(a.Target, a.ID, a.Enabled)
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
}

func (s *Session) setPRChoice(target release.Target, sha string, choice release.Choice) {
	s.choices.PRs[target] = choice
	ignored := choice == release.Ignored || choice == release.Excluded
	s.dispatcher.Ignore(target, ignored)
	if ignored {
		delete(s.plans, target)
	}
	s.record(target, sha, "inclusion", "", string(choice), "user")
}

func (s *Session) ignoreComponentPRs() bool {
	changed := false
	pinning := s.config.PinningTarget()
	for _, source := range s.config.Components {
		for _, pr := range s.repos[source.Name].PRs {
			if pr.Target.Repo != source.Repo || pr.Target == pinning || !pr.Selectable(s.choices.PRs[pr.Target]) {
				continue
			}
			s.setPRChoice(pr.Target, pr.SHA, release.Ignored)
			changed = true
		}
	}
	return changed
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
	c := s.config
	c.Components = slices.Clone(c.Components)
	choices := s.choices
	choices.PRs = maps.Clone(choices.PRs)
	choices.Tags = maps.Clone(choices.Tags)
	choices.Acknowledged = maps.Clone(choices.Acknowledged)
	x := Snapshot{
		Config: c, Choices: choices, Error: s.errorText, Generation: s.generation,
		Interactive: s.dispatcher.IsInteractive(), WritesEnabled: s.dispatcher.WritesEnabled(),
		Authority: s.dispatcher.Views(), NextSourceCheck: maps.Clone(s.nextSourceCheck),
	}
	if planner, ok := s.planner.(interface{ ReadIssues() []konflux.ReadIssue }); ok {
		x.KonfluxReadIssues = planner.ReadIssues()
	}
	if s.options.Cache != nil {
		x.CacheAvailable = true
		x.CacheEnabled = s.options.Cache.Enabled()
	}
	if c.Validate() == nil {
		for _, source := range append([]release.Source{c.ACS()}, c.Components...) {
			if !s.initiallyLoaded[source.Name] {
				x.Loading = true
				break
			}
		}
	}
	for _, cycle := range s.history {
		cycle.Images = slices.Clone(cycle.Images)
		cycle.Plan = clonePlan(cycle.Plan)
		x.History = append(x.History, cycle)
	}
	for _, r := range s.repos {
		x.Repositories = append(x.Repositories, cloneRepository(r))
	}
	slices.SortFunc(x.Repositories, func(a, b release.Repository) int {
		if a.Source.Name < b.Source.Name {
			return -1
		}
		return 1
	})
	for _, p := range s.plans {
		x.Plans = append(x.Plans, clonePlan(p))
	}
	slices.SortFunc(x.Plans, func(a, b release.Plan) int {
		if a.Target.String() < b.Target.String() {
			return -1
		}
		return 1
	})
	x.Inventory = s.inventory
	x.Inventory.Versions = maps.Clone(s.inventory.Versions)
	x.Inventory.Images = slices.Clone(s.inventory.Images)
	x.Inventory.ReleaseImages = slices.Clone(s.inventory.ReleaseImages)
	if s.log != nil {
		x.LogError = s.log.Error()
		x.LogEntries = s.log.Recent()
	}
	x.Assessment = release.Assess(release.Input{Config: c, Choices: choices, Repositories: x.Repositories, Inventory: x.Inventory})
	return x
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
	r.Pins.Paths = maps.Clone(r.Pins.Paths)
	r.Pins.Failures = maps.Clone(r.Pins.Failures)
	r.PreviousPins.Versions = maps.Clone(r.PreviousPins.Versions)
	r.PreviousPins.Paths = maps.Clone(r.PreviousPins.Paths)
	r.PreviousPins.Failures = maps.Clone(r.PreviousPins.Failures)
	r.PinDiscovery.Files = maps.Clone(r.PinDiscovery.Files)
	r.PinningPins.Versions = maps.Clone(r.PinningPins.Versions)
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
		s.mu.Unlock()
		child, cancel := context.WithCancel(ctx)
		var wg sync.WaitGroup
		if valid {
			wg.Go(func() { s.monitorRelease(child, gen, c) })
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

func (s *Session) monitor(ctx context.Context, generation uint64, source release.Source) {
	var discoverAt time.Time
	var delay time.Duration
	failures := 0
	for {
		previousDelay := delay
		s.mu.Lock()
		if generation == s.generation {
			delete(s.nextSourceCheck, source.Repo)
		}
		s.mu.Unlock()
		err := s.refreshPlanned(ctx, generation, source, !time.Now().Before(discoverAt))
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			failures++
			delay = min(s.options.Poll*time.Duration(1<<min(failures, 12)), s.options.BackoffCap)
		} else {
			failures = 0
			delay = s.options.Poll
		}
		if !time.Now().Before(discoverAt) {
			discoverAt = time.Now().Add(s.options.Discovery)
		}
		if err != nil && (failures == 1 || delay != previousDelay) {
			s.mu.Lock()
			if generation == s.generation {
				message := fmt.Sprintf("The last monitoring attempt failed while %s. The next monitoring attempt for %s is in %s.", err, consoleBranch(source), retryDelay(delay))
				s.record(release.Target{Repo: source.Repo}, "", "retry-wait", "", fmt.Sprintf("Next check attempt in %s; next GitHub discovery at %s.", delay, discoverAt.Format(time.RFC3339)), "system", message)
			}
			s.mu.Unlock()
		}
		next := time.Now().Add(delay)
		s.mu.Lock()
		if generation == s.generation {
			s.nextSourceCheck[source.Repo] = next
		}
		s.mu.Unlock()
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// Refresh performs an explicit full source cycle independently of browser navigation.
func (s *Session) Refresh(ctx context.Context, generation uint64, source release.Source, discover bool) error {
	return s.refresh(ctx, generation, source, discover, sourceWork{scope: ReadAll, plans: true, images: source.Name == "acs"})
}

func (s *Session) refresh(ctx context.Context, generation uint64, source release.Source, discover bool, work sourceWork) error {
	ctx, cancel := context.WithTimeout(ctx, s.options.Timeout)
	ctx = konflux.WithReadCycle(ctx)
	var imageReads sync.WaitGroup
	defer func() {
		cancel()
		imageReads.Wait()
	}()
	s.mu.Lock()
	if generation != s.generation || (work.scope != ReadAll && work.revision != s.workRevision) {
		s.mu.Unlock()
		return nil
	}
	c := s.config
	earlyComponentImages := work.images && !s.rcReady()
	initial := discover && !s.initiallyLoaded[source.Name]
	request := DiscoveryRequest{Source: source, ConfirmedTag: s.choices.Tags[source.Name], PinningPR: c.PinningPR, Version: c.Version, Scope: work.scope}
	if source.Name == "acs" && c.PinningPR == 0 {
		request.DetectPinningPR = true
		request.PreviousPinFiles = s.repos["acs"].PinDiscovery.Files
	}
	if work.tagInventory {
		request.ConfirmedTag = ""
	}
	revision := s.workRevision
	for t, choice := range s.choices.PRs {
		if t == c.PinningTarget() && source.Name == "acs" && request.Reads(ReadPins) {
			continue
		}
		if t.Repo == source.Repo {
			if choice == release.Ignored || choice == release.Excluded {
				request.Ignored = append(request.Ignored, t)
			} else if choice == release.Included || work.scope == ReadAll {
				request.Tracked = append(request.Tracked, t)
			}
		}
	}
	if initial {
		message := fmt.Sprintf("Checking %s on %s.", discoverySubject(request), consoleBranch(source))
		s.record(release.Target{Repo: source.Repo}, "", "github-start", "", "Reading GitHub branch "+source.Branch+", PRs, and tags.", "system", message)
	} else if discover && work.scope&ReadPRs != 0 {
		message := fmt.Sprintf("Checking GitHub for pull request updates on %s.", consoleBranch(source))
		s.record(release.Target{Repo: source.Repo}, "", "github-start", "", message, "system", message)
	}
	s.mu.Unlock()
	var imageErr, componentImageErr error
	if earlyComponentImages {
		imageReads.Go(func() {
			componentImageErr = s.refreshImagesScoped(ctx, generation, initial, false, work.scope != ReadAll)
		})
	}
	var errs []error
	if discover {
		r, err := s.discovery.Discover(ctx, request)
		if err != nil && errors.Is(ctx.Err(), context.Canceled) {
			return fmt.Errorf("reading %s on %s: %w", discoverySubject(request), consoleBranch(source), ctx.Err())
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("reading %s on %s: %w", discoverySubject(request), consoleBranch(source), err))
		}
		s.mu.Lock()
		if generation != s.generation || (work.scope != ReadAll && revision != s.workRevision) {
			s.mu.Unlock()
			return nil
		}
		message := ""
		if err != nil {
			message = err.Error()
		}
		operation := "github"
		if work.tagInventory {
			operation = "tags"
		}
		s.recordReadResult(release.Target{Repo: source.Repo}, operation, message, discoverySubject(request)+" on "+consoleBranch(source))
		if initial {
			message := discoverySummary(request, r, err != nil || r.TagsPending)
			s.record(release.Target{Repo: source.Repo}, r.Head, "github-complete", "", fmt.Sprintf("GitHub discovery returned %d PRs and %d tags; complete=%t.", len(r.PRs), len(r.Tags), err == nil && !r.TagsPending), "system", message)
		}
		old := s.repos[source.Name]
		if work.scope != ReadAll {
			r = mergeDiscovery(old, r, request)
		}
		if err != nil && r.Head == "" {
			if work.tagInventory {
				r = cloneRepository(old)
				r.TagsPending = true
			} else {
				r = retainDiscoveryFailure(old, r, err)
			}
		}
		r.Source = source
		if request.DetectPinningPR && request.Reads(ReadPRs) {
			if err != nil && r.PinDiscovery.Evidence.At.IsZero() {
				r.PinDiscovery.Evidence = release.Evidence{At: time.Now(), Error: err.Error()}
			}
			s.logPinDiscovery(old.PinDiscovery, r.PinDiscovery)
		}
		if err != nil {
			if source.Name != "acs" && (work.tagInventory || work.scope == ReadAll) {
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
			if !slices.ContainsFunc(r.PRs, func(v release.PR) bool { return v.Target == p.Target }) {
				if source.Name != "acs" || s.choices.PRs[p.Target] != release.Ignored {
					p.Evidence.Known = false
					p.Evidence.Error = "not returned by latest discovery"
					r.PRs = append(r.PRs, p)
				}
			}
		}
		if request.Reads(ReadPreviousPins) && source.Name == "acs" {
			s.recordReadResult(release.Target{Repo: source.Repo}, "previous-pins", r.PreviousPins.Evidence.Error, "component versions from the previous ACS release")
		}
		if !initial {
			s.recordDiscoveryChanges(old, r, request)
		}
		if source.Name == "acs" && (old.Candidate.Target != r.Candidate.Target || old.Candidate.SHA != r.Candidate.SHA) {
			s.clearRCPlan()
			s.inventory.ReleaseImages = nil
			delete(s.inventory.Versions, "acs")
		}
		s.repos[source.Name] = r
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
			s.dispatcher.ObserveIdentity(dispatch.Identity{Target: p.Target, SHA: p.SHA, Base: p.Base, Open: p.State == "open" && p.Base == source.Branch, OptOut: p.OptOut, Evidence: p.Evidence})
			if p.State != "open" {
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
		if s.initializePinnedTags() {
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
	if generation != s.generation || (work.scope != ReadAll && revision != s.workRevision) {
		s.mu.Unlock()
		return errors.Join(errs...)
	}
	r := s.repos[source.Name]
	var targets []release.Target
	for _, p := range r.PRs {
		if work.plans && s.choices.PRs[p.Target] != release.Ignored && s.choices.PRs[p.Target] != release.Excluded && (work.scope == ReadAll || s.choices.PRs[p.Target] == release.Included || p.Target == c.PinningTarget()) && p.State == "open" {
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
	s.mu.Unlock()
	if work.images && (includeRC || !earlyComponentImages) {
		imageReads.Go(func() { imageErr = s.refreshImagesScoped(ctx, generation, initial, includeRC, true) })
	}
	for _, target := range targets {
		isRC := target == rc.Target
		s.mu.Lock()
		ignore := generation != s.generation || (work.scope != ReadAll && revision != s.workRevision) || (s.choices.PRs[target] == release.Ignored || s.choices.PRs[target] == release.Excluded)
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
		readPlan := s.planner.Plan
		if isRC {
			readPlan = s.readRCPlan
		}
		plan, err := readPlan(ctx, target)
		if imageErr := s.inspectPinningImages(ctx, generation, &plan); imageErr != nil {
			errs = append(errs, fmt.Errorf("checking built images for %s: %w", consoleRevision(target, plan.SHA), imageErr))
		}
		if canceled(ctx) {
			return ctx.Err()
		}
		if err != nil && !isRC {
			errs = append(errs, fmt.Errorf("reading build results for %s: %w", consoleRevision(target, plan.SHA), err))
		}
		s.mu.Lock()
		if generation != s.generation || (work.scope != ReadAll && revision != s.workRevision) || (s.choices.PRs[target] == release.Ignored || s.choices.PRs[target] == release.Excluded) {
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
			if p.Target == target {
				expected = p.SHA
			}
		}
		if target.Tag != "" {
			expected = s.confirmedSHAs[source.Name]
		}
		if plan.SHA != "" && plan.SHA != expected && target.PR > 0 {
			// The planner resolved a new head; withhold authority until labels refresh.
			identity := dispatch.Identity{Target: target, SHA: plan.SHA, Open: true, Evidence: release.Evidence{At: plan.Evidence.At, Error: "head changed; fresh authorization required"}}
			s.dispatcher.ObserveIdentity(identity)
			repo := s.repos[source.Name]
			for n := range repo.PRs {
				if repo.PRs[n].Target == target {
					repo.PRs[n].SHA = plan.SHA
					repo.PRs[n].Evidence = identity.Evidence
				}
			}
			if repo.PinningPR != nil && repo.PinningPR.Target == target {
				p := *repo.PinningPR
				p.SHA, p.Evidence = plan.SHA, identity.Evidence
				p.ChecksEvidence.Known = false
				p.CheckResultsEvidence.Known = false
				repo.PinningPR = &p
			}
			s.repos[source.Name] = repo
			expected = plan.SHA
		}
		if err != nil && plan.SHA == "" {
			old := clonePlan(s.plans[target])
			old.Proposals = nil
			old.Evidence.Known = false
			old.Target = target
			old.Evidence.Error = message
			old.StartFailures = staleStartFailures(old.StartFailures, message)
			for i := range old.Runs {
				if old.Runs[i].Image.Reference == "" && old.Runs[i].Image.Evidence.Error == "" {
					continue
				}
				old.Runs[i].Image.Evidence.Known = false
				old.Runs[i].Image.Evidence.Error = message
			}
			s.plans[target] = old
			s.dispatcher.ObservePlan(release.Plan{Target: target, SHA: expected})
		}
		if plan.SHA == expected && expected != "" {
			oldPlan := s.plans[target]
			retainVerifiedBuilds(oldPlan, &plan)
			if err != nil && len(plan.StartFailures) == 0 && oldPlan.SHA == plan.SHA {
				plan.StartFailures = staleStartFailures(oldPlan.StartFailures, message)
				markSupersededStartFailures(&plan)
			}
			s.recordStartFailures(oldPlan, plan)
			for _, proposal := range plan.Proposals {
				if !slices.ContainsFunc(oldPlan.Proposals, func(old release.Proposal) bool { return dispatch.ID(old) == dispatch.ID(proposal) }) {
					s.record(target, proposal.SHA, "proposal", proposal.Payload, "run "+proposal.RunID, "system", fmt.Sprintf("A retry is available for %s in %s (run %s).", proposal.Job, consoleRevision(target, proposal.SHA), proposal.RunID))
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
			s.dispatcher.ObservePlan(plan)
		}
		s.drain()
		s.mu.Unlock()
	}
	imageReads.Wait()
	if imageErr = errors.Join(componentImageErr, imageErr); imageErr != nil {
		errs = append(errs, fmt.Errorf("checking container images for release %s: %w", c.Version, imageErr))
	}

	// A fresh revalidation occurs inside Send; a polled proposal is never queued.
	s.mu.Lock()
	currentGeneration := s.generation == generation && (work.scope == ReadAll || revision == s.workRevision)
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
	s.mu.Lock()
	if generation == s.generation && discover {
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
	return s.refreshImagesScoped(ctx, generation, initial, true, false)
}

func (s *Session) refreshImagesScoped(ctx context.Context, generation uint64, initial, includeRC, scheduled bool) error {
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
	if initial {
		s.record(release.Target{Repo: c.ACS().Repo}, "", "images-start", "", "Checking registry images using saved naming patterns.", "system", fmt.Sprintf("Checking container images for release %s.", c.Version))
	}
	s.mu.Unlock()
	var inventory release.Inventory
	var err error
	if includeRC {
		tags := maps.Clone(versions)
		tags["acs"] = rc.Target.Tag
		inventory, err = s.images.Inspect(ctx, c, tags)
	} else if components, ok := s.images.(ComponentInspector); ok {
		inventory, err = components.InspectComponents(ctx, c, versions)
	} else {
		inventory, err = s.images.Inspect(ctx, c, versions)
	}
	if canceled(ctx) {
		return ctx.Err()
	}
	s.mu.Lock()
	current := s.repos["acs"].Candidate
	sameCandidate := rc.Target == current.Target && rc.SHA == current.SHA
	if generation == s.generation && maps.Equal(versions, s.choices.Tags) && (!scheduled || revision == s.workRevision) &&
		(!includeRC || (sameCandidate && (!scheduled || s.rcReady()))) {
		message := ""
		if err != nil {
			message = err.Error()
		}
		s.recordReadResult(release.Target{Repo: c.ACS().Repo}, "images", message, "container images for release "+c.Version)
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
			if !s.rcReady() {
				inventory.ReleaseImages = unverifiedImages(inventory.ReleaseImages, "")
			}
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
	}
	s.mu.Unlock()
	return err
}
