package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"release-assistant/internal/dispatch"
	"release-assistant/internal/githubread"
	"release-assistant/internal/release"

	retry "github.com/vikin91/konflux-retest"
)

type buildPollPlanner func(context.Context, release.Target) (release.Plan, error)

func (f buildPollPlanner) PlanRetries(ctx context.Context, target release.Target) (release.RetryPlan, error) {
	plan, err := f.Plan(ctx, target)
	return plan.RetryPlan(), err
}

func (f buildPollPlanner) Plan(ctx context.Context, target release.Target) (release.Plan, error) {
	return f(ctx, target)
}

type prPollReader struct {
	*fixture
	read func(release.Target) (release.PR, error)
}

func (r prPollReader) ReadPR(_ context.Context, target release.Target) (release.PR, error) {
	return r.read(target)
}

func TestBuildPollRefreshesChangedPRHeadOnce(t *testing.T) {
	for name, tc := range map[string]struct {
		githubHead, wantHead string
		readErr              error
		wantPlan             bool
	}{
		"should verify the new head":                       {githubHead: "new-head", wantHead: "new-head", wantPlan: true},
		"should prefer a different current head on GitHub": {githubHead: "latest-head", wantHead: "latest-head"},
		"should leave identity stale on read failure":      {readErr: errors.New("GitHub unavailable"), wantHead: "new-head", wantPlan: true},
	} {
		t.Run(name, func(t *testing.T) {
			s, f := newFixture()
			config := configuration()
			source := config.Components[1]
			target := release.Target{Repo: source.Repo, PR: 4298}
			pr := release.PR{Target: target, SHA: "old-head", State: "open", Base: source.Branch, Evidence: observed(),
				Checks: []release.Check{{Name: "CI", Required: release.Yes, Status: "completed", Conclusion: "success"}}, ChecksEvidence: observed()}
			repo := f.repos[source.Name]
			repo.PRs = []release.PR{pr}
			f.repos[source.Name] = repo
			apply(t, s, Action{Kind: "configure", Config: config})
			if err := fetchSourcePolls(t, s, source, pollPRs); err != nil {
				t.Fatal(err)
			}
			apply(t, s, Action{Kind: "choice", Target: target, Choice: release.Included})
			if err := fetchSourcePolls(t, s, source, pollChecks); err != nil {
				t.Fatal(err)
			}
			if err := s.dispatcher.Grant(target, pr.SHA, true); err != nil {
				t.Fatal(err)
			}
			head := pr.SHA
			s.planner = buildPollPlanner(func(_ context.Context, target release.Target) (release.Plan, error) {
				return release.Plan{Target: target, SHA: head, Evidence: observed()}, nil
			})
			reads := 0
			fresh := release.PR{Target: target, SHA: tc.githubHead, Title: "updated title", State: "open", Base: source.Branch, CommentLabel: true, Evidence: observed()}
			s.discovery = prPollReader{fixture: f, read: func(got release.Target) (release.PR, error) {
				reads++
				if got != target {
					t.Fatalf("refreshed %v, want %v", got, target)
				}
				if err := s.dispatcher.Grant(target, pr.SHA, true); err == nil {
					t.Fatal("old head was still eligible for a grant during verification")
				}
				return fresh, tc.readErr
			}}
			if err := fetchSourcePolls(t, s, source, pollBuilds); err != nil || reads != 0 {
				t.Fatalf("unchanged head: %d targeted reads, err=%v", reads, err)
			}
			head = "new-head"
			for range 2 {
				if err := fetchSourcePolls(t, s, source, pollBuilds); err != nil {
					t.Fatal(err)
				}
			}
			if reads != 1 || pollCount(f, source.Name, ReadPRs) != 1 || pollCount(f, source.Name, ReadChecks) != 1 {
				t.Fatalf("expected one targeted refresh with the regular poll counts, got %d targeted reads and discovery requests %+v", reads, f.requests)
			}
			got := s.repos[source.Name].PRs[0]
			if got.SHA != tc.wantHead || got.Evidence.Fresh() != (tc.readErr == nil) || got.ChecksEvidence.Fresh() || len(got.Checks) != 0 {
				t.Fatalf("unexpected refreshed PR: %+v", got)
			}
			if tc.readErr == nil && (got.Title != fresh.Title || !got.CommentLabel) {
				t.Fatalf("fresh PR metadata missing: %+v", got)
			}
			plan, exists := s.plans[target]
			if exists != tc.wantPlan || exists && plan.SHA != tc.wantHead {
				t.Fatalf("unexpected plan: %+v (exists=%t)", plan, exists)
			}
			if slices.ContainsFunc(s.dispatcher.Views(), func(view dispatch.View) bool { return view.Target == target && view.Granted }) {
				t.Fatal("head change kept the old grant")
			}
			if tc.readErr != nil {
				pr.SHA = head
				repo.PRs = []release.PR{pr}
				f.repos[source.Name] = repo
				if err := fetchSourcePolls(t, s, source, pollPRs); err != nil {
					t.Fatal(err)
				}
				if got := s.repos[source.Name].PRs[0]; !got.Evidence.Fresh() || got.SHA != head || reads != 1 {
					t.Fatalf("regular poll did not recover verification: PR=%+v targeted reads=%d", got, reads)
				}
			}
		})
	}
}

