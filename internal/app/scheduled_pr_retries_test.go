package app

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"release-assistant/internal/githubread"
	"release-assistant/internal/release"
)

func drainReadyPRRetries(t *testing.T, s *Session) {
	t.Helper()
	var lastRepo string
	for {
		job, _, _ := s.nextPRRetry(s.generation, lastRepo)
		if job.target.PR == 0 {
			return
		}
		s.runPRRetry(t.Context(), job)
		lastRepo = job.target.Repo
	}
}

func TestScheduledPRRetriesRotateAndResumeDeferredWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := newFixture()
		s.config = configuration()
		collector, fact := s.config.Components[1], s.config.Components[0]
		addPR := func(source release.Source, n int) release.Target {
			target := release.Target{Repo: source.Repo, PR: n}
			repo := s.repos[source.Name]
			repo.Source = source
			repo.PRs = append(repo.PRs, release.PR{Target: target, State: "open", Base: source.Branch, SHA: "head", Evidence: observed()})
			s.repos[source.Name] = repo
			s.choices.PRs[target] = release.Included
			return target
		}
		deferred, routine, small := addPR(collector, 1), addPR(collector, 2), addPR(fact, 1)
		s.manualRetries[routine] = release.ManualRetryReport{Target: routine, SHA: "head"}
		var mu sync.Mutex
		var order []release.Target
		plannerCalls := func() []release.Target {
			mu.Lock()
			defer mu.Unlock()
			return slices.Clone(order)
		}
		resume := time.Now().Add(time.Minute)
		s.options.CIPlanner = manualReaderFunc(func(_ context.Context, target release.Target) (release.ManualRetryReport, error) {
			mu.Lock()
			order = append(order, target)
			mu.Unlock()
			if target == deferred && time.Now().Before(resume) {
				return release.ManualRetryReport{}, &githubread.DeferredError{Reason: "budget exhausted", Until: resume}
			}
			if target == small {
				// A due poll must coalesce the job even while its planner call is running.
				s.enqueuePRRetries(s.generation, s.workRevision, fact)
			}
			return release.ManualRetryReport{Target: target, SHA: "head", Jobs: []release.ManualJob{{ID: 42}}}, nil
		})
		f.repos[collector.Name] = s.repos[collector.Name]
		f.repos[fact.Name] = s.repos[fact.Name]
		for _, source := range []release.Source{collector, fact} {
			ctx, usage := githubread.Track(t.Context(), 80)
			if err := s.fetchPoll(ctx, s.generation, s.workRevision, source, pollJob{kind: pollChecks}, false, usage); err != nil {
				t.Fatal(err)
			}
		}
		if len(order) != 0 {
			t.Fatal("repository poll waited for CI Retest")
		}

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go s.monitorPRRetries(ctx, s.generation)
		synctest.Wait()
		if got, want := plannerCalls(), []release.Target{deferred, small, routine}; !slices.Equal(got, want) {
			t.Fatalf("planner calls = %v, want %v", got, want)
		}
		if got := s.Snapshot(); len(got.RetestQueue) != 2 || !slices.ContainsFunc(got.ManualRetries, func(report release.ManualRetryReport) bool {
			return report.Target == deferred && report.Incomplete
		}) {
			t.Fatalf("expected successful reports while one PR was deferred: %+v", got.ManualRetries)
		}
		time.Sleep(time.Minute - time.Nanosecond)
		synctest.Wait()
		if got := plannerCalls(); len(got) != 3 {
			t.Fatalf("called planner before deferral ended: %v", got)
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if got, want := plannerCalls(), []release.Target{deferred, small, routine, deferred}; !slices.Equal(got, want) {
			t.Fatalf("planner calls = %v, want %v", got, want)
		}
		if got := s.Snapshot(); len(got.RetestQueue) != 3 || slices.ContainsFunc(got.ManualRetries, func(report release.ManualRetryReport) bool {
			return report.Incomplete
		}) {
			t.Fatalf("deferred check did not resume independently of the poll: %+v", got.ManualRetries)
		}
	})
}

