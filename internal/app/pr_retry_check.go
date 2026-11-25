package app

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"release-assistant/internal/apilog"
	"release-assistant/internal/githubread"
	"release-assistant/internal/release"
)

// PRRetryCheck holds a read-only snapshot of both retry providers for one PR head.
type PRRetryCheck struct {
	Started time.Time
	SHA     string
	Konflux release.Plan
	CI      release.ManualRetryReport
}

// retryCheckSource finds the source for the selected, open head in the current release.
// The caller holds the session lock.
func (s *Session) retryCheckSource(a Action) (release.Source, bool) {
	if a.Generation != s.generation || !s.targetsActiveProfile(a.ProfileID) || !a.Target.Valid() || a.Target.PR == 0 || a.SHA == "" ||
		s.choices.PRs[a.Target] != release.Included {
		return release.Source{}, false
	}
	for _, repo := range s.repos {
		if repo.Source.Repo != a.Target.Repo {
			continue
		}
		matches := func(pr release.PR) bool {
			return pr.Target == a.Target && pr.SHA == a.SHA &&
				pr.State == "open" && !pr.Merged && pr.Base == repo.Source.Branch
		}
		if pr := repo.PinningPR; pr != nil && pr.Target == a.Target {
			return repo.Source, matches(*pr)
		}
		return repo.Source, slices.ContainsFunc(repo.PRs, matches)
	}
	return release.Source{}, false
}

// checkPRRetries refreshes PR status and retry evidence without sending retries.
func (s *Session) checkPRRetries(ctx context.Context, a Action) error {
	s.mu.Lock()
	source, allowed := s.retryCheckSource(a)
	ctx = apilog.WithRelease(ctx, s.config.Version)
	s.mu.Unlock()
	if !allowed {
		return fmt.Errorf("retry checks require the current head of an included open PR")
	}

	ctx, cancel := context.WithTimeout(ctx, s.options.Timeout)
	defer cancel()
	if reader, ok := s.discovery.(PRReader); ok {
		prCtx := githubread.WithPriority(ctx, githubread.PRs, source.Repo)
		prCtx = githubread.WithWork(prCtx, source.Repo, "On-demand PR status")
		pr, err := reader.ReadPR(prCtx, a.Target)
		if canceled(ctx) {
			return ctx.Err()
		}
		if err == nil && pr.Target == a.Target && pr.Evidence.Fresh() {
			s.mu.Lock()
			_, allowed := s.retryCheckSource(a)
			if allowed {
				if repo := s.repos[source.Name]; !slices.ContainsFunc(repo.PRs, func(current release.PR) bool {
					return current.Target == pr.Target && current.Evidence.At.After(pr.Evidence.At)
				}) {
					s.observePRHead(source, pr)
				}
			}
			_, stillCurrent := s.retryCheckSource(a)
			s.mu.Unlock()
			if !stillCurrent {
				return nil
			}
		}
	}
	ctx = githubread.WithPriority(ctx, githubread.Builds, a.Target.Repo)
	check := PRRetryCheck{SHA: a.SHA, Started: time.Now()}
	var reads sync.WaitGroup
	var konfluxHeadChanged bool
	reads.Go(func() {
		plan, err := s.planKonflux(githubread.WithWork(ctx, a.Target.Repo, "Konflux on demand"), a.Target)
		if err != nil {
			plan.Evidence.Known = false
			if plan.Evidence.Error == "" {
				plan.Evidence.Error = err.Error()
			}
		}
		konfluxHeadChanged = plan.Target == a.Target && plan.SHA != "" && plan.SHA != a.SHA
		if plan.Target != a.Target || plan.SHA != a.SHA {
			message := plan.Evidence.Error
			if message == "" {
				message = "Konflux could not verify the displayed PR head."
			}
			plan = release.Plan{Evidence: release.Evidence{Error: message}}
		}
		plan.Evidence.At = check.Started
		check.Konflux = plan
	})
	var ciHeadChanged bool
	ciCtx := githubread.WithWork(ctx, a.Target.Repo, "CI Retest on demand")
	check.CI, ciHeadChanged, _ = readManualRetry(ciCtx, s.options.CIPlanner, a.Target, a.SHA)
	check.CI.At = check.Started
	reads.Wait()
	if konfluxHeadChanged || ciHeadChanged {
		check.Konflux = release.Plan{Evidence: release.Evidence{
			At: check.Started, Error: "The PR head changed during the retry check.",
		}}
		check.CI = release.ManualRetryReport{Target: a.Target, SHA: a.SHA, At: check.Started, Incomplete: true}
	}
	if canceled(ctx) {
		return ctx.Err()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, allowed := s.retryCheckSource(a); !allowed {
		return fmt.Errorf("PR head or inclusion changed during the retry check")
	}
	if current := s.prRetryChecks[a.Target]; !current.Started.After(check.Started) {
		s.prRetryChecks[a.Target] = check
		if plan := s.plans[a.Target]; !plan.Evidence.At.After(check.Started) && check.Konflux.Target == a.Target {
			s.dispatcher.ObservePlan(check.Konflux)
		}
	}
	return nil
}

// prioritizeNewPRs reads included PRs awaiting their first plan before other targets.
// The caller holds the session lock.
func (s *Session) prioritizeNewPRs(targets []release.Target) {
	priority := func(t release.Target) int {
		if s.pendingPRChecks[t] {
			return 0
		}
		return 1
	}
	slices.SortStableFunc(targets, func(a, b release.Target) int {
		return priority(a) - priority(b)
	})
}