func TestBuildPollBackoffDistinguishesUnsupportedRetagContracts(t *testing.T) {
	contract := fmt.Errorf("%w: invalid .tekton/retag-pipeline.yaml", retry.ErrUnsupportedRetagContract)
	for name, readErr := range map[string]error{
		"contract mismatch alone keeps the normal interval":    nil,
		"contract mismatch with an unavailable read backs off": context.DeadlineExceeded,
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, f := newFixture()
				config := configuration()
				source := config.ACS()
				bad := release.Target{Repo: source.Repo, PR: 1}
				good := release.Target{Repo: source.Repo, PR: 2}
				repo := f.repos[source.Name]
				repo.PRs = []release.PR{
					{Target: bad, SHA: "bad-head", State: "open", Base: source.Branch, Evidence: observed()},
					{Target: good, SHA: "good-head", State: "open", Base: source.Branch, Evidence: observed()},
				}
				f.repos[source.Name] = repo
				apply(t, s, Action{Kind: "configure", Config: config})
				if err := fetchSourcePolls(t, s, source, pollPRs); err != nil {
					t.Fatal(err)
				}
				for _, target := range []release.Target{bad, good} {
					apply(t, s, Action{Kind: "choice", Target: target, Choice: release.Included})
				}

				planErr := errors.Join(errors.Join(contract, contract), readErr)
				healthyReads := 0
				s.planner = buildPollPlanner(func(_ context.Context, target release.Target) (release.Plan, error) {
					if target == bad {
						return release.Plan{Target: target, SHA: "bad-head", Evidence: release.Evidence{At: time.Now(), Error: planErr.Error()}}, planErr
					}
					healthyReads++
					return release.Plan{Target: target, SHA: "good-head", Evidence: observed()}, nil
				})
				s.options.RefreshIntervals[KonfluxBuilds] = time.Minute
				jobs := s.pollJobs(source)
				i := slices.IndexFunc(jobs, func(job pollJob) bool { return job.kind == pollBuilds })
				if i < 0 {
					t.Fatal("missing build poll for included PRs")
				}
				poll := func() {
					ctx, _ := githubread.Track(t.Context(), 80)
					s.runPoll(ctx, s.generation, s.workRevision, source, jobs[i])
				}
				poll()
				time.Sleep(time.Minute)
				poll()
				wantReads := 2
				if readErr != nil {
					wantReads = 1
				}
				if healthyReads != wantReads {
					t.Fatalf("healthy PR received %d reads, want %d", healthyReads, wantReads)
				}
				if plan := s.plans[bad]; !strings.Contains(plan.Evidence.Error, contract.Error()) || plan.Evidence.Fresh() || len(plan.Proposals) != 0 {
					t.Fatalf("unsupported retag diagnostic lost or became eligible: %+v", plan)
				}
			})
		})
	}
}

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
		check("fact", 1, 0)
		check("collector", 1, 0)
		time.Sleep(time.Minute)
		tag := f.repos["fact"].Tags[0]
		s.mu.Lock()
		repo := s.repos["fact"]
		repo.Tags = []release.Tag{tag}
		s.repos["fact"] = repo
		s.mu.Unlock()
		apply(t, s, Action{Kind: "tag", Component: "fact", Tag: tag.Name, SHA: tag.SHA})
		synctest.Wait()
		check("fact", 1, 1) // Selecting a tag leaves the existing PR poll on its schedule.
		time.Sleep(4 * time.Minute)
		synctest.Wait()
		check("fact", 1, 1)
		check("collector", 2, 0)
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		check("fact", 1, 1)
		check("collector", 3, 0)

		// Shortening an overdue interval wakes the monitor. Lengthening it
		// moves the next read without issuing an extra request.
		apply(t, s, Action{Kind: "application-settings", RefreshIntervals: RefreshIntervals{AfterPRs: 3 * time.Minute, BeforeTags: time.Hour}})
		synctest.Wait()
		check("fact", 2, 1)
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		check("fact", 5, 2)
		check("collector", 5, 0)
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
		tag := f.repos["fact"].Tags[0]
		s.pendingTags["fact"] = savedComponentTag{Tag: tag.Name, SHA: tag.SHA}
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

func TestRefreshRCRequestsDiscoveryBeforeTheNextInterval(t *testing.T) {
	s, f, _ := rcFixture(t)
	source := s.config.ACS()
	original := s.tagPollJob(source)
	s.polls[original.id(source)] = pollRecord{identity: original.identity, finished: time.Now()}
	action := Action{Kind: "refresh-rc", Generation: s.generation}
	apply(t, s, action)
	requested := s.tagPollJob(source)
	apply(t, s, action)
	if requested == original || requested != s.tagPollJob(source) || !s.Snapshot().RCRefreshing {
		t.Fatal("expected one pending RC refresh")
	}
	s.runPoll(t.Context(), s.generation, s.workRevision, source, requested)
	last := f.requests[len(f.requests)-1]
	if last.Source != source || last.Scope != ReadRC|ReadPreviousPins || s.Snapshot().RCRefreshing {
		t.Fatalf("expected the RC discovery poll to finish, last request: %+v", last)
	}
}
