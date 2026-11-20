package app

import (
	"context"
	"errors"
	"slices"
	"testing"

	"release-assistant/internal/release"
)

type tagCheckFixture struct {
	*fixture
	read func(release.Target) (release.Validation, error)
}

func (f tagCheckFixture) ReadTagChecks(_ context.Context, target release.Target) (release.Validation, error) {
	return f.read(target)
}

func TestTagChecksFollowSelectedRevision(t *testing.T) {
	s, f, _ := rcFixture(t)
	source := s.config.Components[0]
	tag := s.repos[source.Name].Tags[0]
	target := release.Target{Repo: source.Repo, Tag: tag.Name}
	planner := &rcPlanner{}
	s.planner = planner
	attempts := 0
	check := release.ValidationCheck{Check: release.Check{Name: "component CI", Conclusion: "success"}}
	resolved := tag.SHA
	s.discovery = tagCheckFixture{f, func(got release.Target) (release.Validation, error) {
		attempts++
		if got != target {
			t.Fatalf("read tag %s, want %s", got, target)
		}
		return release.Validation{SHA: resolved, Evidence: observed(), Checks: []release.ValidationCheck{check}}, nil
	}}
	refresh := func() (release.Validation, error) {
		t.Helper()
		err := s.refreshTagChecks(t.Context(), s.generation, s.workRevision, source)
		for _, plan := range s.Snapshot().Plans {
			if plan.Target == target {
				return plan.Validation, err
			}
		}
		return release.Validation{}, err
	}
	val, err := refresh()
	if err != nil || !val.Evidence.Fresh() || !slices.Equal(val.Checks, []release.ValidationCheck{check}) {
		t.Fatalf("tag checks = %+v, err = %v", val, err)
	}
	if len(planner.calls) > 0 || !s.plans[target].Validation.Evidence.At.IsZero() {
		t.Fatal("reading tag checks invoked the planner or published validation to the build plan")
	}
	resolved = "different-commit"
	val, err = refresh()
	if err == nil || val.Evidence.Fresh() || len(val.Checks) != 0 || val.Evidence.Error == "" {
		t.Fatalf("tag that moved during check lookup supplied results: %+v, err = %v", val, err)
	}
	apply(t, s, Action{Kind: "clear-tag", Component: source.Name})
	val, err = refresh()
	if attempts != 2 || err != nil || len(val.Checks) != 0 {
		t.Fatal("checks remained active for a cleared tag")
	}
}

func TestRCValidationSurvivesIndependentBuildRefresh(t *testing.T) {
	s, f, _ := rcFixture(t)
	acs := s.config.ACS()
	repo := f.repos["acs"]
	rc := repo.Candidate
	s.repos[acs.Name] = repo
	var checkReads int
	s.discovery = tagCheckFixture{f, func(target release.Target) (release.Validation, error) {
		checkReads++
		if target != rc.Target {
			t.Fatalf("unexpected RC target %v", target)
		}
		return release.Validation{SHA: rc.SHA, Evidence: observed()}, nil
	}}
	planner := &rcPlanner{plan: release.Plan{Target: rc.Target, SHA: rc.SHA, Evidence: observed()}}
	s.planner = planner
	if err := s.refreshTagChecks(t.Context(), s.generation, s.workRevision, acs); err != nil {
		t.Fatal(err)
	}
	if err := s.refresh(t.Context(), s.generation, acs, false, sourceWork{scope: ReadTargets, plans: true, scheduled: true, revision: s.workRevision}); err != nil {
		t.Fatal(err)
	}
	snap := s.Snapshot()
	index := slices.IndexFunc(snap.Plans, func(plan release.Plan) bool { return plan.Target == rc.Target })
	if index < 0 || !snap.Plans[index].Validation.Evidence.Fresh() || checkReads != 1 || !slices.Equal(planner.calls, []release.Target{rc.Target}) {
		t.Fatalf("plans = %+v, GitHub reads = %d, planner calls = %+v", snap.Plans, checkReads, planner.calls)
	}
}

func TestManualPRAndTagChecksKeepIndependentReports(t *testing.T) {
	s, _, _ := rcFixture(t)
	source := s.config.Components[0]
	repo := s.repos[source.Name]
	pr := release.PR{Target: release.Target{Repo: source.Repo, PR: 99}, Evidence: observed(), SHA: "pr-head", State: "open", Base: source.Branch}
	repo.PRs = []release.PR{pr}
	s.repos[source.Name] = repo
	apply(t, s, Action{Kind: "choice", Target: pr.Target, Choice: release.Included})
	tag, sha := s.tagCheckTarget(repo)
	var calls []release.Target
	readErr := errors.New("Actions unavailable")
	s.options.ManualRetries = manualReaderFunc(func(_ context.Context, target release.Target) (release.ManualRetryReport, error) {
		calls = append(calls, target)
		report := release.ManualRetryReport{Target: target, SHA: sha, Jobs: []release.ManualJob{{ID: 42}}}
		if target == pr.Target {
			report.SHA = pr.SHA
			return report, nil
		}
		return report, readErr
	})
	if err := s.refreshPRRetries(t.Context(), s.generation, s.workRevision, source); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(calls, []release.Target{pr.Target}) {
		t.Fatalf("PR refresh also read a tag: %v", calls)
	}
	calls = nil
	if err := s.refreshTagChecks(t.Context(), s.generation, s.workRevision, source); !errors.Is(err, readErr) {
		t.Fatalf("tag error = %v, want %v", err, readErr)
	}
	if !slices.Equal(calls, []release.Target{tag}) {
		t.Fatalf("tag refresh also read a PR: %v", calls)
	}
	if got := s.Snapshot().ManualRetries; len(got) != 2 {
		t.Fatalf("missing independently refreshed report: %+v", got)
	}
}
