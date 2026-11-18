package app

import (
	"context"
	"errors"
	"slices"
	"sync"

	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

func (s *Session) readRCPlan(ctx context.Context, target release.Target) (release.Plan, error) {
	var validation release.Validation
	var read sync.WaitGroup
	if reader, ok := s.discovery.(RCCheckReader); ok {
		read.Go(func() { validation = reader.ReadRCChecks(ctx, target) })
	}
	plan, err := s.planner.Plan(ctx, target)
	read.Wait()
	if validation.SHA != "" && plan.SHA != "" && validation.SHA != plan.SHA {
		// A moved tag invalidates both the proposed retries and the combined view.
		err := errors.New("the RC.0 tag resolved to different commits during this refresh")
		return release.Plan{Target: target, Evidence: release.Evidence{At: plan.Evidence.At, Error: err.Error()}}, err
	}
	if plan.SHA == "" && validation.SHA != "" {
		plan.SHA = validation.SHA
		plan.Proposals = nil
	}
	plan.Validation = validation
	return plan, err
}

func (s *Session) clearRCPlan() {
	target := s.config.RC0Target()
	if _, ok := s.plans[target]; ok {
		delete(s.plans, target)
		s.dispatcher.ObserveIdentity(dispatch.Identity{Target: target})
	}
}

func (s *Session) observeRCPlan(target release.Target, plan release.Plan) {
	plan.Target = target
	previous := s.plans[target]
	s.recordCheckChange(target, plan.Validation.SHA, rcCheckActivity(previous.Validation), rcCheckActivity(plan.Validation))
	retainVerifiedBuilds(previous, &plan)
	// A tag check can return partial evidence. Only a verified run can offer a retry.
	plan.Proposals = slices.DeleteFunc(slices.Clone(plan.Proposals), func(proposal release.Proposal) bool {
		if plan.SHA == "" || proposal.RunID == "" || proposal.Target != target || proposal.SHA != plan.SHA {
			return true
		}
		return !slices.ContainsFunc(plan.Runs, func(run release.Run) bool {
			return run.Eligible && run.ID == proposal.RunID && run.Job == proposal.Job
		})
	})
	if plan.SHA != "" {
		identity := release.Evidence{At: plan.Evidence.At, URL: plan.Evidence.URL, Known: true}
		s.dispatcher.ObserveIdentity(dispatch.Identity{
			Target: target, SHA: plan.SHA, Open: true, Evidence: identity,
		})
	} else if s.plans[target].SHA != "" {
		s.dispatcher.ObserveIdentity(dispatch.Identity{Target: target})
	}
	s.recordStartFailures(previous, plan)
	s.plans[target] = plan
	s.dispatcher.ObservePlan(plan)
}
