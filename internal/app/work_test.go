package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"release-assistant/internal/githubread"
	"release-assistant/internal/release"
)

func TestMonitorWarningNamesFailedReadAndBranch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, fixture := newFixture()
		s.config = configuration()
		fixture.fail["acs"] = true
		var output bytes.Buffer
		s.log = NewLog(&output, false)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { defer close(done); s.monitor(ctx, s.generation, s.config.ACS(), false) }()
		synctest.Wait()
		cancel()
		<-done
		for _, want := range []string{
			"Could not read pull requests, component version pins on stackrox/release-5.0. outage",
			"Warn: The last monitoring attempt failed while reading pull requests, component version pins on stackrox/release-5.0: outage.",
			"The next monitoring attempt for stackrox/release-5.0 is in 2 minutes.",
		} {
			if !strings.Contains(output.String(), want) {
				t.Fatalf("warning lacks %q: %s", want, output.String())
			}
		}
		for _, unwanted := range []string{"Results in stackrox are out of date", "Context for", "Finished checking", "0 release tags"} {
			if strings.Contains(output.String(), unwanted) {
				t.Fatalf("misleading log %q: %s", unwanted, output.String())
			}
		}
	})
}

func TestPRDiscoveryLogDoesNotClaimTagsWereRead(t *testing.T) {
	s, fixture := newFixture()
	s.config = configuration()
	source := s.config.Components[0]
	repo := fixture.repos[source.Name]
	repo.PRs = make([]release.PR, 4)
	fixture.repos[source.Name] = repo
	var output bytes.Buffer
	s.log = NewLog(&output, false)
	if err := s.refresh(t.Context(), s.generation, source, true, sourceWork{scope: ReadPRs, revision: s.workRevision}); err != nil {
		t.Fatal(err)
	}
	want := "Found 4 pull requests while checking fact/release-0.3. Branch head of fact/release-0.3 is fact-head."
	if !strings.Contains(output.String(), want) || strings.Contains(output.String(), "release tags") {
		t.Fatalf("discovery log misrepresents the request: %s", output.String())
	}
}

func TestMonitorLogsScheduledPRDiscovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := newFixture()
		s.config = configuration()
		s.log = NewLog(io.Discard, false)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go s.monitor(ctx, s.generation, s.config.Components[0], false)
		synctest.Wait()
		startup := s.log.Recent()

		time.Sleep(s.options.RefreshIntervals[BeforePRs] - time.Minute)
		synctest.Wait()
		if got := s.log.Recent(); !slices.Equal(got, startup) {
			t.Fatalf("unchanged active polls produced activity: %+v", got)
		}

		time.Sleep(time.Minute)
		synctest.Wait()
		progress := s.log.Recent()[len(startup):]
		want := "Checking GitHub for pull request updates on fact/release-0.3."
		if len(progress) != 1 || progress[0].Level() != "Info" || progress[0].Message() != want {
			t.Fatalf("expected one discovery progress entry with %q, got %+v", want, progress)
		}
	})
}

// fetchSourcePolls runs the eligible production jobs in the order specified by the test.
// Recompute eligibility after each read because tag discovery can enable follow-up jobs.
func fetchSourcePolls(t *testing.T, s *Session, source release.Source, kinds ...pollKind) error {
	t.Helper()
	var errs []error
	for _, kind := range kinds {
		for _, job := range s.pollJobs(source) {
			if job.kind == kind {
				ctx, usage := githubread.Track(t.Context(), 80)
				errs = append(errs, s.fetchPoll(ctx, s.generation, s.workRevision, source, job, false, usage))
				if kind == pollChecks {
					drainReadyPRRetries(t, s)
				}
				break
			}
		}
	}
	return errors.Join(errs...)
}

type workServices struct {
	*fixture
	plans  []release.Target
	images int
}

func (f *workServices) PlanRetries(ctx context.Context, target release.Target) (release.RetryPlan, error) {
	plan, err := f.Plan(ctx, target)
	return plan.RetryPlan(), err
}

func (f *workServices) Plan(_ context.Context, target release.Target) (release.Plan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plans = append(f.plans, target)
	return release.Plan{Target: target}, nil
}

func (f *workServices) Inspect(ctx context.Context, c release.Config, versions map[string]string) (release.Inventory, error) {
	f.mu.Lock()
	f.images++
	f.mu.Unlock()
	return f.fixture.Inspect(ctx, c, versions)
}

