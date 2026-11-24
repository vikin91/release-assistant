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

func (p *rcPlanner) PlanRetries(ctx context.Context, target release.Target) (release.RetryPlan, error) {
	plan, err := p.Plan(ctx, target)
	return plan.RetryPlan(), err
}

func (p *rcPlanner) Plan(_ context.Context, target release.Target) (release.Plan, error) {
	p.calls = append(p.calls, target)
	return p.plan, p.err
}

func TestRCBuildChecksAndRetryAuthority(t *testing.T) {
	s, f, _ := rcFixture(t)
	snapshot := s.Snapshot()
	target := f.repos["acs"].Candidate.Target
	proposal := release.Proposal{Target: target, SHA: "rc-commit", RunID: "17", Job: "main-on-push"}
	planner := &rcPlanner{plan: release.Plan{
		Target: target, SHA: proposal.SHA, Evidence: observed(),
		Runs:      []release.Run{{Job: proposal.Job, ID: proposal.RunID, Eligible: true}},
		Proposals: []release.Proposal{proposal, {Target: target, SHA: proposal.SHA, Job: "roxctl-on-push"}},
	}}
	s.planner = planner
	if err := fetchSourcePolls(t, s, snapshot.Config.ACS(), pollTags, pollPRs, pollBuilds); err != nil {
		t.Fatal(err)
	}
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
	if err := fetchSourcePolls(t, s, snapshot.Config.ACS(), pollBuilds); !errors.Is(err, planner.err) {
		t.Fatalf("build poll error = %v, want %v", err, planner.err)
	}
	for _, view := range s.Snapshot().Authority {
		if view.Target == target && (view.Granted || len(view.Offers) != 0) {
			t.Fatalf("unresolved RC retained write authority: %+v", view)
		}
	}
	apply(t, s, Action{Kind: "clear-tag", Component: "fact"})
	if !slices.ContainsFunc(s.Snapshot().Plans, func(plan release.Plan) bool { return plan.Target == target }) {
		t.Fatal("clearing a component tag stopped RC monitoring")
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
	acs.Candidate = release.Candidate{
		Target: release.Target{Repo: c.ACS().Repo, Tag: c.Version + "-rc.2"},
		SHA:    "rc-commit", Number: 2, Evidence: observed(),
	}
	f.repos["acs"] = acs
	inspector := &phaseImages{fixture: f}
	s.images = inspector
	return s, f, inspector
}

func TestRCMonitoringStartsOnDiscoveryWithoutPinning(t *testing.T) {
	s, f, inspector := rcFixture(t)
	c := s.config
	c.PinningPR = 0
	apply(t, s, Action{Kind: "configure", Config: c})
	acs := f.repos["acs"]
	acs.PinningPR = nil
	acs.Pins = release.Pins{}
	f.repos["acs"] = acs
	rc := acs.Candidate
	planner := &rcPlanner{plan: release.Plan{Target: rc.Target, SHA: rc.SHA, Evidence: observed()}}
	s.planner = planner
	if err := fetchSourcePolls(t, s, c.ACS(), pollTags, pollPRs, pollBuilds, pollImages); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(planner.calls, rc.Target) || inspector.release != 1 || s.Snapshot().Inventory.Versions["acs"] != rc.Target.Tag {
		t.Fatalf("first discovery did not monitor %s: plans=%v images=%d", rc.Target, planner.calls, inspector.release)
	}
	if err := s.Apply(t.Context(), Action{Generation: s.generation, Kind: "grant", Target: rc.Target, SHA: rc.SHA, Enabled: true}); err == nil {
		t.Fatal("monitoring granted retry access without verified pins")
	}
}

func TestRCMonitoringRequiresCurrentTagAndCommit(t *testing.T) {
	for name, invalidate := range map[string]func(*release.Candidate){
		"missing tag":    func(rc *release.Candidate) { rc.Target.Tag = "" },
		"missing commit": func(rc *release.Candidate) { rc.SHA = "" },
		"stale evidence": func(rc *release.Candidate) { rc.Evidence.Known = false },
	} {
		t.Run(name, func(t *testing.T) {
			s, f, inspector := rcFixture(t)
			if err := fetchSourcePolls(t, s, s.config.ACS(), pollTags, pollPRs, pollBuilds, pollImages); err != nil {
				t.Fatal(err)
			}
			acs := f.repos["acs"]
			invalidate(&acs.Candidate)
			f.repos["acs"] = acs
			planner := &rcPlanner{}
			s.planner = planner
			if err := fetchSourcePolls(t, s, s.config.ACS(), pollTags, pollPRs, pollBuilds, pollImages); err != nil {
				t.Fatal(err)
			}
			if inspector.release != 1 || len(planner.calls) != 0 || s.Snapshot().RCImageWaitReason() == "" {
				t.Fatalf("invalid RC triggered monitoring: plans=%v images=%d", planner.calls, inspector.release)
			}
		})
	}
}

func TestRCResultsSurviveBranchRecheck(t *testing.T) {
	for name, change := range map[string]func(*release.Candidate){
		"should retain the same tag and commit": func(*release.Candidate) {},
		"should reset for a new tag":            func(rc *release.Candidate) { rc.Target.Tag = "5.0.1-rc.3" },
		"should reset for a moved tag":          func(rc *release.Candidate) { rc.SHA = "new-commit" },
	} {
		t.Run(name, func(t *testing.T) {
			s, f, _ := rcFixture(t)
			source := s.config.ACS()
			acs := f.repos[source.Name]
			rc := acs.Candidate
			s.discovery = tagCheckFixture{f, func(release.Target) (release.Validation, error) {
				return release.Validation{SHA: rc.SHA, Evidence: observed()}, nil
			}}
			s.planner = &rcPlanner{plan: release.Plan{
				Target: rc.Target, SHA: rc.SHA, Evidence: observed(),
				Runs: []release.Run{{Job: "main-on-push", ID: "17", Status: "completed", Outcome: "success"}},
			}}
			if err := fetchSourcePolls(t, s, source, pollTags, pollPRs, pollTagChecks, pollBuilds, pollImages); err != nil {
				t.Fatal(err)
			}
			before := s.Snapshot()
			apply(t, s, Action{Kind: "grant", Target: rc.Target, SHA: rc.SHA, Enabled: true})
			acs.Head = "advanced-branch-head"
			f.repos[source.Name] = acs
			if err := fetchSourcePolls(t, s, source, pollPRs, pollImages); err != nil {
				t.Fatal(err)
			}
			assertRetained := func(snapshot Snapshot) {
				t.Helper()
				candidate := snapshot.Candidate()
				if candidate.Target != rc.Target || candidate.SHA != rc.SHA ||
					len(before.Inventory.ReleaseImages) == 0 ||
					!slices.Equal(before.Inventory.ReleaseImages, snapshot.Inventory.ReleaseImages) ||
					!slices.ContainsFunc(snapshot.Plans, func(plan release.Plan) bool {
						return plan.Target == rc.Target && plan.SHA == rc.SHA && plan.Validation.Evidence.Fresh() &&
							len(plan.Runs) == 1 && plan.Runs[0].ID == "17"
					}) {
					t.Fatalf("lost the last observed RC results: candidate=%+v, images=%+v, plans=%+v",
						candidate, snapshot.Inventory.ReleaseImages, snapshot.Plans)
				}
			}
			pending := s.Snapshot()
			assertRetained(pending)
			if pending.Candidate().Evidence.Fresh() || pending.Candidate().Evidence.Error != "" ||
				slices.ContainsFunc(pending.Authority, func(view dispatch.View) bool { return view.Target == rc.Target && view.Granted }) {
				t.Fatal("the pending comparison must not establish current retry authority")
			}

			f.fail[source.Name] = true
			if err := fetchSourcePolls(t, s, source, pollTags); err == nil {
				t.Fatal("expected discovery failure")
			}
			failed := s.Snapshot()
			assertRetained(failed)
			if failed.Candidate().Evidence.Error == "" {
				t.Fatal("missing the discovery failure")
			}

			f.fail[source.Name] = false
			change(&acs.Candidate)
			acs.Candidate.Ahead = 1
			f.repos[source.Name] = acs
			if err := fetchSourcePolls(t, s, source, pollTags); err != nil {
				t.Fatal(err)
			}
			after := s.Snapshot()
			candidate := after.Candidate()
			if !candidate.Evidence.Fresh() || candidate.Ahead != 1 {
				t.Fatalf("missing the reverified branch comparison: %+v", candidate)
			}
			if candidate.Target == rc.Target && candidate.SHA == rc.SHA {
				assertRetained(after)
			} else if len(after.Inventory.ReleaseImages) != 0 || slices.ContainsFunc(after.Plans, func(plan release.Plan) bool {
				return plan.Target == rc.Target && plan.SHA == rc.SHA
			}) {
				t.Fatal("old results carried over to a different candidate")
			}
		})
	}
}

type rcCheckFixture struct {
	*fixture
	validation release.Validation
}

func (f *rcCheckFixture) ReadTagChecks(context.Context, release.Target) (release.Validation, error) {
	return f.validation, nil
}

func TestRCPlanBindsValidationToTheCandidate(t *testing.T) {
	target := release.Target{Repo: configuration().ACS().Repo, Tag: "5.0.1-rc.2"}
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
			s, f, _ := rcFixture(t)
			config := s.Snapshot().Config
			target := f.repos["acs"].Candidate.Target
			proposal := release.Proposal{
				Target: target, SHA: "rc-commit", Job: "checks", RunID: "51",
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
			refresh := func(kinds ...pollKind) {
				t.Helper()
				if err := fetchSourcePolls(t, s, config.ACS(), kinds...); err != nil {
					t.Fatal(err)
				}
			}
			refresh(pollTags, pollPRs, pollBuilds)
			refresh(pollBuilds)
			grant := Action{Kind: "grant", Target: target, SHA: proposal.SHA, Enabled: true, Generation: generation}
			if err := s.Apply(t.Context(), grant); (err == nil) != mode.writes {
				t.Fatalf("unexpected permission result %v", err)
			}
			apply(t, s, Action{Kind: "interactive", Enabled: mode.interactive})
			refresh(pollBuilds)
			if mode.interactive {
				if len(tr.posted) != 0 {
					t.Fatal("posted before an interactive confirmation")
				}
				apply(t, s, Action{Kind: "queue-send", QueueID: queueID(t, s, proposal)})
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
			refresh(pollBuilds)
			for _, view := range s.Snapshot().Authority {
				if view.Target == target && len(view.Offers) != 0 {
					t.Fatal("a running rerun retained a retry offer")
				}
			}
		})
	}
}

