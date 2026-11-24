package app

import (
	"context"
	"errors"
	"slices"
	"testing"

	"release-assistant/internal/release"
)

type manualReaderFunc func(context.Context, release.Target) (release.ManualRetryReport, error)

func (f manualReaderFunc) PlanRetries(ctx context.Context, target release.Target) (release.RetryPlan, error) {
	report, err := f(ctx, target)
	return report.RetryPlan(), err
}

func TestMonitorManualRetries(t *testing.T) {
	s, f := newFixture()
	s.config = configuration()
	source := s.config.Components[0]
	pr := release.Target{Repo: source.Repo, PR: 1}
	tag := f.repos[source.Name].Tags[0]
	selectedTag := release.Target{Repo: source.Repo, Tag: tag.Name}
	repo := f.repos[source.Name]
	repo.PRs = []release.PR{{Target: pr, SHA: "reviewed", Base: source.Branch, State: "open", Evidence: observed()}}
	f.repos[source.Name] = repo
	apply(t, s, Action{Kind: "interactive", Enabled: false})
	if err := s.Refresh(t.Context(), s.generation, source, true); err != nil {
		t.Fatal(err)
	}
	apply(t, s, Action{Kind: "choice", Target: pr, Choice: release.Included})
	apply(t, s, Action{Kind: "tag", Component: source.Name, Tag: tag.Name, SHA: tag.SHA})
	apply(t, s, Action{Kind: "grant", Target: pr, SHA: "reviewed", Enabled: true})
	calls := make(map[release.Target]int)
	reportSHA := "reviewed"
	s.options.CIPlanner = manualReaderFunc(func(_ context.Context, target release.Target) (release.ManualRetryReport, error) {
		calls[target]++
		if target == selectedTag {
			return release.ManualRetryReport{Target: target, SHA: tag.SHA, Jobs: []release.ManualJob{{ID: 42}}}, nil
		}
		return release.ManualRetryReport{Target: target, SHA: reportSHA, Prow: []release.ProwRetry{{Context: "ci/prow/test", Command: "/test test"}}}, errors.New("Actions jobs unavailable")
	})
	refresh := func() Snapshot {
		t.Helper()
		if err := fetchSourcePolls(t, s, source, pollChecks, pollTagChecks); err == nil {
			t.Fatal("the failed PR retry read should schedule a retry")
		}
		return s.Snapshot()
	}
	snap := refresh()
	if len(calls) != 2 || calls[pr] != 1 || calls[selectedTag] != 1 || len(snap.ManualRetries) != 2 {
		t.Fatalf("selected targets %v, reports %+v", calls, snap.ManualRetries)
	}
	if got := snap.ManualRetries[0]; got.Target != pr || !slices.Equal(got.Prow, []release.ProwRetry{{Context: "ci/prow/test", Command: "/test test"}}) || !slices.Contains(got.Warnings, "Actions jobs unavailable") {
		t.Fatalf("partial report: %+v", got)
	}
	for _, v := range snap.Authority {
		if len(v.Offers) > 0 {
			t.Fatalf("manual results reached dispatcher: %+v", v)
		}
	}
	for _, e := range snap.LogEntries {
		if e.Decision == "dispatch" {
			t.Fatalf("manual retry caused a write attempt: %+v", e)
		}
	}
	reportSHA = "moved"
	if got := refresh().ManualRetries[0]; len(got.Jobs)+len(got.Prow) != 0 || len(got.Warnings) == 0 {
		t.Fatalf("displayed instructions for changed revision: %+v", got)
	}
	apply(t, s, Action{Kind: "choice", Target: pr, Choice: release.Excluded})
	apply(t, s, Action{Kind: "clear-tag", Component: source.Name})
	if got := s.Snapshot().ManualRetries; len(got) != 0 {
		t.Fatalf("deselected targets: %+v", got)
	}
}

