package web

import (
	"cmp"
	"fmt"
	"slices"

	"release-assistant/internal/release"
)

type (
	prListModel struct {
		PinDiscoveryStatus                       string
		Repos                                    []repoModel
		Rows                                     map[release.Target]prListRow
		AwaitingMerge, Closed, Undecided, Merged int
		Open, Active, Ignored                    int
	}
	prListRow struct {
		PinRole, PinReason string
		Checks             prCheckSummary
		MergeStatus        release.Truth
		Attention          bool
		Retry              *pageModel
		HasProposal        bool
	}
	prCheckSummary struct {
		URL, RetryURL, Problem            string
		Available, Partial, RequiredKnown bool
		Failed, RequiredFailed, Running   int
	}
	requiredCheckCounts struct {
		Failed, Running int
	}
)

// PRList groups PRs by repository and summarizes current check evidence.
func (p pageModel) PRList() prListModel {
	triage := prListModel{
		Repos: slices.Clone(p.Repos),
		Rows:  make(map[release.Target]prListRow),
	}
	var (
		retries      map[release.Target]*pageModel
		acsProposals map[release.Target]bool
	)
	if p.Tab == "acs-prs" {
		acsProposals = make(map[release.Target]bool)
		for _, report := range p.ManualRetries {
			if len(report.Jobs)+len(report.Prow) > 0 {
				acsProposals[report.Target] = true
			}
		}
		for _, plan := range p.Plans {
			if len(plan.Plan.Proposals) > 0 {
				acsProposals[plan.Plan.Target] = true
			}
		}
		for _, authority := range p.Authority {
			if len(authority.View.Offers) > 0 {
				acsProposals[authority.View.Target] = true
			}
		}
	}
	if p.Tab == "component-prs" {
		retries = make(map[release.Target]*pageModel)
		retryPage := func(target release.Target) *pageModel {
			if retries[target] == nil {
				page := p
				page.Repos, page.Plans, page.Authority = nil, nil, nil
				page.RetrySection = p.PRSection
				retries[target] = &page
			}
			return retries[target]
		}
		for _, plan := range p.Plans {
			page := retryPage(plan.Plan.Target)
			page.Plans = append(page.Plans, plan)
		}
		for _, authority := range p.Authority {
			page := retryPage(authority.View.Target)
			page.Authority = append(page.Authority, authority)
		}
	}
	for i, repo := range triage.Repos {
		triage.Merged += len(repo.MergedPRs)
		triage.Repos[i].PRs = slices.Clone(repo.PRs)
		slices.SortStableFunc(triage.Repos[i].PRs, func(a, b prModel) int {
			return cmp.Compare(a.PR.Target.PR, b.PR.Target.PR)
		})
		discovery := repo.Repository.PinDiscovery
		detecting := repo.Repository.Source.Name == "acs" && p.Snapshot.Config.PinningPR == 0
		candidates := discovery.Candidates()
		if detecting {
			switch {
			case !discovery.Evidence.Fresh():
				triage.PinDiscoveryStatus = "Pinning PR discovery incomplete. " + discovery.Evidence.Error
			case candidates == 0:
				triage.PinDiscoveryStatus = "No open PR changes a component pin file."
			case candidates > 1:
				triage.PinDiscoveryStatus = "Several PRs change component pin files. Choose the PR to monitor."
			}
		}
		for _, row := range repo.PRs {
			if row.PR.State == "open" && row.PR.Evidence.Fresh() {
				triage.Open++
			}
			view := prListRow{Retry: retries[row.PR.Target], HasProposal: acsProposals[row.PR.Target]}
			if row.PR.Target == p.Snapshot.Config.PinningTarget() {
				view.PinRole = "Confirmed pinning PR"
			} else if match := discovery.Files[row.PR.Target.PR]; detecting && match.Reason != "" && match.Head == row.PR.SHA && row.PR.Evidence.Fresh() && row.PR.State == "open" {
				view.PinRole, view.PinReason = "Pinning PR candidate", match.Reason
				if discovery.Evidence.Fresh() && candidates == 1 {
					view.PinRole = "Suggested pinning PR"
				}
			}
			triage.Rows[row.PR.Target] = view
			if row.Choice == release.Ignored {
				triage.Ignored++
				continue
			}
			view.MergeStatus = prMergeStatus(row.PR)
			triage.Active++
			closed := row.PR.State == "closed"
			switch {
			case row.Choice == release.Included && closed:
				triage.Closed++
			case row.Choice == release.Included:
				triage.AwaitingMerge++
			case row.Choice == "" && undecidedCandidate(row.PR, repo.Repository.Source.Branch):
				triage.Undecided++
			}
			retry := retries[row.PR.Target]

			if row.Choice == release.Included {
				view.Checks = summarizePRChecks(row.PR)
				if view.HasProposal {
					view.Checks.RetryURL = fmt.Sprintf("/?tab=acs-prs#acs-pr-retries-%d", row.PR.Target.PR)
				}
				view.Attention = closed || view.Checks.Failed > 0
			}
			if retry != nil {
				view.HasProposal = slices.ContainsFunc(retry.Plans, func(plan planModel) bool {
					return len(plan.Plan.Proposals) > 0
				}) || slices.ContainsFunc(retry.Authority, func(view authorityModel) bool {
					return len(view.View.Offers) > 0
				})
			}
			triage.Rows[row.PR.Target] = view
		}
	}
	return triage
}

