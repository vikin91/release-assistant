package app

import (
	"context"
	"slices"
	"testing"

	"release-assistant/internal/release"
)

type workServices struct {
	*fixture
	plans  []release.Target
	images int
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

func TestScheduledWorkFollowsSelectionAndExcludesUnselectedPRs(t *testing.T) {
	s, f := newFixture()
	apply(t, s, Action{Kind: "configure", Config: configuration()})
	services := &workServices{fixture: f}
	s.planner, s.images = services, services
	fact := f.repos["fact"]
	first := release.Target{Repo: fact.Source.Repo, PR: 1}
	second := release.Target{Repo: fact.Source.Repo, PR: 2}
	fact.PRs = []release.PR{{Target: first, SHA: "one", Base: fact.Source.Branch, State: "open", Evidence: observed()}, {Target: second, SHA: "two", Base: fact.Source.Branch, State: "open", Evidence: observed()}}
	f.repos["fact"] = fact
	generation := s.Snapshot().Generation
	if err := s.Apply(t.Context(), Action{Kind: "finish-selection", Generation: generation}); err == nil {
		t.Fatal("finished selection without tags or PR inventories")
	}
	for _, source := range configuration().Components {
		s.loadTags(t.Context(), generation, source)
		tag := f.repos[source.Name].Tags[0]
		if err := s.Apply(t.Context(), Action{Kind: "tag", Generation: generation, Component: source.Name, Tag: tag.Name, SHA: tag.SHA}); err != nil {
			t.Fatal(err)
		}
	}
	if s.Snapshot().Phase != "prs" {
		t.Fatal("confirmed tags did not advance to PR selection")
	}
	for _, source := range append(configuration().Components, configuration().ACS()) {
		if err := s.refreshPlanned(t.Context(), generation, source, true); err != nil {
			t.Fatal(err)
		}
	}
	if len(services.plans) != 0 || services.images != 0 || !s.Snapshot().SelectionReady {
		t.Fatalf("premature build work: plans=%v images=%d", services.plans, services.images)
	}
	for target, choice := range map[release.Target]release.Choice{first: release.Included, second: release.Excluded} {
		if err := s.Apply(t.Context(), Action{Kind: "choice", Generation: generation, Target: target, Choice: choice}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Apply(t.Context(), Action{Kind: "finish-selection", Generation: generation}); err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().Phase != "builds" {
		t.Fatal("finished selection did not enable build monitoring")
	}
	if err := s.refreshPlanned(t.Context(), generation, fact.Source, true); err != nil {
		t.Fatal(err)
	}
	request := f.requests[len(f.requests)-1]
	if !slices.Equal(request.Tracked, []release.Target{first}) || !slices.Contains(request.Ignored, second) || slices.Contains(services.plans, second) || !slices.Contains(services.plans, first) {
		t.Fatalf("excluded PR was monitored: request=%+v plans=%v", request, services.plans)
	}
	if err := s.Apply(t.Context(), Action{Kind: "choice", Generation: generation, Target: first, Choice: release.Excluded}); err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().Phase != "prs" || s.Snapshot().Assessment.Ready {
		t.Fatal("scope changes did not reopen selection")
	}
	for _, plan := range s.Snapshot().Plans {
		if len(plan.Proposals) != 0 || plan.Evidence.Fresh() {
			t.Fatal("deferred build retained retry authority")
		}
	}
	if err := s.Apply(t.Context(), Action{Kind: "finish-selection", Generation: generation}); err != nil {
		t.Fatalf("zero included PRs must be a valid selection: %v", err)
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
	if merged.Tags[0].Evidence.Fresh() || merged.Pins.Evidence.Fresh() {
		t.Fatal("branch changes retained stale ancestry or pin evidence")
	}
}
