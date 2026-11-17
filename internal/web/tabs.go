package web

import "release-assistant/internal/release"

func validTab(tab string) string {
	switch tab {
	case "component-releases", "acs-prs", "acs-rcs":
		return tab
	default:
		return "component-prs"
	}
}

func tabTitle(tab string) string {
	switch tab {
	case "component-releases":
		return "Component releases"
	case "acs-prs":
		return "ACS PRs and pinning"
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
		return "4 · Inspect release candidate images"
	default:
		return "1 · Prepare component branches"
	}
}

func tabHelp(tab string) string {
	switch tab {
	case "component-releases":
		return "Cut component tags, trigger their releases, and follow builds and images. You can select a newer tag whenever more changes are ready."
	case "acs-prs":
		return "Pin the selected component versions and merge ACS backports and dependency updates. Keep adding PRs after pinning is green or merged."
	case "acs-rcs":
		return "Inspect the existing RC.0 image checks. Component and ACS PR monitoring continue while release candidate images build."
	default:
		return "Choose the changes for this patch, help their checks pass, and merge them into the component release branches. No component tags are required here."
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
		page.OfferSection = tab + "-offers"
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
	case "component-releases":
		page.Guidance = page.Snapshot.Assessment.ComponentReleaseRecommendations
	case "acs-prs":
		page.Guidance = page.Snapshot.Assessment.ACSRecommendations
	default:
		page.Guidance = page.Snapshot.Assessment.ComponentPRRecommendations
	}
	return page
}