func prMergeStatus(pr release.PR) release.Truth {
	if pr.State != "open" || !pr.Evidence.Fresh() {
		return ""
	}
	if pr.Draft {
		return release.No
	}
	switch pr.MergeState {
	case "clean", "unstable":
		return release.Yes
	case "blocked", "behind", "dirty", "draft":
		return release.No
	default:
		return ""
	}
}

func summarizePRChecks(pr release.PR) prCheckSummary {
	summary := prCheckSummary{}
	if pr.Target.Valid() && pr.Target.PR > 0 {
		summary.URL = fmt.Sprintf("https://github.com/%s/pull/%d/checks", pr.Target.Repo, pr.Target.PR)
	}
	if !pr.Evidence.Fresh() {
		summary.Problem = pr.Evidence.Error
		return summary
	}
	results := pr.CheckResultsEvidence
	available := !results.At.IsZero() && results.Known
	if results.At.IsZero() {
		results, available = pr.ChecksEvidence, pr.ChecksEvidence.Fresh()
	}
	summary.Problem = results.Error
	if !available || len(pr.Checks) == 0 {
		if available && summary.Problem == "" {
			summary.Problem = "GitHub has not reported checks for this PR head."
		}
		return summary
	}
	summary.Available = true
	summary.Partial = !results.Fresh()
	summary.RequiredKnown = pr.RequiredChecksError == ""
	for _, check := range pr.Checks {
		if check.Result() == release.No {
			summary.Failed++
			if check.Required == release.Yes {
				summary.RequiredFailed++
			}
		}
		if check.Required == "" {
			summary.RequiredKnown = false
		}
		if check.Running() {
			summary.Running++
		}
	}
	return summary
}

func undecidedCandidate(pr release.PR, branch string) bool {
	return pr.Selectable("") && pr.Evidence.Fresh() && pr.Base == branch
}

func countRequiredChecks(pr release.PR) requiredCheckCounts {
	if !pr.Evidence.Fresh() || !pr.ChecksEvidence.Fresh() {
		return requiredCheckCounts{}
	}
	var counts requiredCheckCounts
	for _, check := range pr.Checks {
		if check.Required != release.Yes {
			continue
		}
		switch {
		case check.Result() == release.No:
			counts.Failed++
		case check.Running():
			counts.Running++
		}
	}
	return counts
}

// DetachedPRRetries keeps observations visible when the corresponding PR row is unavailable.
func (p pageModel) DetachedPRRetries() pageModel {
	if p.Tab != "component-prs" {
		return p
	}
	visible := make(map[release.Target]bool)
	for _, repo := range p.Repos {
		for _, row := range repo.PRs {
			visible[row.PR.Target] = true
		}
	}
	p.Plans = slices.DeleteFunc(slices.Clone(p.Plans), func(plan planModel) bool {
		return visible[plan.Plan.Target]
	})
	p.Authority = slices.DeleteFunc(slices.Clone(p.Authority), func(view authorityModel) bool {
		return visible[view.View.Target]
	})
	return p
}
