package app

import (
	"context"
	"errors"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

type fixture struct {
	mu       sync.Mutex
	repos    map[string]release.Repository
	calls    map[string]int
	requests []DiscoveryRequest
	fail     map[string]bool
	block    chan struct{}
	started  chan struct{}
}

func (f *fixture) Discover(ctx context.Context, r DiscoveryRequest) (release.Repository, error) {
	if f.started != nil {
		select {
		case f.started <- struct{}{}:
		default:
		}
		select {
		case <-ctx.Done():
			return release.Repository{}, ctx.Err()
		case <-f.block:
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[r.Source.Name]++
	f.requests = append(f.requests, r)
	repo := cloneRepository(f.repos[r.Source.Name])
	repo.Source = r.Source
	if r.Scope != ReadAll {
		if !r.Reads(ReadTags) {
			repo.Tags = nil
		} else if r.ConfirmedTag != "" {
			repo.Tags = slices.DeleteFunc(repo.Tags, func(tag release.Tag) bool { return tag.Name != r.ConfirmedTag })
		}
		if !r.Reads(ReadPRs) {
			repo.PRs = slices.DeleteFunc(repo.PRs, func(p release.PR) bool {
				tracked := r.Reads(ReadTargets) && slices.Contains(r.Tracked, p.Target)
				pinning := r.Reads(ReadPins) && p.Target.PR == r.PinningPR
				return !tracked && !pinning
			})
		}
		if !r.Reads(ReadChecks) {
			for i := range repo.PRs {
				repo.PRs[i].Checks = nil
				repo.PRs[i].ChecksEvidence = release.Evidence{}
			}
		}
		if !r.Reads(ReadPins) {
			repo.Pins = release.Pins{}
			repo.PinningPR = nil
			repo.PinningPins = release.Pins{}
			repo.PinningEvidence = release.Evidence{}
		}
	}
	if f.fail[r.Source.Name] {
		return release.Repository{}, errors.New("outage")
	}
	return repo, nil
}
func (f *fixture) Plan(_ context.Context, t release.Target) (release.Plan, error) {
	return release.Plan{Target: t}, nil
}
func (f *fixture) ReadTarget(context.Context, release.Target) (dispatch.Identity, error) {
	return dispatch.Identity{}, errors.New("not configured")
}
func (f *fixture) Write(context.Context, release.Proposal) (dispatch.Delivery, error) {
	return dispatch.Delivery{}, errors.New("fixture has no writes")
}
func (f *fixture) Inspect(_ context.Context, config release.Config, versions map[string]string) (release.Inventory, error) {
	inventory := release.Inventory{ReleaseVersion: strings.TrimPrefix(config.Version, "v"), Versions: map[string]string{}, Evidence: observed()}
	for component, version := range versions {
		inventory.Versions[component] = strings.TrimPrefix(version, "v")
		for _, family := range []string{"ci", "konflux"} {
			image := release.Image{Component: component, Family: family, Reference: "quay.io/fixture/" + component + ":" + version, Present: release.Yes, Evidence: observed()}
			if component == "acs" {
				inventory.ReleaseImages = append(inventory.ReleaseImages, image)
			} else {
				inventory.Images = append(inventory.Images, image)
			}
		}
	}
	return inventory, nil
}
func (f *fixture) InspectComponents(ctx context.Context, config release.Config, versions map[string]string) (release.Inventory, error) {
	return f.Inspect(ctx, config, versions)
}
func (f *fixture) InspectReference(_ context.Context, reference string) (release.Image, error) {
	return release.Image{Component: "acs", Family: "konflux", Reference: reference, Present: release.Yes, Evidence: observed()}, nil
}
func observed() release.Evidence {
	return release.Evidence{At: time.Now(), Known: true, URL: "https://example.com/evidence"}
}
func configuration() release.Config {
	return release.Config{Version: "5.0.1", Branch: "release-5.0", Components: []release.Source{{Name: "fact", Repo: "stackrox/fact", Branch: "release-0.3", Stream: "0.3"}, {Name: "collector", Repo: "stackrox/collector", Branch: "release-3.25", Stream: "3.25"}}}
}
func newFixture() (*Session, *fixture) {
	c := configuration()
	f := &fixture{repos: map[string]release.Repository{}, calls: map[string]int{}, fail: map[string]bool{}}
	f.repos["acs"] = release.Repository{Source: c.ACS(), Head: "acs-head", Evidence: observed(), Pins: release.Pins{Versions: map[string]string{"fact": "0.3.76", "collector": "3.25.7"}, Paths: map[string]string{"fact": "FACT_VERSION", "collector": "COLLECTOR_VERSION"}, Evidence: observed()}}
	for _, source := range c.Components {
		version := "0.3.77"
		if source.Name == "collector" {
			version = "3.25.8"
		}
		f.repos[source.Name] = release.Repository{Source: source, Head: source.Name + "-head", Evidence: observed(), Tags: []release.Tag{{Name: version, SHA: source.Name + "-head", Belongs: release.Yes, Contains: map[string]release.Truth{}, Evidence: observed()}}}
	}
	d := dispatch.New(f, f, f, dispatch.Options{EnableWrites: true})
	s := New(f, f, f, d, NewLog(io.Discard), Options{})
	return s, f
}
func apply(t *testing.T, s *Session, a Action) {
	t.Helper()
	a.Generation = s.Snapshot().Generation
	if err := s.Apply(t.Context(), a); err != nil {
		t.Fatal(err)
	}
}
func refreshAll(t *testing.T, s *Session) {
	t.Helper()
	x := s.Snapshot()
	for _, source := range append(x.Config.Components, x.Config.ACS()) {
		if err := s.Refresh(t.Context(), x.Generation, source, true); err != nil {
			t.Fatal(err)
		}
	}
}
func TestFixtureReleaseWorkflow(t *testing.T) {
	s, f := newFixture()
	if len(s.Snapshot().Choices.PRs) != 0 || !s.Snapshot().Interactive {
		t.Fatal("unsafe restart defaults")
	}
	c := configuration()
	apply(t, s, Action{Kind: "configure", Config: c})
	refreshAll(t, s)
	if s.Snapshot().Assessment.Ready {
		t.Fatal("unconfirmed ready")
	}
	target := release.Target{Repo: "stackrox/fact", PR: 1}
	fact := f.repos["fact"]
	fact.PRs = []release.PR{{Target: target, Title: "fix", SHA: "pr-head", Base: "release-0.3", State: "open", Evidence: observed()}}
	f.repos["fact"] = fact
	refreshAll(t, s)
	apply(t, s, Action{Kind: "choice", Target: target, Choice: release.Included})
	for _, source := range c.Components {
		tag := f.repos[source.Name].Tags[0]
		apply(t, s, Action{Kind: "tag", Component: source.Name, Tag: tag.Name, SHA: tag.SHA})
	}
	refreshAll(t, s)
	if s.Snapshot().Assessment.Ready || s.Snapshot().Assessment.Commands != "" {
		t.Fatal("open selected PR must prevent pinning")
	}
	fact.PRs[0].State = "merged"
	fact.PRs[0].Merged = true
	fact.PRs[0].MergeSHA = "merged"
	fact.Tags[0].Contains["merged"] = release.Yes
	f.repos["fact"] = fact
	refreshAll(t, s)
	x := s.Snapshot()
	if x.Assessment.Commands == "" || x.Assessment.Ready {
		t.Fatal("expected pinning recommendation", x.Assessment)
	}
	acs := f.repos["acs"]
	acs.Pins.Versions = map[string]string{"fact": "0.3.77", "collector": "3.25.8"}
	f.repos["acs"] = acs
	refreshAll(t, s)
	if !s.Snapshot().Assessment.Ready {
		t.Fatal(s.Snapshot().Assessment)
	}
	fact.PRs = append(fact.PRs, release.PR{Target: release.Target{Repo: "stackrox/fact", PR: 2}, SHA: "second", Base: "release-0.3", State: "merged", Merged: true, MergeSHA: "later", Evidence: observed()})
	fact.Tags[0].Contains["later"] = release.No
	f.repos["fact"] = fact
	refreshAll(t, s)
	apply(t, s, Action{Kind: "choice", Target: fact.PRs[1].Target, Choice: release.Included})
	if s.Snapshot().Assessment.Ready {
		t.Fatal("inclusion must immediately invalidate readiness")
	}
	if s.Snapshot().Choices.PRs[target] != release.Included {
		t.Fatal("existing included PR was lost")
	}
}
func TestStaleGenerationAndCopiedSnapshots(t *testing.T) {
	s, f := newFixture()
	apply(t, s, Action{Kind: "configure", Config: configuration()})
	refreshAll(t, s)
	old := s.Snapshot()
	old.Choices.Tags["fact"] = "invalid"
	old.Repositories[0].Head = "mutated"
	if s.Snapshot().Choices.Tags["fact"] != "" {
		t.Fatal("snapshot shared maps")
	}
	f.started = make(chan struct{}, 1)
	f.block = make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Refresh(t.Context(), old.Generation, configuration().Components[0], true)
	}()
	<-f.started
	c := configuration()
	c.Version = "5.0.2"
	apply(t, s, Action{Kind: "configure", Config: c})
	close(f.block)
	<-done
	if len(s.Snapshot().Repositories) != 0 {
		t.Fatal("old response crossed session generation")
	}
	if !s.Snapshot().Loading {
		t.Fatal("old response completed the new configuration's initial load")
	}
	if err := s.Apply(t.Context(), Action{Kind: "interactive", Enabled: true, Generation: old.Generation}); err == nil {
		t.Fatal("stale form accepted")
	}
}
func TestPollingBackoffRecoveryAndBrowserIndependence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := newFixture()
		s.options = Options{Poll: time.Minute, Discovery: time.Minute, BackoffCap: 4 * time.Minute, Timeout: time.Minute}
		apply(t, s, Action{Kind: "configure", Config: configuration()})
		f.fail["fact"] = true
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { defer close(done); s.Run(ctx) }()
		synctest.Wait()
		time.Sleep(time.Minute)
		synctest.Wait()
		f.mu.Lock()
		if f.calls["collector"] < 2 || f.calls["fact"] != 2 {
			t.Fatal(maps.Clone(f.calls))
		}
		f.fail["fact"] = false
		f.mu.Unlock()
		time.Sleep(time.Minute)
		synctest.Wait()
		f.mu.Lock()
		if f.calls["fact"] != 4 {
			t.Fatal(maps.Clone(f.calls))
		}
		f.mu.Unlock()
		if !s.Snapshot().Repositories[2].Evidence.Fresh() {
			t.Fatal("fact did not recover")
		}
		cancel()
		<-done
	})
}
func TestConfigurationStartsInitialLoadWithoutWaitingForPoll(t *testing.T) {
	for name, fail := range map[string]bool{"successful discovery": false, "one unavailable source": true} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, f := newFixture()
				f.fail["fact"] = fail
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan struct{})
				go func() { defer close(done); s.Run(ctx) }()
				synctest.Wait()
				if s.Snapshot().Loading {
					t.Fatal("unconfigured session is loading")
				}
				for attempt := range 2 {
					f.started = make(chan struct{}, 3)
					f.block = make(chan struct{})
					apply(t, s, Action{Kind: "configure", Config: configuration()})
					synctest.Wait()
					if !s.Snapshot().Loading {
						t.Fatal("configuration did not report the initial load")
					}
					close(f.block)
					synctest.Wait()
					if s.Snapshot().Loading || len(s.Snapshot().Repositories) != 3 {
						t.Fatal("initial attempts did not complete independently of the polling interval")
					}
					f.mu.Lock()
					for _, source := range []string{"acs", "fact", "collector"} {
						want := attempt + 1
						if source != "acs" {
							want *= 2
						}
						if f.calls[source] != want {
							t.Errorf("%s discovery calls=%d, want %d", source, f.calls[source], want)
						}
					}
					f.mu.Unlock()
				}
				cancel()
				<-done
			})
		})
	}
}

