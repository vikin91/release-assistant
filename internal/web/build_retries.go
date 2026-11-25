package web

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

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
		Comment                     string
		Queueable                   bool
		Inline                      bool
		NextAction                  string
		WaitingForCheck             bool
		KonfluxWarning              string
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

// BuildRetries groups retry instructions by target, including PRs awaiting their first observation.
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
	if p.Tab == "acs-prs" || p.Tab == "component-prs" {
		for target, choice := range p.Snapshot.Choices.PRs {
			if choice == release.Included && ((p.Tab == "acs-prs") == (target.Repo == p.Snapshot.Config.ACS().Repo)) && target.PR > 0 {
				targetGroup(target)
			}
		}
		for _, report := range p.ManualRetries {
			if group := byTarget[report.Target]; group != nil {
				group.Manual = &report
			}
		}
		for _, repo := range p.Repos {
			for _, row := range repo.PRs {
				if group := byTarget[row.PR.Target]; group != nil {
					group.setPRDetails(row.PR)
					group.Inline = p.PRRetryStatus(row.PR, repo.Repository.Source.Branch) != nil
				}
			}
			for _, pr := range repo.MergedPRs {
				if group := byTarget[pr.Target]; group != nil {
					group.setPRDetails(pr)
				}
			}
		}
		slices.SortFunc(targets, func(a, b *buildRetryTarget) int {
			return cmp.Or(cmp.Compare(a.Target.Repo, b.Target.Repo), cmp.Compare(a.Target.PR, b.Target.PR))
		})
	}
	for _, group := range targets {
		if group.Target.PR > 0 && !group.Inline {
			group.Comment = group.prComment()
			group.Queueable = slices.ContainsFunc(group.Candidates, func(candidate *buildRetryCandidate) bool {
				return candidate.Offer != nil && candidate.Offer.Queueable
			})
		}
		group.RequiredStartFailureSummary = group.requiredStartFailureSummary()
		if group.Plan != nil {
			group.KonfluxWarning = konfluxVerificationWarning(group.Plan.Plan)
		}
		group.NextAction, group.WaitingForCheck = group.nextAction(p.Snapshot.Interactive)
	}
	return targets
}

func (g buildRetryTarget) prComment() string {
	var payloads []string
	for _, candidate := range g.Candidates {
		proposal := candidate.Proposal
		if proposal.Target == g.Target && (g.PR.SHA == "" || proposal.SHA == g.PR.SHA) && proposal.Payload != "" {
			payloads = append(payloads, proposal.Payload)
		}
	}
	slices.Sort(payloads)
	return strings.Join(slices.Compact(payloads), "\n")
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

// nextAction summarizes Konflux observations that the row does not already show.
func (g buildRetryTarget) nextAction(interactive bool) (message string, waitingForCheck bool) {
	if len(g.Candidates) > 0 {
		if g.Target.PR == 0 {
			return "", false
		}
		sent := 0
		for _, candidate := range g.Candidates {
			offer := candidate.Offer
			if offer == nil {
				continue
			}
			if offer.CommentSent {
				sent++
			} else if offer.CanApprove {
				if !interactive {
					return "Eligible for automatic retry.", false
				}
				return "", false
			}
		}
		if sent == len(g.Candidates) {
			return "Retry comments posted. Waiting for new Konflux runs.", false
		}
		return "", false
	}
	if g.Comment != "" {
		return "", false
	}
	if g.Plan == nil {
		return "Waiting for the first Konflux check observation.", false
	}
	if g.RequiredStartFailureSummary != "" {
		return g.RequiredStartFailureSummary, false
	}
	plan := g.Plan.Plan
	if plan.KonfluxAPIUnavailable {
		return "", false
	}
	for _, failure := range plan.StartFailures {
		if !failure.Superseded && failure.Evidence.Fresh() {
			return "A PipelineRun failed to start. See Log.", false
		}
	}
	if plan.Evidence.Error != "" {
		return "", false
	}
	if plan.Action == "Wait" {
		for _, run := range plan.Runs {
			if run.ID != "" && (run.Status == "in_progress" || run.Status == "queued") {
				return "Waiting for the " + run.Job + " check to finish.", true
			}
		}
	}
	switch plan.Message {
	case "PipelineRun creation failed for a check with unknown PR or tag.":
		return "Waiting for a verified Konflux check observation.", false
	case "No verified build checks match the requested tag or PR.", "GitHub returned no check runs for this commit.":
		return "No matching Konflux build check observed on this commit.", false
	case "":
		if plan.Action == "None" {
			return "No retry needed.", false
		}
		return "", false
	default:
		return plan.Message, false
	}
}

func konfluxVerificationWarning(plan release.Plan) string {
	if plan.Evidence.Error == "" && !plan.KonfluxAPIUnavailable {
		return ""
	}
	const heading = "Could not verify Konflux checks"
	if diagnostics := retryDiagnostics(false, false, plan.Evidence.Error, nil); len(diagnostics) > 0 {
		if reason := diagnostics[0].Message; utf8.RuneCountInString(reason) <= 160 {
			return heading + " · " + reason
		}
	} else if plan.KonfluxAPIUnavailable {
		return heading + " · Konflux API unavailable"
	}
	return heading
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

// ConformaDevChecks selects developer policy findings for the PR disclosure.
func (p pageModel) ConformaDevChecks(plan planModel) retryPlanEvidence {
	plan.Plan.Advisories = slices.DeleteFunc(slices.Clone(plan.Plan.Advisories), func(advisory release.Advisory) bool {
		name, _ := strings.CutPrefix(advisory.Name, "Red Hat Konflux / ")
		scenario, _ := conformaScenario(name)
		return !isDevConformaScenario(scenario)
	})
	return p.RetryPlanEvidence(plan)
}