func TestNewCandidateReplacesImagesAndRevokesPreviousRetryAuthority(t *testing.T) {
	s, f, _ := rcFixture(t)
	config := s.config
	older := f.repos["acs"].Candidate
	proposal := release.Proposal{Target: older.Target, SHA: older.SHA, RunID: "17", Job: "main-on-push"}
	planner := &rcPlanner{plan: release.Plan{
		Target: older.Target, SHA: older.SHA, Evidence: observed(),
		Runs: []release.Run{{Job: proposal.Job, ID: proposal.RunID, Eligible: true}}, Proposals: []release.Proposal{proposal},
	}}
	s.planner = planner
	refresh := func() {
		t.Helper()
		if err := fetchSourcePolls(t, s, config.ACS(), pollTags, pollPRs, pollBuilds, pollImages); err != nil {
			t.Fatal(err)
		}
	}
	refresh()
	refresh()
	apply(t, s, Action{Kind: "grant", Target: older.Target, SHA: older.SHA, Enabled: true})
	id := queueID(t, s, proposal)

	acs := f.repos["acs"]
	acs.Candidate = release.Candidate{
		Target: release.Target{Repo: config.ACS().Repo, Tag: config.Version + "-rc.3"},
		SHA:    "new-candidate", Number: 3, Evidence: observed(),
	}
	f.repos["acs"] = acs
	planner.plan = release.Plan{Target: acs.Candidate.Target, SHA: acs.Candidate.SHA, Evidence: observed()}
	refresh()
	snapshot := s.Snapshot()
	if snapshot.Candidate().Target != acs.Candidate.Target || snapshot.Inventory.Versions["acs"] != acs.Candidate.Target.Tag ||
		len(snapshot.Inventory.ReleaseImages) == 0 {
		t.Fatalf("latest candidate and images did not advance together: %+v %+v", snapshot.Candidate(), snapshot.Inventory)
	}
	for _, plan := range snapshot.Plans {
		if plan.Target == older.Target {
			t.Fatal("previous candidate plan survived the new tag")
		}
	}
	err := s.Apply(t.Context(), Action{Kind: "queue-send", QueueID: id, Generation: snapshot.Generation})
	if err == nil {
		t.Fatal("accepted a retry for a superseded candidate")
	}
}