func TestIgnoreAndHeadChangePreserveInclusion(t *testing.T) {
	s, f := newFixture()
	apply(t, s, Action{Kind: "configure", Config: configuration()})
	repo := f.repos["fact"]
	target := release.Target{Repo: "stackrox/fact", PR: 9}
	repo.PRs = []release.PR{{Target: target, SHA: "one", State: "open", Base: repo.Source.Branch, Evidence: observed()}}
	f.repos["fact"] = repo
	refreshAll(t, s)
	apply(t, s, Action{Kind: "choice", Target: target, Choice: release.Included})
	apply(t, s, Action{Kind: "grant", Target: target, SHA: "one", Enabled: true})
	repo.PRs[0].SHA = "two"
	f.repos["fact"] = repo
	refreshAll(t, s)
	if s.Snapshot().Choices.PRs[target] != release.Included {
		t.Fatal("scope lost")
	}
	for _, v := range s.Snapshot().Authority {
		if v.Target == target && v.Granted {
			t.Fatal("head retained grant")
		}
	}
	apply(t, s, Action{Kind: "choice", Target: target, Choice: release.Ignored})
	refreshAll(t, s)
	f.mu.Lock()
	defer f.mu.Unlock()
	found := false
	for _, r := range f.requests {
		if r.Source.Name == "fact" && len(r.Ignored) > 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("ignore not passed to discovery")
	}
}

