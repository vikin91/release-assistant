package web

import (
	"cmp"
	"fmt"
	"slices"

	"release-assistant/internal/release"
)

func componentPRSection(component string) string {
	return component + "-pr-selection"
}

type (
	prListModel struct {
		PinDiscoveryStatus                       string
		Repos                                    []repoModel
		Rows                                     map[release.Target]prListRow
		AwaitingMerge, Closed, Undecided, Merged int
		Open, Selectable, Ignored                int
	}
	prListRow struct {
		PinRole, PinReason string
		Checks             prCheckSummary
		MergeStatus        release.Truth
		Attention          bool
	}
	prCheckSummary struct {
		URL, Problem                                     string
		Available, Partial, RequiredKnown                bool
		Failed, RequiredFailed, Running, RequiredRunning int
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
			view := prListRow{}
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
			if row.PR.Target.Repo == repo.Repository.Source.Repo && row.PR.Target != p.Snapshot.Config.PinningTarget() &&
				row.PR.Selectable(row.Choice) {
				triage.Selectable++
			}
			closed := row.PR.State == "closed"
			switch {
			case row.Choice == release.Included && closed:
				triage.Closed++
			case row.Choice == release.Included:
				triage.AwaitingMerge++
			case row.Choice == "" && undecidedCandidate(row.PR, repo.Repository.Source.Branch):
				triage.Undecided++
			}
			if row.Choice == release.Included {
				view.Checks = summarizePRChecks(row.PR)
				view.Attention = closed || view.Checks.Failed > 0 || view.MergeStatus == release.No
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
	if !pr.Evidence.Fresh() && !pr.Evidence.Historical {
		summary.Problem = pr.Evidence.Error
		return summary
	}
	results := pr.CheckResultsEvidence
	available := !results.At.IsZero() && results.Known
	if results.At.IsZero() {
		results, available = pr.ChecksEvidence, pr.ChecksEvidence.Fresh() || pr.ChecksEvidence.Historical
	}
	summary.Problem = results.Error
	if !available || len(pr.Checks) == 0 {
		if available && summary.Problem == "" {
			summary.Problem = "GitHub has not reported checks for this PR head."
		}
		return summary
	}
	summary.Available = true
	summary.Partial = !results.Fresh() && !results.Historical
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
			if check.Required == release.Yes {
				summary.RequiredRunning++
			}
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
