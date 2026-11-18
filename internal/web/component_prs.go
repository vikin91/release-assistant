package web

import (
	"cmp"
	"fmt"
	"slices"

	"release-assistant/internal/release"
)

type (
	componentPRTriage struct {
		Repos                                    []repoModel
		Rows                                     map[release.Target]componentPRRow
		AwaitingMerge, Closed, Undecided, Merged int
	}
	componentPRRow struct {
		CheckSummary string
		CheckStatus  release.Truth
		Attention    bool
		Retry        *pageModel
		HasProposal  bool
	}
)

// ComponentPRTriage groups PRs by component and summarizes current check evidence.
func (p pageModel) ComponentPRTriage() componentPRTriage {
	triage := componentPRTriage{Repos: p.Repos}
	if p.Tab != "component-prs" {
		return triage
	}
	triage.Repos = slices.Clone(p.Repos)
	triage.Rows = make(map[release.Target]componentPRRow)
	retries := make(map[release.Target]*pageModel)
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
	for i, repo := range triage.Repos {
		triage.Merged += len(repo.MergedPRs)
		triage.Repos[i].PRs = slices.Clone(repo.PRs)
		slices.SortStableFunc(triage.Repos[i].PRs, func(a, b prModel) int {
			return cmp.Compare(componentPRRank(a), componentPRRank(b))
		})
		for _, row := range repo.PRs {
			summary, status := componentPRChecks(row.PR)
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
			view := componentPRRow{
				CheckSummary: summary,
				CheckStatus:  status,
				Attention:    row.Choice == release.Included && (closed || status == release.No),
				Retry:        retry,
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

func componentPRRank(row prModel) int {
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

func componentPRChecks(pr release.PR) (string, release.Truth) {
	if !pr.Evidence.Fresh() || !pr.ChecksEvidence.Fresh() {
		return "Checks unverified", ""
	}
	var failed, running int
	var required, unknown bool
	for _, check := range pr.Checks {
		if check.Required == "" {
			unknown = true
		}
		if check.Required != release.Yes {
			continue
		}
		required = true
		result := check.Result()
		switch {
		case result == release.No:
			failed++
		case result == release.Yes:
		case check.Status == "pending" || check.Status == "queued" || check.Status == "in_progress" ||
			check.Status == "requested" || check.Status == "waiting":
			running++
		default:
			unknown = true
		}
	}
	switch {
	case failed == 1:
		return "1 required check failed", release.No
	case failed > 1:
		return fmt.Sprintf("%d required checks failed", failed), release.No
	case unknown:
		return "Checks unverified", ""
	case running > 0:
		return "Required checks pending", ""
	case required:
		return "Required checks passed", release.Yes
	default:
		return "No required checks reported", ""
	}
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