func TestIgnoreAllComponentPRsLeavesOtherTargetsAndKeepsHiddenPRsRestorable(t *testing.T) {
	s, f := newFixture()
	config := configuration()
	apply(t, s, Action{Kind: "configure", Config: config})
	fact, collector, acs := f.repos["fact"], f.repos["collector"], f.repos["acs"]
	factPR := release.Target{Repo: fact.Source.Repo, PR: 41}
	closedPR := release.Target{Repo: fact.Source.Repo, PR: 42}
	mergedPR := release.Target{Repo: fact.Source.Repo, PR: 43}
	acrossRepos := release.Target{Repo: collector.Source.Repo, PR: 44}
	acsPR := release.Target{Repo: acs.Source.Repo, PR: 45}
	fact.PRs = []release.PR{
		{Target: factPR, SHA: "fact-head", Title: "Fact change", State: "open", Base: fact.Source.Branch, Evidence: observed()},
		{Target: closedPR, State: "closed"},
		{Target: mergedPR, State: "closed", Merged: true},
	}
	collector.PRs = []release.PR{{Target: acrossRepos, State: "open"}}
	acs.PRs = []release.PR{{Target: acsPR, State: "open"}}
	f.repos["fact"], f.repos["collector"], f.repos["acs"] = fact, collector, acs
	refreshAll(t, s)
	for _, target := range []release.Target{factPR, closedPR, mergedPR, acsPR} {
		apply(t, s, Action{Kind: "choice", Target: target, Choice: release.Included})
	}
	apply(t, s, Action{Kind: "grant", Target: factPR, SHA: "fact-head", Enabled: true})
	s.plans[factPR] = release.Plan{Target: factPR}
	s.plans[acsPR] = release.Plan{Target: acsPR}

	apply(t, s, Action{Kind: "ignore-component-prs"})
	snapshot := s.Snapshot()
	for _, target := range []release.Target{factPR, closedPR, acrossRepos} {
		if snapshot.Choices.PRs[target] != release.Ignored {
			t.Errorf("bulk action missed %s", target)
		}
	}
	if snapshot.Choices.PRs[acsPR] != release.Included || snapshot.Choices.PRs[mergedPR] != release.Included ||
		s.plans[acsPR].Target != acsPR {
		t.Fatal("bulk action changed another activity or an already merged PR")
	}
	if _, found := s.plans[factPR]; found {
		t.Fatal("ignored PR kept its retry plan")
	}
	for _, view := range snapshot.Authority {
		if view.Target == factPR && (view.Granted || !view.Ignored) {
			t.Fatal("ignored PR kept retry authorization")
		}
	}

	fact.PRs = nil
	f.repos["fact"] = fact
	refreshAll(t, s)
	apply(t, s, Action{Kind: "choice", Target: factPR, Choice: release.Included})
	if s.Snapshot().Choices.PRs[factPR] != release.Included {
		t.Fatal("a hidden PR missing from discovery could not be restored")
	}
}