func TestScheduledPRWorkDoesNotRequireComponentTags(t *testing.T) {
	s, f := newFixture()
	apply(t, s, Action{Kind: "configure", Config: configuration()})
	services := &workServices{fixture: f}
	s.planner, s.images = services, services
	fact := f.repos["fact"]
	first := release.Target{Repo: fact.Source.Repo, PR: 1}
	second := release.Target{Repo: fact.Source.Repo, PR: 2}
	fact.PRs = []release.PR{{Target: first, SHA: "one", Base: fact.Source.Branch, State: "open", Evidence: observed()}, {Target: second, SHA: "two", Base: fact.Source.Branch, State: "open", Evidence: observed()}}
	fact.Tags = nil
	f.repos["fact"] = fact
	if err := fetchSourcePolls(t, s, fact.Source, pollPRs); err != nil {
		t.Fatal(err)
	}
	if len(s.Snapshot().Repositories[0].PRs) != 2 || len(services.plans) != 0 {
		t.Fatal("PRs were hidden behind component tags or undecided PRs received plans")
	}
	for target, choice := range map[release.Target]release.Choice{first: release.Included, second: release.Excluded} {
		apply(t, s, Action{Kind: "choice", Target: target, Choice: choice})
	}
	if err := fetchSourcePolls(t, s, fact.Source, pollChecks, pollBuilds); err != nil {
		t.Fatal(err)
	}
	request := f.requests[len(f.requests)-1]
	if request.Reads(ReadPRs) || request.Reads(ReadTags) || !slices.Equal(request.Tracked, []release.Target{first}) || !slices.Contains(request.Ignored, second) || slices.Contains(services.plans, second) || !slices.Contains(services.plans, first) {
		t.Fatalf("unexpected active work: request=%+v plans=%v", request, services.plans)
	}
	if len(s.Snapshot().Choices.Tags) != 0 {
		t.Fatal("PR work selected a component tag")
	}
}

func TestScheduledDiscoveryContinuesAfterTagSelectionAndPinning(t *testing.T) {
	s, f, _ := rcFixture(t)
	acs := f.repos["acs"]
	old := release.Target{Repo: acs.Source.Repo, PR: 1}
	acs.PRs = []release.PR{{Target: old, SHA: "old-head", State: "open", Base: acs.Source.Branch, Evidence: observed()}}
	f.repos["acs"] = acs
	if err := fetchSourcePolls(t, s, acs.Source, pollPRs); err != nil {
		t.Fatal(err)
	}
	apply(t, s, Action{Kind: "choice", Target: old, Choice: release.Included})
	acs.PRs[0].Merged, acs.PRs[0].State, acs.PRs[0].MergeSHA = true, "merged", "merge-one"
	acs.PRs = append(acs.PRs, release.PR{Target: release.Target{Repo: acs.Source.Repo, PR: 2}, SHA: "new-head", State: "open", Base: acs.Source.Branch, Evidence: observed()})
	f.repos["acs"] = acs
	if err := fetchSourcePolls(t, s, acs.Source, pollPRs); err != nil {
		t.Fatal(err)
	}
	observedACS := s.repos["acs"]
	if len(observedACS.PRs) != 2 || !observedACS.PRs[0].Merged {
		t.Fatalf("pinning stopped ordinary ACS discovery: %+v", observedACS.PRs)
	}
	fact := f.repos["fact"]
	selected := s.choices.Tags["fact"]
	fact.Tags = append(fact.Tags, release.Tag{Name: "0.3.78", SHA: fact.Head, Belongs: release.Yes, Evidence: observed()})
	f.repos["fact"] = fact
	if err := fetchSourcePolls(t, s, fact.Source, pollTags); err != nil {
		t.Fatal(err)
	}
	if len(s.repos["fact"].Tags) != 2 || s.choices.Tags["fact"] != selected {
		t.Fatal("new tags were hidden or silently selected")
	}
}