func TestACSManualRetryProgress(t *testing.T) {
	s, f := newFixture()
	s.config = configuration()
	source := s.config.ACS()
	repo := f.repos[source.Name]
	target := release.Target{Repo: source.Repo, PR: 1}
	pr := release.PR{Target: target, SHA: "head", Base: source.Branch, State: "open", Evidence: observed()}
	repo.PRs = []release.PR{pr}
	s.repos[source.Name] = repo
	s.choices.PRs[target] = release.Included
	for _, src := range append(slices.Clone(s.config.Components), source) {
		s.initiallyLoaded[src.Name] = true
	}
	report := func() release.ManualRetryReport {
		t.Helper()
		snap := s.Snapshot()
		if len(snap.ManualRetries) != 1 || snap.ManualRetries[0].Target != target {
			t.Fatalf("expected one ACS PR report, got %+v", snap.ManualRetries)
		}
		got := snap.ManualRetries[0]
		if snap.Loading != got.Pending {
			t.Fatalf("dashboard loading = %t for report %+v", snap.Loading, got)
		}
		return got
	}
	readErr := errors.New("GitHub read deferred")
	s.options.CIPlanner = manualReaderFunc(func(context.Context, release.Target) (release.ManualRetryReport, error) {
		if readErr != nil && !report().Pending {
			t.Fatal("first read must remain pending until it returns")
		}
		return release.ManualRetryReport{Target: target, SHA: pr.SHA}, readErr
	})
	s.refreshManualRetries(t.Context(), s.generation, s.workRevision, source)
	if got := report(); got.Pending || !got.Incomplete || len(got.Warnings) == 0 {
		t.Fatalf("deferred read: %+v", got)
	}
	readErr = nil
	s.refreshManualRetries(t.Context(), s.generation, s.workRevision, source)
	if got := report(); got.Pending || got.Incomplete || len(got.Warnings) > 0 {
		t.Fatalf("completed empty read: %+v", got)
	}
	repo.PRs[0].SHA = "new-head"
	if got := report(); got.SHA != "new-head" || !got.Pending {
		t.Fatalf("new revision: %+v", got)
	}
	repo.PRs[0].CheckResultsEvidence = observed()
	if got := s.Snapshot(); got.Loading || len(got.ManualRetries) != 0 {
		t.Fatal("a PR without failed checks should not wait for retry collection")
	}
}

func TestManualRetriesFollowCurrentRC(t *testing.T) {
	s, f, _ := rcFixture(t)
	source := s.config.ACS()
	rc := f.repos[source.Name].Candidate
	s.options.CIPlanner = manualReaderFunc(func(_ context.Context, target release.Target) (release.ManualRetryReport, error) {
		if target != rc.Target {
			t.Fatalf("unexpected manual target %s", target)
		}
		return release.ManualRetryReport{Target: target, SHA: rc.SHA, Jobs: []release.ManualJob{{ID: 88}}}, nil
	})
	if err := fetchSourcePolls(t, s, source, pollTags, pollTagChecks); err != nil {
		t.Fatal(err)
	}
	if got := s.Snapshot().ManualRetries; len(got) != 1 || got[0].Jobs[0].ID != 88 {
		t.Fatalf("missing RC job: %+v", got)
	}
	s.options.CIPlanner = manualReaderFunc(func(_ context.Context, target release.Target) (release.ManualRetryReport, error) {
		return release.ManualRetryReport{Target: target, SHA: rc.SHA}, nil
	})
	if err := fetchSourcePolls(t, s, source, pollTagChecks); err != nil {
		t.Fatal(err)
	}
	if got := s.Snapshot().ManualRetries; len(got) != 1 || got[0].SHA != rc.SHA || len(got[0].Jobs) != 0 {
		t.Fatalf("successful empty RC retry report: %+v", got)
	}
	repo := s.repos[source.Name]
	repo.Candidate.SHA = "new-head"
	s.repos[source.Name] = repo
	if got := s.Snapshot().ManualRetries; len(got) > 0 {
		t.Fatalf("stale RC job: %+v", got)
	}
}