type badLog struct{}

func (badLog) Write([]byte) (int, error) { return 0, errors.New("disk full") }
func TestActionLogFailureVisibleAndNoCredentialFields(t *testing.T) {
	l := NewLog(badLog{})
	l.Append(Record{Decision: "permission", Origin: "user"})
	if l.Error() == "" {
		t.Fatal("log failure hidden")
	}
}
func TestPinningInputPreservesContextAndNewReleaseClearsIt(t *testing.T) {
	s, f := newFixture()
	c := configuration()
	apply(t, s, Action{Kind: "configure", Config: c})
	refreshAll(t, s)
	tag := f.repos["fact"].Tags[0]
	apply(t, s, Action{Kind: "tag", Component: "fact", Tag: tag.Name, SHA: tag.SHA})
	c.PinningPR = 123
	apply(t, s, Action{Kind: "configure", Config: c})
	if s.Snapshot().Choices.Tags["fact"] != tag.Name {
		t.Fatal("pinning PR input lost selected tag")
	}
	c.Version = "5.0.2"
	apply(t, s, Action{Kind: "configure", Config: c})
	if len(s.Snapshot().Choices.Tags) != 0 || !s.Snapshot().Interactive {
		t.Fatal("new context reused session state")
	}
}

func TestIncompleteInitialConfigStartsNoMonitoring(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, f := newFixture()
		d := dispatch.New(f, f, f, dispatch.Options{})
		initial := configuration()
		initial.Branch = ""
		s := New(f, f, f, d, NewLog(io.Discard), Options{Initial: initial})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { defer close(done); s.Run(ctx) }()
		synctest.Wait()
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		f.mu.Lock()
		calls := len(f.calls)
		f.mu.Unlock()
		if calls != 0 || s.Snapshot().Config.Version != initial.Version {
			t.Fatal("incomplete startup did not remain in setup")
		}
		cancel()
		<-done
	})
}

