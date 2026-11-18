package app

import (
	"context"
	"errors"
	"slices"
	"testing"

	"release-assistant/internal/release"
)

type phaseImages struct {
	*fixture
	components, release int
}

type rcPlanner struct {
	calls []release.Target
	plan  release.Plan
	err   error
}

func (p *rcPlanner) Plan(_ context.Context, target release.Target) (release.Plan, error) {
	p.calls = append(p.calls, target)
	return p.plan, p.err
}

func TestRCBuildChecksAndRetryAuthority(t *testing.T) {
	s, _, _ := rcFixture(t)
	snapshot := s.Snapshot()
	target := snapshot.Config.RC0Target()
	proposal := release.Proposal{Target: target, SHA: "rc-commit", RunID: "17", Job: "main-on-push"}
	planner := &rcPlanner{plan: release.Plan{
		Target: target, SHA: proposal.SHA, Evidence: observed(),
		Runs:      []release.Run{{Job: proposal.Job, ID: proposal.RunID, Eligible: true}},
		Proposals: []release.Proposal{proposal, {Target: target, SHA: proposal.SHA, Job: "roxctl-on-push"}},
	}}
	s.planner = planner
	refresh := func(discover bool) {
		t.Helper()
		if err := s.refreshPlanned(t.Context(), snapshot.Generation, snapshot.Config.ACS(), discover); err != nil {
			t.Fatal(err)
		}
	}
	refresh(true)
	if slices.Contains(planner.calls, target) {
		t.Fatal("read RC checks before pinning was verified")
	}
	refresh(false)
	if !slices.Equal(planner.calls, []release.Target{target}) {
		t.Fatalf("expected one check for %s, got %v", target, planner.calls)
	}
	apply(t, s, Action{Kind: "grant", Target: target, SHA: proposal.SHA, Enabled: true})
	found := false
	for _, view := range s.Snapshot().Authority {
		if view.Target != target {
			continue
		}
		found = true
		if !view.Granted || len(view.Offers) != 1 || view.Offers[0].Proposal != proposal {
			t.Fatalf("unexpected RC retry authority: %+v", view)
		}
	}
	if !found {
		t.Fatal("missing RC retry authority")
	}
	planner.plan = release.Plan{Target: target, Evidence: observed()}
	planner.err = errors.New("tag lookup failed")
	refresh(false)
	for _, view := range s.Snapshot().Authority {
		if view.Target == target && (view.Granted || len(view.Offers) != 0) {
			t.Fatalf("unresolved RC retained write authority: %+v", view)
		}
	}
	apply(t, s, Action{Kind: "clear-tag", Component: "fact"})
	if slices.ContainsFunc(s.Snapshot().Plans, func(plan release.Plan) bool { return plan.Target == target }) {
		t.Fatal("RC plan survived loss of a prerequisite")
	}
}

func (i *phaseImages) Inspect(ctx context.Context, c release.Config, tags map[string]string) (release.Inventory, error) {
	i.release++
	return i.fixture.Inspect(ctx, c, tags)
}
func (i *phaseImages) InspectComponents(ctx context.Context, c release.Config, tags map[string]string) (release.Inventory, error) {
	i.components++
	return i.fixture.Inspect(ctx, c, tags)
}

func rcFixture(t *testing.T) (*Session, *fixture, *phaseImages) {
	t.Helper()
	s, f := newFixture()
	c := configuration()
	c.PinningPR = 23958
	apply(t, s, Action{Kind: "configure", Config: c})
	refreshAll(t, s)
	for _, source := range c.Components {
		tag := f.repos[source.Name].Tags[0]
		apply(t, s, Action{Kind: "tag", Component: source.Name, Tag: tag.Name, SHA: tag.SHA})
	}
	acs := f.repos["acs"]
	acs.Pins.Versions = map[string]string{"fact": "0.3.77", "collector": "3.25.8"}
	acs.PinningPR = &release.PR{Target: c.PinningTarget(), Base: c.Branch, SHA: "pin-head", State: "closed", Merged: true, MergeSHA: "pin-merge", Evidence: observed()}
	acs.PinningEvidence = observed()
	acs.PinningPins = acs.Pins
	f.repos["acs"] = acs
	inspector := &phaseImages{fixture: f}
	s.images = inspector
	return s, f, inspector
}

func TestReleaseImagesWaitForVerifiedMergeAndMatchingBranchPins(t *testing.T) {
	for name, change := range map[string]func(*release.Repository){
		"unmerged":              func(r *release.Repository) { r.PinningPR.Merged = false; r.PinningPR.State = "open" },
		"wrong base":            func(r *release.Repository) { r.PinningPR.Base = "master" },
		"wrong PR":              func(r *release.Repository) { r.PinningPR.Target.PR++ },
		"missing merge SHA":     func(r *release.Repository) { r.PinningPR.MergeSHA = "" },
		"unknown PR metadata":   func(r *release.Repository) { r.PinningEvidence.Known = false },
		"unknown PR identity":   func(r *release.Repository) { r.PinningPR.Evidence.Known = false },
		"unknown pins":          func(r *release.Repository) { r.Pins.Evidence.Known = false },
		"different branch pins": func(r *release.Repository) { r.Pins.Versions["fact"] = "0.3.76" },
	} {
		t.Run(name, func(t *testing.T) {
			s, f, inspector := rcFixture(t)
			acs := f.repos["acs"]
			change(&acs)
			f.repos["acs"] = acs
			generation := s.Snapshot().Generation
			for range 2 {
				if err := s.refreshPlanned(t.Context(), generation, configuration().ACS(), true); err != nil {
					t.Fatal(err)
				}
			}
			if inspector.release != 0 || inspector.components == 0 || s.rcReady() {
				t.Fatalf("premature RC reads: release=%d components=%d ready=%t", inspector.release, inspector.components, s.rcReady())
			}
			reason := s.Snapshot().RCImageWaitReason()
			if reason == "" || (name == "unmerged" && reason != "Waiting for the pinning PR to merge.") {
				t.Fatalf("missing or incorrect image check prerequisite: %q", reason)
			}
		})
	}
}

func TestReleasePhaseRevalidatesPinsBeforeInspectingRC(t *testing.T) {
	s, f, inspector := rcFixture(t)
	generation := s.Snapshot().Generation
	if err := s.refreshPlanned(t.Context(), generation, configuration().ACS(), true); err != nil {
		t.Fatal(err)
	}
	if !s.rcReady() || inspector.release != 0 {
		t.Fatal("merge transition performed RC reads before the new phase")
	}
	if err := s.refreshPlanned(t.Context(), generation, configuration().ACS(), false); err != nil {
		t.Fatal(err)
	}
	if inspector.release != 1 {
		t.Fatalf("expected release image inspection, got %d", inspector.release)
	}
	// A gate that was true at scheduling time must not survive failed current metadata.
	f.fail["acs"] = true
	if err := s.refreshPlanned(t.Context(), generation, configuration().ACS(), false); err == nil {
		t.Fatal("expected discovery failure")
	}
	if inspector.release != 1 || s.rcReady() {
		t.Fatal("RC reads used pin metadata from before the failed revalidation")
	}
}
