package app

import (
	"context"
	"errors"
	"slices"
	"testing"

	"release-assistant/internal/release"
)

type manualReaderFunc func(context.Context, release.Target) (release.ManualRetryReport, error)

func (f manualReaderFunc) ManualRetries(ctx context.Context, target release.Target) (release.ManualRetryReport, error) {
	return f(ctx, target)
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
	s.options.ManualRetries = manualReaderFunc(func(_ context.Context, target release.Target) (release.ManualRetryReport, error) {
		calls[target]++
		if target == selectedTag {
			return release.ManualRetryReport{Target: target, SHA: tag.SHA, Jobs: []release.ManualJob{{ID: 42}}}, nil
		}
		return release.ManualRetryReport{Target: target, SHA: reportSHA, Prow: []string{"ci/prow/test"}}, errors.New("Actions jobs unavailable")
	})
	refresh := func() Snapshot {
		t.Helper()
		if err := s.refreshPlanned(t.Context(), s.generation, source, false); err != nil {
			t.Fatal(err)
		}
		return s.Snapshot()
	}
	snap := refresh()
	if len(calls) != 2 || calls[pr] != 1 || calls[selectedTag] != 1 || len(snap.ManualRetries) != 2 {
		t.Fatalf("selected targets %v, reports %+v", calls, snap.ManualRetries)
	}
	if got := snap.ManualRetries[0]; got.Target != pr || !slices.Equal(got.Prow, []string{"ci/prow/test"}) || !slices.Contains(got.Warnings, "Actions jobs unavailable") {
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

func TestManualRetriesFollowCurrentRC(t *testing.T) {
	s, f, _ := rcFixture(t)
	source := s.config.ACS()
	rc := f.repos[source.Name].Candidate
	s.options.ManualRetries = manualReaderFunc(func(_ context.Context, target release.Target) (release.ManualRetryReport, error) {
		if target != rc.Target {
			t.Fatalf("unexpected manual target %s", target)
		}
		return release.ManualRetryReport{Target: target, SHA: rc.SHA, Jobs: []release.ManualJob{{ID: 88}}}, nil
	})
	if err := s.refreshPlanned(t.Context(), s.generation, source, true); err != nil {
		t.Fatal(err)
	}
	if got := s.Snapshot().ManualRetries; len(got) != 1 || got[0].Jobs[0].ID != 88 {
		t.Fatalf("missing RC job: %+v", got)
	}
	repo := s.repos[source.Name]
	repo.Candidate.SHA = "new-head"
	s.repos[source.Name] = repo
	if got := s.Snapshot().ManualRetries; len(got) > 0 {
		t.Fatalf("stale RC job: %+v", got)
	}
}