func TestChoiceChangesPreserveUnrelatedPlans(t *testing.T) {
	s, f := newFixture()
	apply(t, s, Action{Kind: "configure", Config: configuration()})
	refreshAll(t, s)
	fact := f.repos["fact"]
	selected := release.Target{Repo: fact.Source.Repo, Tag: fact.Tags[0].Name}
	unrelated := release.Target{Repo: "stackrox/collector", PR: 8}
	plan := release.Plan{Target: unrelated, SHA: "unrelated", Evidence: observed(), Proposals: []release.Proposal{{Target: unrelated, SHA: "unrelated", Job: "build", RunID: "run", Payload: "/retest build"}}}
	s.plans[unrelated] = plan
	apply(t, s, Action{Kind: "tag", Component: "fact", Tag: selected.Tag, SHA: fact.Tags[0].SHA})
	if !s.plans[unrelated].Evidence.Fresh() || len(s.plans[unrelated].Proposals) != 1 {
		t.Fatal("tag selection suspended unrelated PR retries")
	}
	apply(t, s, Action{Kind: "clear-tag", Component: "fact"})
	if s.choices.Tags["fact"] != "" || !s.tagInitialized["fact"] || len(s.history) != 1 || !s.plans[unrelated].Evidence.Fresh() {
		t.Fatal("preparing a new tag did not isolate the component selection")
	}
	if len(s.dispatcher.Views()) == 0 || !s.dispatcher.Views()[0].Ignored {
		t.Fatal("cleared tag retained retry authority")
	}
}

func TestScopedMergeRetainsUnreadSectionsWithoutRefreshingTheirEvidence(t *testing.T) {
	c := configuration()
	tag := release.Tag{Name: "0.3.77", SHA: "tag", Belongs: release.Yes, Evidence: observed()}
	old := release.Repository{Source: c.Components[0], Head: "head", Tags: []release.Tag{tag}, Pins: release.Pins{Evidence: observed()}}
	latest := release.Repository{Source: old.Source, Head: "head", Evidence: observed(), PRs: []release.PR{{Target: release.Target{Repo: old.Source.Repo, PR: 1}, SHA: "pr-head", Evidence: observed()}}}
	merged := mergeDiscovery(old, latest, DiscoveryRequest{Scope: ReadPRs})
	if len(merged.Tags) != 1 || merged.Tags[0].SHA != "tag" || !merged.Tags[0].Evidence.At.Equal(tag.Evidence.At) || !merged.Pins.Evidence.At.Equal(old.Pins.Evidence.At) {
		t.Fatal("unread sections were erased or stamped as new")
	}
	latest.Head = "changed-head"
	merged = mergeDiscovery(old, latest, DiscoveryRequest{Scope: ReadPRs})
	if merged.Tags[0].Evidence.Fresh() || merged.Pins.Evidence.Fresh() || !merged.TagsPending {
		t.Fatal("branch changes retained stale ancestry or pin evidence")
	}
}

type tagFailureServices struct{ *fixture }

func (f tagFailureServices) Discover(ctx context.Context, request DiscoveryRequest) (release.Repository, error) {
	if request.Scope == ReadTags {
		return release.Repository{Source: request.Source}, errors.New("tag inventory unavailable")
	}
	return f.fixture.Discover(ctx, request)
}

func TestTagInventoryFailurePreservesIndependentPRChecks(t *testing.T) {
	s, f := newFixture()
	apply(t, s, Action{Kind: "configure", Config: configuration()})
	fact := f.repos["fact"]
	target := release.Target{Repo: fact.Source.Repo, PR: 1}
	fact.PRs = []release.PR{{Target: target, SHA: "pr-head", State: "open", Base: fact.Source.Branch, Evidence: observed(), ChecksEvidence: observed(), Checks: []release.Check{{Name: "build", Status: "success", Required: release.Yes}}}}
	f.repos["fact"] = fact
	s.discovery = tagFailureServices{fixture: f}
	s.choices.PRs[target] = release.Included
	if err := fetchSourcePolls(t, s, fact.Source, pollPRs, pollChecks, pollTags); err == nil {
		t.Fatal("tag inventory failure was hidden")
	}
	pr := s.repos["fact"].PRs[0]
	if !s.repos["fact"].Evidence.Fresh() || !pr.Evidence.Fresh() || pr.ChecksStatus() != release.Yes {
		t.Fatalf("tag inventory failure invalidated independent PR evidence: %+v", s.repos["fact"])
	}
}

type truncatedInventory struct{ *fixture }

func (f truncatedInventory) Discover(ctx context.Context, request DiscoveryRequest) (release.Repository, error) {
	repo, err := f.fixture.Discover(ctx, request)
	if err == nil && request.Scope == ReadTags {
		repo.Evidence.Error = "GitHub pagination limit reached"
		return repo, errors.New(repo.Evidence.Error)
	}
	return repo, err
}

