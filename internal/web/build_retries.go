package web

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

type (
	buildRetryTarget struct {
		Target                      release.Target
		Title                       string
		Plan                        *planModel
		Authority                   *dispatch.View
		Candidates                  []*buildRetryCandidate
		NextAction                  string
		RequiredStartFailureSummary string
		Manual                      *release.ManualRetryReport
		ClosedState                 string
		PR                          release.PR
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
)

// BuildRetries groups retry instructions by target, including ACS PRs awaiting their first observation.
func (p pageModel) BuildRetries() []*buildRetryTarget {
	var targets []*buildRetryTarget
	byTarget := make(map[release.Target]*buildRetryTarget)
	byIdentity := make(map[string]*buildRetryCandidate)
	targetGroup := func(target release.Target) *buildRetryTarget {
		group := byTarget[target]
		if group == nil {
			group = &buildRetryTarget{Target: target, Title: p.acsPRTitles[target]}
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
	if p.Tab == "acs-prs" {
		for target, choice := range p.Snapshot.Choices.PRs {
			if choice == release.Included && target.Repo == p.Snapshot.Config.ACS().Repo && target.PR > 0 {
				targetGroup(target)
			}
		}
		for _, report := range p.ManualRetries {
			if group := byTarget[report.Target]; group != nil {
				group.Manual = &report
			}
		}
		if p.ACS != nil {
			for _, pr := range p.ACS.PRs {
				if group := byTarget[pr.Target]; group != nil {
					group.setPRDetails(pr)
				}
			}
			if pr := p.ACS.PinningPR; pr != nil && byTarget[pr.Target] != nil {
				byTarget[pr.Target].setPRDetails(*pr)
			}
		}
		slices.SortFunc(targets, func(a, b *buildRetryTarget) int {
			return cmp.Compare(a.Target.PR, b.Target.PR)
		})
	}
	for _, group := range targets {
		group.RequiredStartFailureSummary = group.requiredStartFailureSummary()
		group.NextAction = group.nextAction(p.Snapshot.Interactive)
	}
	return targets
}

func (g *buildRetryTarget) setPRDetails(pr release.PR) {
	g.PR = pr
	if pr.Merged || pr.State == "merged" {
		g.ClosedState = "Merged"
	} else if pr.State == "closed" {
		g.ClosedState = "Closed"
	}
}

func (g buildRetryTarget) requiredStartFailureSummary() string {
	if g.Plan == nil || g.PR.SHA == "" || g.PR.SHA != g.Plan.Plan.SHA || !g.PR.Evidence.Fresh() || !g.PR.ChecksEvidence.Fresh() {
		return ""
	}
	var names []string
	for _, check := range g.PR.Checks {
		if check.CheckRunID <= 0 || check.Required != release.Yes || check.Result() != release.No {
			continue
		}
		if slices.ContainsFunc(g.Plan.Plan.StartFailures, func(failure release.StartFailure) bool {
			return failure.CheckID == check.CheckRunID && !failure.Superseded && failure.Evidence.Fresh()
		}) {
			name, _ := strings.CutPrefix(check.Name, "Red Hat Konflux / ")
			names = append(names, name)
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	switch len(names) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf("1 required check failed to start (%s). Waiting for a new attempt.", names[0])
	default:
		return fmt.Sprintf("%d required checks failed to start (%s). Waiting for new attempts.", len(names), strings.Join(names, ", "))
	}
}

// nextAction describes the next step supported by this target's observations.
func (g buildRetryTarget) nextAction(interactive bool) string {
	if len(g.Candidates) > 0 {
		sent := 0
		for _, candidate := range g.Candidates {
			offer := candidate.Offer
			if offer == nil {
				continue
			}
			if offer.CommentSent {
				sent++
			} else if offer.CanApprove {
				if interactive {
					return "Review the proposed retry comment in the Retest Queue."
				}
				return "Wait for the automatic retry comment to be posted."
			}
		}
		if sent == len(g.Candidates) {
			return "Wait for Konflux to start a new run after the retry comment."
		}
		return "Review the retry status for each job below."
	}
	if g.Plan == nil {
		return "Wait for the first Konflux check observation."
	}
	if g.RequiredStartFailureSummary != "" {
		return g.RequiredStartFailureSummary
	}
	plan := g.Plan.Plan
	if plan.KonfluxAPIUnavailable {
		return "Wait for the Konflux API to recover. It was unavailable at the last check."
	}
	for _, failure := range plan.StartFailures {
		if !failure.Superseded && failure.Evidence.Fresh() {
			return "A PipelineRun failed to start. See Reported build creation failures."
		}
	}
	if plan.Evidence.Error != "" {
		return "Review the observation error in Check details."
	}
	if plan.Action == "Wait" {
		for _, run := range plan.Runs {
			if run.ID != "" && (run.Status == "in_progress" || run.Status == "queued") {
				return "Wait for the " + run.Job + " check to finish."
			}
		}
	}
	switch plan.Message {
	case "PipelineRun creation failed for a check with unknown PR or tag.":
		return "Wait for the next Konflux check observation."
	case "No verified build checks match the requested tag or PR.", "GitHub returned no check runs for this commit.":
		return "Wait for a matching Konflux build check to appear on this commit."
	case "Waiting for usable input images.":
		return "Wait for the input images to become available."
	case "":
		if plan.Action == "None" {
			return "No retry needed."
		}
		return "Wait for the next Konflux check observation."
	default:
		return plan.Message
	}
}

func (p pageModel) retryStatus(offer *dispatch.Offer) string {
	switch {
	case offer != nil && offer.CommentSent:
		return "Comment sent"
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
