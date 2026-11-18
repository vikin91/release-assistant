package web

import (
	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

type (
	buildRetryTarget struct {
		Target     release.Target
		Plan       *planModel
		Authority  *dispatch.View
		Candidates []*buildRetryCandidate
	}
	buildRetryCandidate struct {
		Key      string
		Proposal release.Proposal
		Offer    *dispatch.Offer
		Status   string
	}
	retryPlanEvidence struct {
		Page pageModel
		Plan planModel
	}
	retryOfferActions struct {
		Page  pageModel
		Offer dispatch.Offer
	}
)

// BuildRetries groups exact retry comments and their current offers by target.
func (p pageModel) BuildRetries() []*buildRetryTarget {
	var targets []*buildRetryTarget
	byTarget := make(map[release.Target]*buildRetryTarget)
	byIdentity := make(map[string]*buildRetryCandidate)
	targetGroup := func(target release.Target) *buildRetryTarget {
		group := byTarget[target]
		if group == nil {
			group = &buildRetryTarget{Target: target}
			byTarget[target] = group
			targets = append(targets, group)
		}
		return group
	}
	candidate := func(group *buildRetryTarget, proposal release.Proposal) *buildRetryCandidate {
		key := dispatch.ID(proposal)
		row := byIdentity[key]
		if row == nil {
			row = &buildRetryCandidate{Key: key, Proposal: proposal, Status: p.retryStatus(nil)}
			byIdentity[key] = row
			group.Candidates = append(group.Candidates, row)
		}
		return row
	}
	for _, plan := range p.Plans {
		group := targetGroup(plan.Plan.Target)
		group.Plan = &plan
		for _, proposal := range plan.Plan.Proposals {
			candidate(group, proposal)
		}
	}
	for _, authority := range p.Authority {
		group := targetGroup(authority.View.Target)
		group.Authority = &authority.View
		for _, offer := range authority.View.Offers {
			row := candidate(group, offer.Proposal)
			row.Offer = &offer
			row.Status = p.retryStatus(&offer)
		}
	}
	return targets
}

func (p pageModel) retryStatus(offer *dispatch.Offer) string {
	switch {
	case offer != nil && offer.Rejected:
		return "Skipped for this run"
	case !p.Snapshot.WritesEnabled:
		return "Copy only · read-only mode"
	case offer == nil:
		return "Copy only · no current offer"
	case offer.CanApprove && p.Snapshot.Interactive:
		return "Ready to review"
	case offer.CanApprove:
		return "Eligible for automatic retry"
	case offer.Reason != "":
		return offer.Reason
	default:
		return "Posting unavailable"
	}
}

// RetryPlanEvidence supplies the live session values for build evidence and review forms.
func (p pageModel) RetryPlanEvidence(plan planModel) retryPlanEvidence {
	return retryPlanEvidence{Page: p, Plan: plan}
}

// RetryOfferActions binds the exact offer to the current session's comment forms.
func (p pageModel) RetryOfferActions(offer dispatch.Offer) retryOfferActions {
	return retryOfferActions{Page: p, Offer: offer}
}