func TestScheduledPRRetryDiscardsChangedTargets(t *testing.T) {
	for name, change := range map[string]func(*Session, *release.PR){
		"head changes": func(_ *Session, pr *release.PR) { pr.SHA = "new-head" },
		"PR closes":    func(_ *Session, pr *release.PR) { pr.State = "closed" },
		"PR excluded":  func(s *Session, pr *release.PR) { s.choices.PRs[pr.Target] = release.Excluded },
		"release switches": func(s *Session, _ *release.PR) {
			s.generation++
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := newFixture()
			source := configuration().Components[0]
			target := release.Target{Repo: source.Repo, PR: 1}
			repo := release.Repository{Source: source, PRs: []release.PR{{
				Target: target, State: "open", Base: source.Branch, SHA: "head", Evidence: observed(),
			}}}
			s.repos[source.Name] = repo
			s.choices.PRs[target] = release.Included
			s.options.CIPlanner = manualReaderFunc(func(_ context.Context, _ release.Target) (release.ManualRetryReport, error) {
				s.mu.Lock()
				change(s, &repo.PRs[0])
				s.mu.Unlock()
				return release.ManualRetryReport{Target: target, SHA: "head", Jobs: []release.ManualJob{{ID: 42}}}, nil
			})
			s.enqueuePRRetries(s.generation, s.workRevision, source)
			drainReadyPRRetries(t, s)
			if len(s.manualRetries) != 0 || len(s.ciRetryJobs) != 0 || len(s.Snapshot().RetestQueue) != 0 {
				t.Fatal("published or kept the old result after the target changed")
			}
		})
	}
}

func TestScheduledPRRetriesUseSharedLimiterOutsideSourceCycle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := newFixture()
		source := configuration().Components[0]
		target := release.Target{Repo: source.Repo, PR: 1}
		repo := f.repos[source.Name]
		repo.PRs = []release.PR{{Target: target, State: "open", Base: source.Branch, SHA: "head", Evidence: observed()}}
		f.repos[source.Name] = repo
		s.choices.PRs[target] = release.Included

		var mu sync.Mutex
		var reads []string
		githubCalls := func() []string {
			mu.Lock()
			defer mu.Unlock()
			return slices.Clone(reads)
		}
		s.options.GitHubReads = githubread.New(advisoryTransport(func(req *http.Request) (*http.Response, error) {
			mu.Lock()
			defer mu.Unlock()
			reads = append(reads, req.URL.Path)
			if len(reads) == 2 {
				return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"60"}}}, nil
			}
			return &http.Response{StatusCode: http.StatusOK}, nil
		}))
		client := &http.Client{Transport: s.options.GitHubReads}
		read := func(ctx context.Context, path string) error {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/"+path, nil)
			if err != nil {
				return err
			}
			resp, err := client.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusTooManyRequests {
				return errors.New("GitHub rate limit reached")
			}
			return nil
		}
		s.options.CIPlanner = manualReaderFunc(func(ctx context.Context, target release.Target) (release.ManualRetryReport, error) {
			report := release.ManualRetryReport{Target: target, SHA: "head"}
			if err := read(ctx, "pr"); err != nil {
				return report, err
			}
			return report, read(ctx, "checks")
		})

		cycle, usage := githubread.Track(t.Context(), 1)
		if err := read(cycle, "discovery"); err != nil {
			t.Fatal(err)
		}
		if err := s.fetchPoll(cycle, s.generation, s.workRevision, source, pollJob{kind: pollChecks}, false, usage); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go s.monitorPRRetries(ctx, s.generation)
		synctest.Wait()
		if got, want := githubCalls(), []string{"/discovery", "/pr"}; !slices.Equal(got, want) {
			t.Fatalf("reads = %v, want %v", got, want)
		}
		time.Sleep(59 * time.Second)
		synctest.Wait()
		if got := githubCalls(); len(got) != 2 {
			t.Fatalf("worker ignored the shared GitHub pause: %v", got)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if got, want := githubCalls(), []string{"/discovery", "/pr", "/pr", "/checks"}; !slices.Equal(got, want) || usage.Calls() != 1 {
			t.Fatalf("resumed reads = %v with %d source reads, want %v with one source read", got, usage.Calls(), want)
		}
		if got := s.Snapshot().ManualRetries; len(got) != 1 || got[0].SHA != "head" || got[0].Incomplete {
			t.Fatalf("report after resuming: %+v", got)
		}
	})
}
