package app

import (
	"context"
	"errors"
	"slices"
	"testing"

	"release-assistant/internal/dispatch"
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

type rcCheckFixture struct {
	*fixture
	validation release.Validation
}

func (f *rcCheckFixture) ReadRCChecks(context.Context, release.Target) release.Validation {
	return f.validation
}

func TestRCPlanBindsValidationToTheCandidate(t *testing.T) {
	target := configuration().RC0Target()
	for name, tc := range map[string]struct {
		planSHA, validationSHA, wantSHA string
		plannerErr, wantErr             bool
	}{
		"matching identity keeps validation":                {"candidate", "candidate", "candidate", false, false},
		"moved tag discards both observations":              {"old", "new", "", false, true},
		"independent validation survives a planner timeout": {"", "candidate", "candidate", true, true},
	} {
		t.Run(name, func(t *testing.T) {
			s, f, _ := rcFixture(t)
			s.discovery = &rcCheckFixture{fixture: f, validation: release.Validation{
				SHA: tc.validationSHA, Evidence: observed(),
				Checks: []release.ValidationCheck{{Check: release.Check{Name: "unit tests", Status: "in_progress"}}},
			}}
			planner := &rcPlanner{plan: release.Plan{Target: target, SHA: tc.planSHA, Evidence: observed()}}
			if tc.plannerErr {
				planner.err = context.DeadlineExceeded
			}
			s.planner = planner
			plan, err := s.readRCPlan(t.Context(), target)
			if plan.SHA != tc.wantSHA || (err != nil) != tc.wantErr {
				t.Fatalf("plan SHA=%q error=%v, want SHA=%q error=%t", plan.SHA, err, tc.wantSHA, tc.wantErr)
			}
			if (len(plan.Validation.Checks) != 0) != (tc.wantSHA != "") {
				t.Fatalf("unexpected validation: %+v", plan.Validation)
			}
		})
	}
}

type rcRetryTransport struct {
	*rcPlanner
	posted []release.Proposal
}

func (tr *rcRetryTransport) ReadTarget(_ context.Context, target release.Target) (dispatch.Identity, error) {
	return dispatch.Identity{Target: target, SHA: tr.plan.SHA, Open: true, Evidence: observed()}, nil
}

func (tr *rcRetryTransport) Write(_ context.Context, proposal release.Proposal) (dispatch.Delivery, error) {
	tr.posted = append(tr.posted, proposal)
	return dispatch.Delivery{Status: "sent"}, nil
}

func TestRCValidationRetryRespectsWriteAndConfirmationModes(t *testing.T) {
	for name, mode := range map[string]struct{ writes, interactive bool }{
		"read-only": {}, "interactive": {true, true}, "automatic": {true, false},
	} {
		t.Run(name, func(t *testing.T) {
			s, _, _ := rcFixture(t)
			config := s.Snapshot().Config
			target := config.RC0Target()
			proposal := release.Proposal{
				Target: target, SHA: "candidate", Job: "checks", RunID: "51",
				Payload: "/retest checks tag:" + target.Tag,
			}
			tr := &rcRetryTransport{rcPlanner: &rcPlanner{plan: release.Plan{
				Target: target, SHA: proposal.SHA, Evidence: observed(),
				Runs:      []release.Run{{Job: proposal.Job, ID: proposal.RunID, Status: "completed", Outcome: "cancelled", Eligible: true}},
				Proposals: []release.Proposal{proposal},
			}}}
			s.planner = tr
			s.dispatcher = dispatch.New(tr, tr, tr, dispatch.Options{EnableWrites: mode.writes})
			generation := s.Snapshot().Generation
			refresh := func(discover bool) {
				t.Helper()
				if err := s.refreshPlanned(t.Context(), generation, config.ACS(), discover); err != nil {
					t.Fatal(err)
				}
			}
			refresh(true)
			refresh(false)
			grant := Action{Kind: "grant", Target: target, SHA: proposal.SHA, Enabled: true, Generation: generation}
			if err := s.Apply(t.Context(), grant); (err == nil) != mode.writes {
				t.Fatalf("unexpected permission result %v", err)
			}
			apply(t, s, Action{Kind: "interactive", Enabled: mode.interactive})
			refresh(false)
			if mode.interactive {
				if len(tr.posted) != 0 {
					t.Fatal("posted before an interactive confirmation")
				}
				apply(t, s, Action{Kind: "send", Target: target, ID: dispatch.ID(proposal)})
			}
			if mode.writes {
				if !slices.Equal(tr.posted, []release.Proposal{proposal}) {
					t.Fatalf("expected exactly the authorized tag comment, got %v", tr.posted)
				}
			} else if len(tr.posted) != 0 {
				t.Fatal("read-only mode posted a retry")
			}
			tr.plan.Proposals = nil
			tr.plan.Runs[0] = release.Run{Job: proposal.Job, ID: "52", Status: "in_progress"}
			refresh(false)
			for _, view := range s.Snapshot().Authority {
				if view.Target == target && len(view.Offers) != 0 {
					t.Fatal("a running rerun retained a retry offer")
				}
			}
		})
	}
}