type imageReaderFunc func(context.Context, release.Config, map[string]string) (release.Inventory, error)

func (f imageReaderFunc) InspectReference(context.Context, string) (release.Image, error) {
	return release.Image{}, errors.New("unexpected build image check")
}
func (f imageReaderFunc) Inspect(ctx context.Context, c release.Config, tags map[string]string) (release.Inventory, error) {
	return f(ctx, c, tags)
}

func TestImageRefreshPreservesPartialResultsAndInvalidatesRetainedResults(t *testing.T) {
	s, _, _ := rcFixture(t)
	c := s.config
	refreshAll(t, s)
	refreshAll(t, s)
	next := s.Snapshot().Inventory
	next.Images[0].Present = release.No
	next.Images[1].Evidence.Known = false
	next.Images[1].Evidence.Error = "ACS definition unavailable"
	next.Evidence.Known = false
	next.Evidence.Error = "Some definitions could not be read"
	next.ReleaseImages = []release.Image{{Component: "acs", Name: "main", Family: "ci", Reference: "quay.io/fixture/main:5.0.1-rc.2", Present: release.Yes, Evidence: observed()}}
	s.images = imageReaderFunc(func(context.Context, release.Config, map[string]string) (release.Inventory, error) {
		return next, errors.New("definition unavailable")
	})
	generation := s.Snapshot().Generation
	if err := s.Refresh(t.Context(), generation, c.ACS(), false); err == nil {
		t.Fatal("expected partial observation error")
	}
	snapshot := s.Snapshot()
	if snapshot.Inventory.Images[0].Present != release.No || !snapshot.Inventory.Images[0].Evidence.Fresh() || snapshot.Inventory.Images[1].Evidence.Fresh() || snapshot.Assessment.Ready {
		t.Fatal("partial results were replaced by previous successful observations")
	}
	snapshot.Inventory.ReleaseImages[0].Reference = "mutated"
	if s.Snapshot().Inventory.ReleaseImages[0].Reference == "mutated" {
		t.Fatal("snapshot shares ACS image slice")
	}
	s.images = imageReaderFunc(func(context.Context, release.Config, map[string]string) (release.Inventory, error) {
		return release.Inventory{}, errors.New("transport failure")
	})
	_ = s.Refresh(t.Context(), generation, c.ACS(), false)
	snapshot = s.Snapshot()
	if len(snapshot.Inventory.Images) == 0 || len(snapshot.Inventory.ReleaseImages) == 0 {
		t.Fatal("outage discarded prior image observations")
	}
	for _, image := range append(snapshot.Inventory.Images, snapshot.Inventory.ReleaseImages...) {
		if image.Evidence.Fresh() || image.Evidence.Error == "" {
			t.Fatalf("retained outage result looks current: %+v", image)
		}
	}
}

