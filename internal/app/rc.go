package app

import (
	"slices"

	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

func (s *Session) clearRCPlan() {
	target := s.config.RC0Target()
	if _, ok := s.plans[target]; ok {
		delete(s.plans, target)
		s.dispatcher.ObserveIdentity(dispatch.Identity{Target: target})
	}
}

func (s *Session) observeRCPlan(target release.Target, plan release.Plan) {
	plan.Target = target
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
	s.recordStartFailures(s.plans[target], plan)
	s.plans[target] = plan
	s.dispatcher.ObservePlan(plan)
}
