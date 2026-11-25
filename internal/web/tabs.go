package web

import (
	"slices"
	"strings"

	"release-assistant/internal/release"
)

func validTab(tab string) string {
	if prComponent(tab) != "" {
		return tab
	}
	switch tab {
	case "component-releases", "acs-prs", "acs-rcs", "activity", "retest-queue", "settings":
		return tab
	default:
		return "component-prs"
	}
}

func prComponent(tab string) string {
	if name, ok := strings.CutSuffix(tab, "-prs"); ok {
		switch name {
		case "collector", "fact", "scanner":
			return name
		}
	}
	return ""
}

// ComponentPRTabs returns the PR views for the configured components in sidebar order.
func (p pageModel) ComponentPRTabs() []string {
	var tabs []string
	if p.Configured {
		for _, name := range []string{"collector", "fact", "scanner"} {
			if _, ok := p.ConfiguredSources[name]; ok {
				tabs = append(tabs, name+"-prs")
			}
		}
	}
	return tabs
}

// ReleasePhaseTabs orders the workflow panels to match the sidebar.
func (p pageModel) ReleasePhaseTabs() []string {
	tabs := append([]string{"component-prs"}, p.ComponentPRTabs()...)
	return append(tabs, "component-releases", "acs-prs", "acs-rcs")
}

func (p pageModel) availableTab(tab string) string {
	tab = validTab(tab)
	if prComponent(tab) != "" && !slices.Contains(p.ComponentPRTabs(), tab) {
		return "component-prs"
	}
	return tab
}

func tabTitle(tab string) string {
	switch tab {
	case "settings":
		return "Settings"
	case "activity":
		return "Log"
	case "retest-queue":
		return "Retest Queue"
	case "component-releases":
		return "Component releases"
	case "acs-prs":
		return "ACS PRs"
	case "acs-rcs":
		return "ACS RCs"
	case "collector-prs":
		return "Collector PRs"
	case "fact-prs":
		return "Fact PRs"
	case "scanner-prs":
		return "Scanner PRs"
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
		return "Pinning details compares the selected component versions with the ACS release branch and the pinning PR. Including an ACS PR tracks its required checks and merge."
	default:
		return ""
	}
}

// tabPage scopes presentation while all activities share the same release session.
func tabPage(page pageModel, tab string) pageModel {
	page.Tab = tab
	page.PRSection = tab + "-pr-selection"
	page.RetrySection = tab + "-retry"
	page.RetryDetails = tab + "-retry-details"
	component := prComponent(tab)
	if component != "" {
		page.PRSection = componentPRSection(component)
	}
	repos := make([]repoModel, 0, len(page.Repos))
	for _, repo := range page.Repos {
		name := repo.Repository.Source.Name
		if (tab == "acs-prs" && name == "acs") || (component != "" && name == component) {
			repos = append(repos, repo)
		}
	}
	page.Repos = repos
	acsRepo := page.Snapshot.Config.ACS().Repo
	matchesTarget := func(target release.Target) bool {
		if component != "" {
			return target.PR > 0 && slices.ContainsFunc(repos, func(repo repoModel) bool {
				return repo.Repository.Source.Repo == target.Repo
			})
		}
		switch tab {
		case "component-releases":
			return target.Tag != "" && target.Repo != acsRepo
		case "acs-prs":
			return target.PR > 0 && target.Repo == acsRepo
		case "acs-rcs":
			return target == page.Snapshot.Candidate().Target
		default:
			return false
		}
	}
	plans := make([]planModel, 0, len(page.Plans))
	for _, plan := range page.Plans {
		if matchesTarget(plan.Plan.Target) {
			plans = append(plans, plan)
		}
	}
	page.Plans = plans
	for _, report := range page.Snapshot.ManualRetries {
		if matchesTarget(report.Target) {
			page.ManualRetries = append(page.ManualRetries, report)
		}
	}
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
	case "component-prs":
		page.ComponentPRNotice = page.Snapshot.Assessment.ComponentPRPreparation()
	}
	return page
}