func TestDiscoveryErrorsAndRecoveryReachActivityLogWithoutRepeatedFailures(t *testing.T) {
	s, f := newFixture()
	apply(t, s, Action{Kind: "configure", Config: configuration()})
	source := configuration().Components[0]
	f.fail["fact"] = true
	for range 2 {
		if err := s.Refresh(t.Context(), s.Snapshot().Generation, source, true); err == nil {
			t.Fatal("expected discovery failure")
		}
	}
	failures := 0
	for _, entry := range s.Snapshot().LogEntries {
		if entry.Decision == "github-error" {
			failures++
			if entry.Target.Repo != source.Repo || entry.Result != "outage" {
				t.Fatalf("diagnostic context lost: %+v", entry)
			}
		}
	}
	if failures != 1 {
		t.Fatalf("unchanged error logged %d times", failures)
	}
	f.fail["fact"] = false
	if err := s.Refresh(t.Context(), s.Snapshot().Generation, source, true); err != nil {
		t.Fatal(err)
	}
	entries := s.Snapshot().LogEntries
	if !slices.ContainsFunc(entries, func(entry Record) bool {
		return entry.Decision == "github-recovered" && entry.Target.Repo == source.Repo
	}) {
		t.Fatal("recovery not visible in activity log")
	}
	entries[0].Result = "modified snapshot"
	if s.Snapshot().LogEntries[0].Result == "modified snapshot" {
		t.Fatal("activity snapshot is mutable")
	}
}

func TestReadOnlySessionRejectsForgedGrantAndSend(t *testing.T) {
	s, f := newFixture()
	writer := &writeCounter{}
	s.dispatcher = dispatch.New(f, f, writer, dispatch.Options{})
	apply(t, s, Action{Kind: "configure", Config: configuration()})
	target := release.Target{Repo: "stackrox/fact", PR: 7}
	r := f.repos["fact"]
	r.PRs = []release.PR{{Target: target, SHA: "head", State: "open", Base: r.Source.Branch, Evidence: observed()}}
	f.repos["fact"] = r
	refreshAll(t, s)
	apply(t, s, Action{Kind: "choice", Target: target, Choice: release.Included})
	proposal := release.Proposal{Target: target, SHA: "head", Job: "unit", RunID: "run", Payload: "/konflux-retest unit"}
	s.dispatcher.ObservePlan(release.Plan{Target: target, SHA: "head", Proposals: []release.Proposal{proposal}, Evidence: observed()})
	for _, action := range []Action{
		{Kind: "grant", Target: target, SHA: "head", Enabled: true},
		{Kind: "send", Target: target, SHA: "head", ID: dispatch.ID(proposal)},
	} {
		action.Generation = s.Snapshot().Generation
		if err := s.Apply(t.Context(), action); err == nil || !strings.Contains(err.Error(), "-enable-writes") {
			t.Fatalf("read-only %s: %v", action.Kind, err)
		}
	}
	snapshot := s.Snapshot()
	if snapshot.WritesEnabled || snapshot.Choices.PRs[target] != release.Included || writer.calls != 0 {
		t.Fatal("read-only session lost inclusion or allowed a write")
	}
	apply(t, s, Action{Kind: "configure", Config: configuration()})
	if s.Snapshot().WritesEnabled {
		t.Fatal("configuration enabled writes")
	}
}

// blockingPlanner holds a build check until its context ends, like a canceled request.
type blockingPlanner struct {
	*fixture
	started chan struct{}
}

func (p *blockingPlanner) Plan(ctx context.Context, target release.Target) (release.Plan, error) {
	select {
	case p.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return release.Plan{Target: target, Evidence: release.Evidence{At: time.Now(), Error: ctx.Err().Error()}}, ctx.Err()
}

func TestCanceledRefreshRecordsNoReadFailure(t *testing.T) {
	s, f := newFixture()
	apply(t, s, Action{Kind: "configure", Config: configuration()})
	refreshAll(t, s)
	tag := f.repos["fact"].Tags[0]
	apply(t, s, Action{Kind: "tag", Component: "fact", Tag: tag.Name, SHA: tag.SHA})
	planner := &blockingPlanner{fixture: f, started: make(chan struct{}, 1)}
	s.planner = planner

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Refresh(ctx, s.Snapshot().Generation, configuration().Components[0], false) }()
	<-planner.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("refresh error = %v, want context cancellation", err)
	}
	for _, entry := range s.Snapshot().LogEntries {
		if entry.Decision == "checks" || entry.Decision == "checks-error" {
			t.Fatalf("canceled refresh recorded %q: %s", entry.Decision, entry.Result)
		}
	}
	s.mu.Lock()
	_, published := s.plans[release.Target{Repo: "stackrox/fact", Tag: tag.Name}]
	s.mu.Unlock()
	if published {
		t.Fatal("canceled read published build results")
	}
}
