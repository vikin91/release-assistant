package app

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"release-assistant/internal/githubread"
	"release-assistant/internal/release"
)

func pollCount(f *fixture, source string, scope DiscoveryScope) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, r := range f.requests {
		if r.Source.Name == source && r.Reads(scope) {
			count++
		}
	}
	return count
}

func TestComponentRefreshIntervals(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := newFixture()
		apply(t, s, Action{Kind: "configure", Config: configuration()})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go s.Run(ctx)
		synctest.Wait()

		check := func(source string, prs, tags int) {
			t.Helper()
			if got := pollCount(f, source, ReadPRs); got != prs {
				t.Fatalf("%s PR inventories = %d, want %d", source, got, prs)
			}
			if got := pollCount(f, source, ReadTags); got != tags {
				t.Fatalf("%s tag inventories = %d, want %d", source, got, tags)
			}
		}
		check("fact", 1, 1)
		check("collector", 1, 1)
		time.Sleep(time.Minute)
		tag := f.repos["fact"].Tags[0]
		apply(t, s, Action{Kind: "tag", Component: "fact", Tag: tag.Name, SHA: tag.SHA})
		synctest.Wait()
		check("fact", 2, 1) // Read PR coverage for the selected SHA.
		time.Sleep(4 * time.Minute)
		synctest.Wait()
		check("fact", 2, 1)
		check("collector", 2, 1)
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		check("fact", 2, 2)
		check("collector", 3, 2)

		// Shortening an overdue interval wakes the monitor. Lengthening it
		// moves the next read without issuing an extra request.
		apply(t, s, Action{Kind: "application-settings", RefreshIntervals: RefreshIntervals{AfterPRs: 3 * time.Minute, BeforeTags: time.Hour}})
		synctest.Wait()
		check("fact", 3, 2)
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		check("fact", 6, 3)
		check("collector", 5, 2)
	})
}

type deferredTags struct {
	*fixture
	until time.Time
}

func (f deferredTags) Discover(ctx context.Context, r DiscoveryRequest) (release.Repository, error) {
	repo, err := f.fixture.Discover(ctx, r)
	if r.Source.Name == "fact" && r.Scope == ReadTags && time.Now().Before(f.until) {
		return release.Repository{}, &githubread.DeferredError{Until: f.until, Reason: "read allowance exhausted"}
	}
	return repo, err
}

func TestDeferredTagPollLeavesPRScheduleActive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := newFixture()
		s.discovery = deferredTags{fixture: f, until: time.Now().Add(12 * time.Minute)}
		apply(t, s, Action{Kind: "configure", Config: configuration()})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go s.Run(ctx)
		synctest.Wait()
		time.Sleep(11 * time.Minute)
		synctest.Wait()
		if prs, tags := pollCount(f, "fact", ReadPRs), pollCount(f, "fact", ReadTags); prs != 3 || tags != 1 {
			t.Fatalf("while tag reads were deferred, got %d PR inventories and %d tag inventories", prs, tags)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if got := pollCount(f, "fact", ReadTags); got != 2 {
			t.Fatalf("tag reads at retry time = %d, want 2", got)
		}
	})
}

func TestNewCandidateStartsItsOwnCheckInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := newFixture()
		cfg := configuration()
		setRC := func(tag string) {
			f.mu.Lock()
			defer f.mu.Unlock()
			repo := f.repos["acs"]
			repo.Candidate = release.Candidate{Target: release.Target{Repo: cfg.ACS().Repo, Tag: tag}, SHA: tag + "-commit", Evidence: observed()}
			f.repos["acs"] = repo
		}
		setRC("5.0.1-rc.0")
		checks := make(chan string, 8)
		s.discovery = tagCheckFixture{f, func(t release.Target) (release.Validation, error) {
			checks <- t.Tag
			return release.Validation{SHA: t.Tag + "-commit", Evidence: observed()}, nil
		}}
		apply(t, s, Action{Kind: "configure", Config: cfg})
		apply(t, s, Action{Kind: "application-settings", RefreshIntervals: RefreshIntervals{ACSTagChecks: time.Hour}})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go s.Run(ctx)
		synctest.Wait()
		setRC("5.0.1-rc.1")
		time.Sleep(9 * time.Minute)
		synctest.Wait()
		if len(checks) != 1 || <-checks != "5.0.1-rc.0" {
			t.Fatal("expected one check for rc.0 before the next tag inventory")
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if len(checks) != 1 || <-checks != "5.0.1-rc.1" {
			t.Fatal("expected a check for rc.1 as soon as tag discovery found it")
		}
	})
}
