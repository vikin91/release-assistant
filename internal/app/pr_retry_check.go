package app

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

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

// retryCheckAllowed requires the selected, open head in the current release.
// The caller holds the session lock.
func (s *Session) retryCheckAllowed(a Action) bool {
	if a.Generation != s.generation || !a.Target.Valid() || a.Target.PR == 0 || a.SHA == "" ||
		s.choices.PRs[a.Target] != release.Included {
		return false
	}
	for _, repo := range s.repos {
		if repo.Source.Repo != a.Target.Repo {
			continue
		}
		matches := func(pr release.PR) bool {
			return pr.Target == a.Target && pr.SHA == a.SHA && pr.Evidence.Fresh() &&
				pr.State == "open" && !pr.Merged && pr.Base == repo.Source.Branch
		}
		if pr := repo.PinningPR; pr != nil && pr.Target == a.Target {
			return matches(*pr)
		}
		return slices.ContainsFunc(repo.PRs, matches)
	}
	return false
}

// checkPRRetries stores display evidence without invoking dispatch.
func (s *Session) checkPRRetries(ctx context.Context, a Action) error {
	s.mu.Lock()
	allowed := s.retryCheckAllowed(a)
	s.mu.Unlock()
	if !allowed {
		return fmt.Errorf("retry checks require the current head of an included open PR")
	}

	ctx, cancel := context.WithTimeout(ctx, s.options.Timeout)
	defer cancel()
	ctx = githubread.WithPriority(ctx, githubread.Builds, a.Target.Repo)
	check := PRRetryCheck{SHA: a.SHA, Started: time.Now()}
	var reads sync.WaitGroup
	var konfluxHeadChanged bool
	reads.Go(func() {
		plan, err := s.planner.Plan(ctx, a.Target)
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
	check.CI, ciHeadChanged, _ = readManualRetry(ctx, s.options.ManualRetries, a.Target, a.SHA)
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
	if !s.retryCheckAllowed(a) {
		return fmt.Errorf("PR head or inclusion changed during the retry check")
	}
	if current := s.prRetryChecks[a.Target]; !current.Started.After(check.Started) {
		s.prRetryChecks[a.Target] = check
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