func TestScopedRefreshPreservesTagInventoryCompleteness(t *testing.T) {
	s, f := newFixture()
	apply(t, s, Action{Kind: "configure", Config: configuration()})
	fact := f.repos["fact"]
	fact.Tags = nil
	f.repos["fact"] = fact
	s.discovery = truncatedInventory{f}
	if err := fetchSourcePolls(t, s, fact.Source, pollPRs, pollTags); err == nil {
		t.Fatal("tag inventory truncation was hidden")
	}
	if repo := s.repos["fact"]; !repo.Evidence.Fresh() || !repo.TagsPending {
		t.Fatalf("expected fresh branch evidence and an incomplete tag inventory: %+v", repo)
	}
	if err := fetchSourcePolls(t, s, fact.Source, pollPRs); err != nil {
		t.Fatal(err)
	}
	if !s.repos["fact"].TagsPending {
		t.Fatal("a PR refresh marked the tag inventory complete")
	}
	s.discovery = f
	if err := fetchSourcePolls(t, s, fact.Source, pollTags); err != nil {
		t.Fatal(err)
	}
	if s.repos["fact"].TagsPending {
		t.Fatal("a successful tag refresh left the inventory pending")
	}
}

func TestDiscoveryLogsNewWorkAndMergesOnce(t *testing.T) {
	s, f := newFixture()
	apply(t, s, Action{Kind: "configure", Config: configuration()})
	var output bytes.Buffer
	s.log = NewLog(&output, false)
	source := configuration().Components[0]
	if err := fetchSourcePolls(t, s, source, pollPRs); err != nil {
		t.Fatal(err)
	}
	fact := f.repos["fact"]
	fact.PRs = []release.PR{{Target: release.Target{Repo: source.Repo, PR: 1}, SHA: "pr-head", Title: "Fix release build", Base: source.Branch, State: "open", Evidence: observed()}}
	f.repos["fact"] = fact
	if err := fetchSourcePolls(t, s, source, pollPRs); err != nil {
		t.Fatal(err)
	}
	apply(t, s, Action{Kind: "choice", Target: fact.PRs[0].Target, Choice: release.Included})
	fact.PRs[0].Merged, fact.PRs[0].State, fact.PRs[0].MergeSHA = true, "merged", "merge-one"
	f.repos["fact"] = fact
	for range 2 {
		if err := fetchSourcePolls(t, s, source, pollPRs); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{
		"Found pull request fact#1 targeting release-0.3: Fix release build.",
		"Pull request fact#1 merged into release-0.3 at merge commit merge-one.",
	} {
		if strings.Count(output.String(), want) != 1 {
			t.Fatalf("expected one readable transition %q: %s", want, output.String())
		}
	}
}

func TestPRInventoryRetainsChecksOnlyForVerifiedOpenRevisions(t *testing.T) {
	source := configuration().ACS()
	pr := release.PR{Target: release.Target{Repo: source.Repo, PR: 42}, Base: source.Branch, State: "open", SHA: "head",
		Evidence: observed(), ChecksEvidence: observed(), CheckResultsEvidence: observed(), Checks: []release.Check{{Name: "build"}}}
	for name, change := range map[string]func(*release.PR){
		"same revision":       func(*release.PR) {},
		"different head":      func(p *release.PR) { p.SHA = "second" },
		"different base":      func(p *release.PR) { p.Base = "master" },
		"closed PR":           func(p *release.PR) { p.State = "closed" },
		"unverified identity": func(p *release.PR) { p.Evidence.Known = false },
	} {
		t.Run(name, func(t *testing.T) {
			fresh := pr
			change(&fresh)
			old := release.Repository{Source: source, PRs: []release.PR{pr}, PinningPR: &pr, PinningEvidence: observed()}
			latest := release.Repository{Source: source, PRs: []release.PR{fresh}, PinningPR: &fresh, PinningEvidence: observed()}
			got := mergeDiscovery(old, latest, DiscoveryRequest{Source: source, Scope: ReadPRs | ReadPins})
			for _, p := range []release.PR{got.PRs[0], *got.PinningPR} {
				if name == "same revision" {
					if len(p.Checks) != 1 || p.ChecksEvidence != pr.ChecksEvidence || p.CheckResultsEvidence != pr.CheckResultsEvidence {
						t.Fatalf("lost checks for unchanged revision: %+v", p)
					}
				} else if len(p.Checks) != 0 || p.ChecksEvidence.Fresh() || p.CheckResultsEvidence.Fresh() {
					t.Fatalf("retained checks for changed revision: %+v", p)
				}
			}
		})
	}
}
