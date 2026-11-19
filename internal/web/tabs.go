package web

import "release-assistant/internal/release"

func validTab(tab string) string {
	switch tab {
	case "component-releases", "acs-prs", "acs-rcs", "activity", "settings":
		return tab
	default:
		return "component-prs"
	}
}

func tabTitle(tab string) string {
	switch tab {
	case "settings":
		return "Settings"
	case "activity":
		return "Log"
	case "component-releases":
		return "Component releases"
	case "acs-prs":
		return "ACS PRs"
	case "acs-rcs":
		return "ACS RCs"
	default:
		return "Component PRs"
	}
}

func tabStep(tab string) string {
	switch tab {
	case "component-releases":
		return "2 · Release components"
	case "acs-prs":
		return "3 · Prepare the ACS branch"
	case "acs-rcs":
		return "4 · Verify the release candidate"
	default:
		return "1 · Prepare component branches"
	}
}

func tabHelp(tab string) string {
	switch tab {
	case "acs-prs":
		return "Choose component versions in the Component releases tab, then use the pinning details here to verify which versions will ship. Review open pull requests targeting the ACS release branch. Include PRs that belong in this patch and exclude those that can wait for the next one. Follow the CI and Konflux checks on included PRs and use the available retry commands to rerun failed jobs. Once the required checks pass, merge the PRs in GitHub."
	case "acs-rcs":
		return "Follow the current RC builds and verify their published images."
	default:
		return ""
	}
}

// tabPage scopes presentation while all activities share the same release session.
func tabPage(page pageModel, tab string) pageModel {
	page.Tab = tab
	page.PRSection = "pr-selection"
	page.RetrySection = "retry-proposals"
	page.RetryDetails = "retry-details"
	page.OfferSection = "comment-offers"
	if tab != "component-prs" {
		page.PRSection = tab + "-pr-selection"
		page.RetrySection = tab + "-retry"
		page.RetryDetails = tab + "-retry-details"
	}
	acsRepo := page.Snapshot.Config.ACS().Repo
	matchesTarget := func(target release.Target) bool {
		switch tab {
		case "component-releases":
			return target.Tag != "" && target.Repo != acsRepo
		case "acs-prs":
			return target.PR > 0 && target.Repo == acsRepo
		case "component-prs":
			return target.PR > 0 && target.Repo != acsRepo
		case "acs-rcs":
			return target == page.Snapshot.Candidate().Target
		default:
			return false
		}
	}
	repos := make([]repoModel, 0, len(page.Repos))
	for _, repo := range page.Repos {
		if tab == "acs-prs" && repo.Repository.Source.Name == "acs" || tab == "component-prs" && repo.Repository.Source.Name != "acs" {
			repos = append(repos, repo)
		}
	}
	page.Repos = repos
	plans := make([]planModel, 0, len(page.Plans))
	for _, plan := range page.Plans {
		if matchesTarget(plan.Plan.Target) {
			plans = append(plans, plan)
		}
	}
	page.Plans = plans
	page.StartFailureClusters, page.EarlierStartFailureClusters = groupStartFailures(plans, tab)
	authority := make([]authorityModel, 0, len(page.Authority))
	for _, view := range page.Authority {
		if matchesTarget(view.View.Target) {
			authority = append(authority, view)
		}
	}
	page.Authority = authority
	problems := make([]release.Condition, 0, len(page.Problems))
	for _, problem := range page.Problems {
		for target := range page.Snapshot.Choices.PRs {
			if problem.Name == "included PR "+target.String() && matchesTarget(target) {
				problems = append(problems, problem)
				break
			}
		}
	}
	page.Problems = problems
	switch tab {
	case "acs-rcs":
		page.prepareRC()
	case "component-releases":
		page.Guidance = page.Snapshot.Assessment.ComponentReleaseRecommendations
	case "acs-prs":
		page.Guidance = page.Snapshot.Assessment.ACSRecommendations
	default:
		page.Guidance = page.Snapshot.Assessment.ComponentPRRecommendations
		if tab == "component-prs" {
			page.ComponentPRNotice = page.Snapshot.Assessment.ComponentPRPreparation()
			if last := len(page.Guidance) - 1; last >= 0 && page.Guidance[last] == page.ComponentPRNotice {
				page.Guidance = page.Guidance[:last]
			}
		}
	}
	return page
}
