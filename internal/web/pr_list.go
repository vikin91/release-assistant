package web

import (
	"cmp"
	"slices"

	"release-assistant/internal/release"
)

type (
	prListModel struct {
		Repos                                    []repoModel
		Rows                                     map[release.Target]prListRow
		AwaitingMerge, Closed, Undecided, Merged int
	}
	prListRow struct {
		Checks      requiredCheckCounts
		Attention   bool
		Retry       *pageModel
		HasProposal bool
	}
	requiredCheckCounts struct {
		Verified                            bool
		Failed, Running, Passed, Unverified int
	}
)

// PRList groups PRs by repository and summarizes current check evidence.
func (p pageModel) PRList() prListModel {
	triage := prListModel{
		Repos: slices.Clone(p.Repos),
		Rows:  make(map[release.Target]prListRow),
	}
	var retries map[release.Target]*pageModel
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
			return cmp.Compare(prListRank(a), prListRank(b))
		})
		for _, row := range repo.PRs {
			closed := row.PR.State == "closed"
			switch {
			case row.Choice == release.Included && closed:
				triage.Closed++
			case row.Choice == release.Included:
				triage.AwaitingMerge++
			case row.Choice == "":
				triage.Undecided++
			}
			retry := retries[row.PR.Target]
			view := prListRow{Retry: retry}
			if row.Choice == release.Included {
				view.Checks = countRequiredChecks(row.PR)
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

func prListRank(row prModel) int {
	switch {
	case row.Choice == release.Included && row.PR.State == "closed":
		return 0
	case row.Choice == release.Included:
		return 1
	case row.Choice == "":
		return 2
	default:
		return 3
	}
}

func countRequiredChecks(pr release.PR) requiredCheckCounts {
	if !pr.Evidence.Fresh() || !pr.ChecksEvidence.Fresh() {
		return requiredCheckCounts{}
	}
	counts := requiredCheckCounts{Verified: true}
	for _, check := range pr.Checks {
		if check.Required != release.Yes {
			if check.Required != release.No {
				counts.Unverified++
			}
			continue
		}
		result := check.Result()
		switch {
		case result == release.No:
			counts.Failed++
		case result == release.Yes:
			counts.Passed++
		case check.Status == "pending" || check.Status == "queued" || check.Status == "in_progress" ||
			check.Status == "requested" || check.Status == "waiting":
			counts.Running++
		default:
			counts.Unverified++
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
