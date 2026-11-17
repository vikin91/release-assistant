package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

type (
	// Options controls polling and bounds each source's work.
	Options struct {
		Poll, Discovery, BackoffCap, Timeout time.Duration
		Initial                              release.Config
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
		Config       release.Config
		Choices      release.Choices
		Repositories []release.Repository
		Inventory    release.Inventory
		Plans        []release.Plan
		Assessment   release.Assessment
		Authority    []dispatch.View
		Interactive  bool
		// WritesEnabled reports startup permission; target grants remain independent.
		WritesEnabled   bool
		Error, LogError string
		Generation      uint64
		History         []Cycle
		LogEntries      []Record
		// Loading remains true until every source completes its first refresh attempt.
		Loading bool
		// Phase identifies the next release decision receiving background work.
		Phase                             string
		SelectionComplete, SelectionReady bool
	}
	// Action is a typed browser or CLI session decision.
	Action struct {
		Kind                    string
		Config                  release.Config
		Target                  release.Target
		SHA, ID, Component, Tag string
		Choice                  release.Choice
		Enabled                 bool
		Generation              uint64
	}
	// Session owns release observations. Dispatcher alone owns write grants.
	Session struct {
		mu                sync.Mutex
		config            release.Config
		choices           release.Choices
		repos             map[string]release.Repository
		plans             map[release.Target]release.Plan
		inventory         release.Inventory
		confirmedSHAs     map[string]string
		tagInitialized    map[string]bool
		initiallyLoaded   map[string]bool
		tagsLoaded        map[string]bool
		prLoaded          map[string]bool
		selectionComplete bool
		workRevision      uint64
		readErrors        map[string]string
		generation        uint64
		errorText         string
		discovery         Discoverer
		planner           Planner
		images            Inspector
		dispatcher        *dispatch.Dispatcher
		log               *Log
		options           Options
		changed           chan struct{}
		history           []Cycle
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
	s.initiallyLoaded = map[string]bool{}
	s.tagsLoaded = map[string]bool{}
	s.prLoaded = map[string]bool{}
	s.selectionComplete = false
	s.workRevision++
	s.readErrors = map[string]string{}
	s.inventory = release.Inventory{}
	s.history = nil
	s.generation++
}
func (s *Session) record(target release.Target, sha, decision, payload, result, origin string) {
	if s.log != nil {
		s.log.Append(Record{At: time.Now(), Release: s.config.Version, Target: target, SHA: sha, Decision: decision, Payload: payload, Result: result, Origin: origin})
	}
}
func (s *Session) drain() {
	for _, e := range s.dispatcher.Events() {
		s.record(e.Target, e.SHA, e.Decision, e.Payload, e.Result, e.Origin)
	}
}

// recordReadResult logs changed failures and recovery without repeating unchanged polls.
func (s *Session) recordReadResult(target release.Target, operation, message string) {
	key := operation + " " + target.String()
	previous := s.readErrors[key]
	if message == previous {
		return
	}
	if message == "" {
		delete(s.readErrors, key)
		s.record(target, "", operation+"-recovered", "", "Reads recovered.", "system")
		return
	}
	s.readErrors[key] = message
	s.record(target, "", operation+"-error", "", message, "system")
}

// Apply validates configuration and enforces session actions independently of the UI.
func (s *Session) Apply(ctx context.Context, a Action) error {
	s.mu.Lock()
	if a.Kind != "configure" && a.Generation != s.generation {
		s.mu.Unlock()
		return fmt.Errorf("page belongs to an earlier configuration; reload")
	}
	if a.Kind == "send" {
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
		if sameContext {
			if s.config.PinningPR != a.Config.PinningPR && s.config.PinningPR > 0 {
				_ = s.dispatcher.Grant(release.Target{Repo: s.config.ACS().Repo, PR: s.config.PinningPR}, "", false)
			}
			s.generation++
		} else {
			s.dispatcher.Reset()
			s.reset()
		}
		s.config = a.Config
		s.config.Components = slices.Clone(a.Config.Components)
		s.initiallyLoaded = map[string]bool{}
		s.tagsLoaded = map[string]bool{}
		s.prLoaded = map[string]bool{}
		s.selectionComplete = false
		s.workRevision++
		s.errorText = ""
		s.record(release.Target{}, "", "configure", "", "new selection context", "user")
		select {
		case s.changed <- struct{}{}:
		default:
		}
	case "choice":
		if a.Choice != "" && a.Choice != release.Included && a.Choice != release.Excluded && a.Choice != release.Ignored {
			return fmt.Errorf("invalid inclusion choice")
		}
		if !s.hasTarget(a.Target) {
			return fmt.Errorf("unknown target")
		}
		s.choices.PRs[a.Target] = a.Choice
		s.dispatcher.Ignore(a.Target, a.Choice == release.Ignored || a.Choice == release.Excluded)
		if a.Choice == release.Ignored || a.Choice == release.Excluded {
			delete(s.plans, a.Target)
		}
		s.selectionComplete = false
		s.workRevision++
		s.suspendPlans()
		s.record(a.Target, a.SHA, "inclusion", "", string(a.Choice), "user")
		select {
		case s.changed <- struct{}{}:
		default:
		}
	case "grant":
		if !s.hasTarget(a.Target) {
			return fmt.Errorf("unknown target")
		}
		err = s.dispatcher.Grant(a.Target, a.SHA, a.Enabled)
	case "interactive":
		s.dispatcher.Interactive(a.Enabled)
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
			cycle := Cycle{Component: a.Component, Tag: old, SHA: s.confirmedSHAs[a.Component], At: time.Now(), Plan: clonePlan(s.plans[release.Target{Repo: r.Source.Repo, Tag: old}])}
			for _, image := range s.inventory.Images {
				if image.Component == a.Component {
					cycle.Images = append(cycle.Images, image)
				}
			}
			s.history = append(s.history, cycle)
			s.dispatcher.Ignore(release.Target{Repo: r.Source.Repo, Tag: old}, true)
		}
		s.selectionComplete = false
		s.workRevision++
		s.suspendPlans()
		s.choices.Tags[a.Component] = a.Tag
		s.confirmedSHAs[a.Component] = a.SHA
		s.tagInitialized[a.Component] = true
		s.dispatcher.Ignore(release.Target{Repo: r.Source.Repo, Tag: a.Tag}, false)
		s.record(release.Target{Repo: r.Source.Repo, Tag: a.Tag}, a.SHA, "tag-confirmed", "", a.Tag, "user")
		select {
		case s.changed <- struct{}{}:
		default:
		}
	case "finish-selection":
		if !s.selectionReady() {
			return fmt.Errorf("confirm component tags and load PR inventories before finishing selection")
		}
		s.selectionComplete = true
		s.workRevision++
		s.record(release.Target{}, "", "selection-complete", "", "PR selection confirmed; monitoring selected builds and pinning.", "user")
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
	s.drain()
	return err
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
		Phase: s.phase(), SelectionComplete: s.selectionComplete, SelectionReady: s.selectionReady(),
		Interactive: s.dispatcher.IsInteractive(), WritesEnabled: s.dispatcher.WritesEnabled(),
		Authority: s.dispatcher.Views(),
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
		p.Runs = slices.Clone(p.Runs)
		p.Proposals = slices.Clone(p.Proposals)
		p.Warnings = slices.Clone(p.Warnings)
		p.Advisories = slices.Clone(p.Advisories)
		x.Plans = append(x.Plans, p)
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
	x.Assessment = release.Assess(release.Input{Config: c, Choices: choices, Repositories: x.Repositories, Inventory: x.Inventory, SelectionPending: !s.selectionComplete})
	return x
}
func cloneRepository(r release.Repository) release.Repository {
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
	s.mu.Lock()
	if source.Name != "acs" && s.tagsLoaded[source.Name] && len(s.repos[source.Name].Tags) > 0 && s.phase() == "tags" {
		discoverAt = time.Now().Add(s.options.Discovery)
	}
	s.mu.Unlock()
	var delay time.Duration
	failures := 0
	for {
		previousDelay := delay
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
				s.record(release.Target{Repo: source.Repo}, "", "retry-wait", "", fmt.Sprintf("Next check attempt in %s; next GitHub discovery at %s.", delay, discoverAt.Format(time.RFC3339)), "system")
			}
			s.mu.Unlock()
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// Refresh performs an explicit full source cycle. Scheduled refreshes obey release phases.
func (s *Session) Refresh(ctx context.Context, generation uint64, source release.Source, discover bool) error {
	return s.refresh(ctx, generation, source, discover, sourceWork{scope: ReadAll, plans: true, images: source.Name == "acs"})
}

func (s *Session) refresh(ctx context.Context, generation uint64, source release.Source, discover bool, work sourceWork) error {
	ctx, cancel := context.WithTimeout(ctx, s.options.Timeout)
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
	initial := discover && !s.initiallyLoaded[source.Name]
	if initial {
		s.record(release.Target{Repo: source.Repo}, "", "github-start", "", "Reading GitHub branch "+source.Branch+", PRs, and tags.", "system")
	}
	request := DiscoveryRequest{Source: source, ConfirmedTag: s.choices.Tags[source.Name], PinningPR: c.PinningPR, Version: c.Version, Scope: work.scope}
	revision := s.workRevision
	for t, choice := range s.choices.PRs {
		if t.Repo == source.Repo {
			if choice == release.Ignored || choice == release.Excluded {
				request.Ignored = append(request.Ignored, t)
			} else if choice == release.Included || work.scope == ReadAll {
				request.Tracked = append(request.Tracked, t)
			}
		}
	}
	s.mu.Unlock()
	var imageErr error
	if work.images {
		imageReads.Go(func() { imageErr = s.refreshImages(ctx, generation, initial) })
	}
	var errs []error
	if discover {
		r, err := s.discovery.Discover(ctx, request)
		if err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			errs = append(errs, err)
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
		s.recordReadResult(release.Target{Repo: source.Repo}, "github", message)
		if initial {
			s.record(release.Target{Repo: source.Repo}, r.Head, "github-complete", "", fmt.Sprintf("GitHub discovery returned %d PRs and %d tags; complete=%t.", len(r.PRs), len(r.Tags), err == nil), "system")
		}
		old := s.repos[source.Name]
		if work.scope != ReadAll {
			r = mergeDiscovery(old, r, request)
		}
		if request.Reads(ReadPRs) {
			s.prLoaded[source.Name] = err == nil
		}
		if err != nil && r.Head == "" {
			r = retainDiscoveryFailure(old, r, err)
		}
		r.Source = source
		if err != nil {
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
				if s.choices.PRs[p.Target] != release.Ignored {
					p.Evidence.Known = false
					p.Evidence.Error = "not returned by latest discovery"
					r.PRs = append(r.PRs, p)
				}
			}
		}
		s.repos[source.Name] = r
		for _, p := range r.PRs {
			s.dispatcher.ObserveIdentity(dispatch.Identity{Target: p.Target, SHA: p.SHA, Base: p.Base, Open: p.State == "open" && p.Base == source.Branch, OptOut: p.OptOut, Evidence: p.Evidence})
			if p.State != "open" {
				if plan, exists := s.plans[p.Target]; exists {
					plan.Proposals = nil
					plan.Evidence.Known = false
					plan.Evidence.Error = "PR is no longer open; retained build observations"
					s.plans[p.Target] = plan
					s.dispatcher.ObservePlan(plan)
				}
			}
		}
		if confirmed := s.choices.Tags[source.Name]; confirmed != "" {
			if !slices.ContainsFunc(r.Tags, func(tag release.Tag) bool { return tag.Name == confirmed }) {
				s.dispatcher.ObserveIdentity(dispatch.Identity{Target: release.Target{Repo: source.Repo, Tag: confirmed}, SHA: s.confirmedSHAs[source.Name], Evidence: release.Evidence{At: time.Now(), Error: "confirmed tag was not returned by current discovery"}})
			}
			for _, tag := range r.Tags {
				if tag.Name == confirmed {
					t := release.Target{Repo: source.Repo, Tag: tag.Name}
					s.dispatcher.ObserveIdentity(dispatch.Identity{Target: t, SHA: tag.SHA, Open: true, Evidence: tag.Evidence})
					if tag.SHA != "" && tag.SHA != s.confirmedSHAs[source.Name] {
						delete(s.choices.Tags, source.Name)
						delete(s.confirmedSHAs, source.Name)
						s.selectionComplete = false
						s.workRevision++
						s.suspendPlans()
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
		if old.Head != r.Head || old.Evidence.Fresh() != r.Evidence.Fresh() {
			s.record(release.Target{Repo: source.Repo}, r.Head, "observation", "", fmt.Sprintf("current=%t", r.Evidence.Fresh()), "system")
		}
		s.drain()
		s.mu.Unlock()
	}
	s.mu.Lock()
	if generation != s.generation || (work.scope != ReadAll && revision != s.workRevision) {
		s.mu.Unlock()
		return nil
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
	s.mu.Unlock()
	for _, target := range targets {
		s.mu.Lock()
		ignore := generation != s.generation || (work.scope != ReadAll && revision != s.workRevision) || (s.choices.PRs[target] == release.Ignored || s.choices.PRs[target] == release.Excluded)
		if initial && !ignore {
			s.record(target, "", "checks-start", "", "Reading build observations.", "system")
		}
		s.mu.Unlock()
		if ignore {
			continue
		}
		plan, err := s.planner.Plan(ctx, target)
		if imageErr := s.inspectPinningImages(ctx, generation, &plan); imageErr != nil {
			errs = append(errs, imageErr)
		}
		if err != nil {
			errs = append(errs, err)
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
		s.recordReadResult(target, "checks", message)
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
				repo.PinningPR = &p
			}
			s.repos[source.Name] = repo
			expected = plan.SHA
		}
		if err != nil && plan.SHA == "" {
			old := clonePlan(s.plans[target])
			old.Evidence.Known = false
			old.Target = target
			old.Evidence.Error = message
			for i := range old.Runs {
				old.Runs[i].Image.Evidence.Known = false
				old.Runs[i].Image.Evidence.Error = message
			}
			s.plans[target] = old
			s.dispatcher.ObservePlan(release.Plan{Target: target, SHA: expected})
		}
		if plan.SHA == expected && expected != "" {
			oldPlan := s.plans[target]
			for _, proposal := range plan.Proposals {
				if !slices.ContainsFunc(oldPlan.Proposals, func(old release.Proposal) bool { return dispatch.ID(old) == dispatch.ID(proposal) }) {
					s.record(target, proposal.SHA, "proposal", proposal.Payload, "run "+proposal.RunID, "system")
				}
			}
			if oldPlan.Action != plan.Action || oldPlan.Evidence.Fresh() != plan.Evidence.Fresh() {
				s.record(target, plan.SHA, "checks", "", plan.Action, "system")
			}
			s.plans[target] = plan
			s.dispatcher.ObservePlan(plan)
		}
		s.drain()
		s.mu.Unlock()
	}
	imageReads.Wait()
	if imageErr != nil {
		errs = append(errs, imageErr)
	}

	// A fresh revalidation occurs inside Send; a polled proposal is never queued.
	s.mu.Lock()
	currentGeneration := s.generation == generation
	s.mu.Unlock()
	if work.plans && currentGeneration && !s.dispatcher.IsInteractive() {
		for _, view := range s.dispatcher.Views() {
			if view.Target.Repo != source.Repo {
				continue
			}
			for _, offer := range view.Offers {
				if offer.CanApprove {
					_, err := s.dispatcher.Send(ctx, view.Target, offer.ID, false)
					if err != nil {
						errs = append(errs, err)
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
	return p
}

// unverifiedImages retains the last observation while making its failed refresh explicit.
func unverifiedImages(previous []release.Image, reason string) []release.Image {
	images := slices.Clone(previous)
	for index := range images {
		images[index].Evidence.Known = false
		images[index].Evidence.Error = reason
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

// refreshImages commits registry evidence only while its release and tag choices remain current.
func (s *Session) refreshImages(ctx context.Context, generation uint64, initial bool) error {
	s.mu.Lock()
	if generation != s.generation {
		s.mu.Unlock()
		return nil
	}
	c := s.config
	versions := maps.Clone(s.choices.Tags)
	if initial {
		s.record(release.Target{Repo: c.ACS().Repo}, "", "images-start", "", "Checking registry images using saved naming patterns.", "system")
	}
	s.mu.Unlock()
	inventory, err := s.images.Inspect(ctx, c, versions)
	s.mu.Lock()
	if generation == s.generation && maps.Equal(versions, s.choices.Tags) {
		message := ""
		if err != nil {
			message = err.Error()
		}
		s.recordReadResult(release.Target{Repo: c.ACS().Repo}, "images", message)
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
		if err != nil && len(inventory.ReleaseImages) == 0 {
			inventory.ReleaseImages = unverifiedImages(s.inventory.ReleaseImages, "The ACS RC.0 image check returned no new results; earlier results are unverified.")
		}
		inventory.Images = retainLastImagePresence(inventory.Images, s.inventory.Images)
		inventory.ReleaseImages = retainLastImagePresence(inventory.ReleaseImages, s.inventory.ReleaseImages)
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
				s.record(release.Target{Repo: c.ACS().Repo}, "", "image", current.Reference, string(current.Present), "system")
			}
		}
		s.inventory = inventory
	}
	s.mu.Unlock()
	return err
}
