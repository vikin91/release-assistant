package web

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"release-assistant/internal/release"
)

type (
	tabState  int
	tabStatus struct {
		State  tabState
		Reason string
	}
)

const (
	tabComplete tabState = iota + 1
	tabPending
	tabInProgress
	tabActionNeeded
	tabNeedsAttention
)

const imagePublishingGrace = 15 * time.Minute

// Label names the status for assistive technology and the tab's tooltip.
func (s tabStatus) Label() string {
	switch s.State {
	case tabComplete:
		return "Complete"
	case tabInProgress:
		return "In progress"
	case tabActionNeeded:
		return "Action needed"
	case tabNeedsAttention:
		return "Needs attention"
	default:
		return ""
	}
}

// Icon gives each visible status a distinct shape.
func (s tabStatus) Icon() string {
	switch s.State {
	case tabComplete:
		return "✓"
	case tabInProgress:
		return "◔"
	case tabActionNeeded:
		return "→"
	case tabNeedsAttention:
		return "⚠︎"
	default:
		return ""
	}
}

// Class styles the icon independently of the selected tab.
func (s tabStatus) Class() string {
	switch s.State {
	case tabComplete:
		return "complete"
	case tabInProgress:
		return "in-progress"
	case tabActionNeeded:
		return "action-needed"
	case tabNeedsAttention:
		return "needs-attention"
	default:
		return ""
	}
}

// PhaseLegend pairs the sidebar symbols with guidance for the release engineer.
func (pageModel) PhaseLegend() []tabStatus {
	return []tabStatus{
		{tabActionNeeded, "Take the next step."},
		{tabNeedsAttention, "Investigate a problem."},
		{tabInProgress, "Work is underway."},
		{tabComplete, "Phase requirements are met."},
	}
}

// add keeps the highest-priority status and its reason.
func (s *tabStatus) add(state tabState, reason string) {
	if state > s.State {
		*s = tabStatus{State: state, Reason: reason}
	}
}

func (p pageModel) workflowStatuses() map[string]tabStatus {
	componentPRs := p.prStatus("")
	acsPRs := p.prStatus("acs")
	pins := p.pinStatus()
	acsPRs.add(pins.State, pins.Reason)
	if acsPRs.State == tabComplete {
		acsPRs.Reason = "All PR decisions are made, included PRs are merged, and the ACS branch pins match."
	}
	statuses := map[string]tabStatus{
		"component-prs":      componentPRs,
		"component-releases": p.componentReleaseStatus(componentPRs.State == tabComplete),
		"acs-prs":            acsPRs,
		"acs-rcs":            p.rcStatus(),
	}
	for _, tab := range p.ComponentPRTabs() {
		statuses[tab] = p.prStatus(prComponent(tab))
	}
	return statuses
}

func (p pageModel) prStatus(scope string) tabStatus {
	status := tabStatus{State: tabComplete, Reason: "All PR decisions are made and included PRs are merged."}
	pin := p.Snapshot.Config.PinningTarget()
	undecided := 0
	seen := make(map[release.Target]bool)
	listChecked := make(map[string]bool)
	var checks, inventory tabStatus
	var componentRepo string
	for _, repo := range p.Repos {
		source := repo.Repository.Source
		if (scope == "" && source.Name == "acs") || (scope != "" && source.Name != scope) {
			continue
		}
		componentRepo = source.Repo
		evidence := repo.Repository.PRListEvidence
		listChecked[source.Repo] = !evidence.At.IsZero()
		if evidence.At.IsZero() {
			evidence = repo.Repository.Evidence
		}
		switch {
		case evidence.Error != "" && listChecked[source.Repo]:
			inventory.add(tabNeedsAttention, "Could not list open PRs for "+source.Repo+".")
		case evidence.Error != "":
			inventory.add(tabNeedsAttention, "GitHub observations for "+source.Repo+" are incomplete.")
		case !evidence.Fresh():
			inventory.add(tabPending, "")
		}
		for _, pr := range repo.Repository.PRs {
			choice := p.Snapshot.Choices.PRs[pr.Target]
			if choice == "" && undecidedCandidate(pr, source.Branch) {
				undecided++
			}
			if choice == release.Included {
				seen[pr.Target] = true
				item := pullRequestStatus(pr, source.Branch)
				checks.add(item.State, item.Reason)
			}
		}
	}
	for target, choice := range p.Snapshot.Choices.PRs {
		if choice != release.Included || target.PR <= 0 || seen[target] {
			continue
		}
		inScope := target.Repo == componentRepo
		if scope == "" || scope == "acs" {
			inScope = (target.Repo == p.Snapshot.Config.ACS().Repo) == (scope == "acs")
		}
		if inScope {
			if scope == "acs" && target == pin && p.PinningPR != nil {
				item := pullRequestStatus(p.PinningPR.PR, p.Snapshot.Config.Branch)
				checks.add(item.State, item.Reason)
			} else if listChecked[target.Repo] {
				checks.add(tabNeedsAttention, fmt.Sprintf("Could not verify included PR #%d.", target.PR))
			} else {
				status.add(tabPending, "")
			}
		}
	}
	if undecided == 1 {
		status.add(tabActionNeeded, "Decide whether to include 1 PR.")
	} else if undecided > 1 {
		status.add(tabActionNeeded, fmt.Sprintf("Decide whether to include %d PRs.", undecided))
	}
	status.add(checks.State, checks.Reason)
	status.add(inventory.State, inventory.Reason)
	return status
}

func pullRequestStatus(pr release.PR, branch string) tabStatus {
	id := fmt.Sprintf("PR #%d", pr.Target.PR)
	checks := countRequiredChecks(pr)
	switch {
	case pr.Evidence.Error != "":
		return tabStatus{State: tabNeedsAttention, Reason: "Unable to verify " + id + "."}
	case !pr.Evidence.Fresh() || pr.Base == "":
		return tabStatus{State: tabPending}
	case pr.Base != branch:
		return tabStatus{State: tabNeedsAttention, Reason: id + " targets a different branch."}
	case pr.Merged && pr.MergeSHA != "":
		return tabStatus{State: tabComplete}
	case pr.State == "closed" && !pr.Merged:
		return tabStatus{State: tabNeedsAttention, Reason: id + " closed without merging."}
	case pr.State != "open" || pr.Merged:
		return tabStatus{State: tabPending}
	case checks.Failed > 0:
		return tabStatus{State: tabNeedsAttention, Reason: id + " has failed required checks."}
	case pr.ChecksEvidence.Error != "":
		return tabStatus{State: tabNeedsAttention, Reason: "Unable to verify required checks for " + id + "."}
	case checks.Running > 0:
		return tabStatus{State: tabInProgress, Reason: "Waiting for required checks on " + id + "."}
	case pr.Draft:
		return tabStatus{State: tabInProgress, Reason: "Waiting for " + id + " to leave draft."}
	case pr.ReviewRequested:
		return tabStatus{State: tabInProgress, Reason: "Waiting for a requested review on " + id + "."}
	case pr.ChecksStatus() == release.Yes:
		return tabStatus{State: tabActionNeeded, Reason: "Review " + id + " for merge."}
	default:
		return tabStatus{State: tabPending}
	}
}

func (p pageModel) componentReleaseStatus(prsComplete bool) tabStatus {
	status := tabStatus{State: tabComplete, Reason: "Selected component tags are verified and required images are available."}
	for _, repo := range p.TagRepos {
		item := p.componentTagStatus(repo, prsComplete)
		status.add(item.State, item.Reason)
	}
	return status
}

func (p pageModel) componentTagStatus(repo repoModel, prsComplete bool) tabStatus {
	source := repo.Repository.Source
	name := source.Name
	if repo.Repository.Evidence.Error != "" {
		return tabStatus{State: tabNeedsAttention, Reason: "Unable to verify tags for " + name + "."}
	}
	if repo.CurrentTag == "" {
		if p.Snapshot.Assessment.ProposedTags[name] != "" {
			return tabStatus{State: tabActionNeeded, Reason: "Select a component tag for " + name + "."}
		}
		if prsComplete && repo.Repository.Evidence.Fresh() && !repo.Repository.TagsPending {
			return tabStatus{State: tabActionNeeded, Reason: "Cut a component tag for " + name + " and start its release."}
		}
		return tabStatus{State: tabPending}
	}
	status := tabStatus{State: tabComplete}
	switch {
	case repo.Repository.CommitGraphLimit > 0 && repo.SelectedTag != nil &&
		repo.SelectedTag.Tag.Evidence.Fresh() && repo.SelectedTag.Tag.Belongs == release.Yes:
		// A coverage limit says nothing about the selected tag's identity or build.
	case repo.Coverage.Status == release.No && repo.Coverage.TagIssue != release.TagPRUnmerged:
		status.add(tabNeedsAttention, repo.Coverage.Reason)
	case repo.Coverage.Status != release.Yes:
		status.add(tabPending, "")
	}

	images := p.componentImageStatus(name, repo.CurrentTag)
	if images.State == tabComplete || repo.SelectedTag == nil || !repo.SelectedTag.Tag.Evidence.Fresh() {
		status.add(images.State, images.Reason)
		return status
	}

	target := release.Target{Repo: source.Repo, Tag: repo.CurrentTag}
	for _, plan := range p.Snapshot.Plans {
		if plan.Target != target || plan.SHA == "" || plan.SHA != repo.SelectedTag.Tag.SHA {
			continue
		}
		result, running := plan.BuildStatus()
		switch {
		case result == release.No:
			status.add(tabNeedsAttention, "A build failed for "+name+" "+repo.CurrentTag+".")
		case running:
			status.add(tabInProgress, "Waiting for builds for "+name+" "+repo.CurrentTag+".")
		case result == release.Yes && images.State == tabPending:
			status.add(tabInProgress, "Waiting for images for "+name+" "+repo.CurrentTag+".")
		case plan.Evidence.Error != "":
			status.add(tabNeedsAttention, "Unable to verify builds for "+name+" "+repo.CurrentTag+".")
		}
	}
	status.add(images.State, images.Reason)
	return status
}

// componentImageStatus considers registry results only for the selected release and tag.
func (p pageModel) componentImageStatus(name, tag string) tabStatus {
	inventory := p.Snapshot.Inventory
	unobserved := tabStatus{State: tabInProgress, Reason: "Waiting for image checks for " + name + " " + tag + "."}
	if inventory.Evidence.Error != "" {
		unobserved = tabStatus{State: tabNeedsAttention, Reason: "Unable to verify component image checks: " + inventory.Evidence.Error}
	}
	if inventory.ReleaseVersion != strings.TrimPrefix(p.Snapshot.Config.Version, "v") ||
		inventory.Versions[name] != strings.TrimPrefix(tag, "v") {
		return unobserved
	}

	var images tabStatus
	for _, image := range p.Snapshot.Assessment.Images {
		if !image.Required || image.Image.Component != name || image.SelectedTag != tag {
			continue
		}
		item := publishingImageStatus(image.Image, image.Status)
		if image.Status == "" && image.Image.Evidence.At.IsZero() && image.Image.Evidence.Error == "" {
			item = unobserved
		}
		images.add(item.State, item.Reason)
	}
	if images.State == 0 {
		return unobserved
	}
	return images
}

func (p pageModel) pinStatus() tabStatus {
	pins := p.Snapshot.Assessment.Pins
	if pins.BranchStatus == release.Yes {
		return tabStatus{State: tabComplete}
	}
	if p.ACS == nil {
		return tabStatus{State: tabPending}
	}
	if p.ACS.Pins.Evidence.Error != "" || p.ACS.PinningEvidence.Error != "" {
		return tabStatus{State: tabNeedsAttention, Reason: "Unable to verify ACS pinning."}
	}
	if p.PinningPR != nil {
		if p.ACS.PinningPins.Evidence.Error != "" {
			return tabStatus{State: tabNeedsAttention, Reason: "Unable to verify the versions in the pinning PR."}
		}
		pr := p.PinningPR.PR
		if pr.Evidence.Fresh() && pr.Merged {
			if pins.BranchStatus == release.No {
				return tabStatus{State: tabNeedsAttention, Reason: "ACS branch pins still differ after the pinning PR merged."}
			}
			return tabStatus{State: tabPending}
		}
		status := pullRequestStatus(pr, p.Snapshot.Config.Branch)
		if status.State == tabNeedsAttention {
			return status
		}
		switch {
		case pins.PROpen && pins.PRStatus == release.No:
			if p.Snapshot.Assessment.PinningRecommendation != "" {
				return tabStatus{State: tabActionNeeded, Reason: "Update the pinning PR with the selected component versions."}
			}
			return tabStatus{State: tabPending}
		case !pins.PROpen || !p.ACS.PinningPins.Evidence.Fresh() || len(pins.PRProblems) > 0:
			return tabStatus{State: tabPending}
		}
		return status
	}
	if p.Snapshot.Assessment.PinningRecommendation != "" {
		return tabStatus{State: tabActionNeeded, Reason: "Create an ACS pinning PR for the selected component versions."}
	}
	return tabStatus{State: tabPending}
}

func (p pageModel) rcStatus() tabStatus {
	snapshot := p.Snapshot
	images := snapshot.Inventory.ReleaseImages
	candidate := snapshot.Candidate()
	target := candidate.Target
	if snapshot.RCImageWaitReason() != "" {
		if candidate.Evidence.Error != "" {
			return tabStatus{State: tabNeedsAttention, Reason: "Unable to discover the current RC tag."}
		}
		return tabStatus{State: tabPending}
	}
	version := strings.TrimPrefix(target.Tag, "v")
	if len(images) == 0 || snapshot.Inventory.Versions["acs"] != version {
		return p.rcBuildStatus(target)
	}
	status := tabStatus{State: tabComplete}
	started := slices.ContainsFunc(images, func(image release.Image) bool {
		return image.Evidence.Fresh() && image.Present == release.Yes
	})
	for _, image := range images {
		if !image.Evidence.Fresh() {
			image.Present = ""
		}
		item := publishingImageStatus(image, image.Present)
		if item.State == tabPending && started && image.Present == release.No {
			item = tabStatus{State: tabInProgress, Reason: "Waiting for the remaining ACS RC images."}
		}
		status.add(item.State, item.Reason)
	}
	if status.State != tabComplete {
		build := p.rcBuildStatus(target)
		status.add(build.State, build.Reason)
		return status
	}
	rc := tabPage(p, "acs-rcs").RC
	if rc.CommitURL != "" {
		return rc.validationStatus()
	}
	if rc.Plan != nil && rc.Plan.Plan.Evidence.Error != "" {
		return tabStatus{State: tabNeedsAttention, Reason: "Images are available. Unable to verify the RC build checks."}
	}
	return tabStatus{State: tabInProgress, Reason: "Images are available. Waiting for RC build checks."}
}

func (p pageModel) rcBuildStatus(target release.Target) tabStatus {
	status := tabStatus{State: tabPending}
	for _, plan := range p.Plans {
		if plan.Plan.Target != target {
			continue
		}
		for _, run := range plan.Plan.Runs {
			if run.ID == "" {
				continue
			}
			check := release.Check{Status: run.Status, Conclusion: run.Outcome}
			proposed := slices.ContainsFunc(plan.Plan.Proposals, func(proposal release.Proposal) bool {
				return proposal.Job == run.Job && proposal.RunID == run.ID && proposal.SHA == plan.Plan.SHA
			})
			switch {
			case check.Result() == release.No && proposed:
				status.add(tabActionNeeded, "A failed RC Konflux build can be retested.")
			case check.Result() == release.No:
				status.add(tabNeedsAttention, "An RC Konflux build failed.")
			case check.Running():
				status.add(tabInProgress, "RC Konflux builds are running or queued.")
			}
		}
	}
	return status
}

func publishingImageStatus(image release.Image, result release.Truth) tabStatus {
	switch {
	case result == release.Yes:
		return tabStatus{State: tabComplete}
	case image.Evidence.Error != "":
		return tabStatus{State: tabNeedsAttention, Reason: "Unable to verify image " + image.Reference + "."}
	case result == release.No && !image.PublishingSince.IsZero():
		if image.Evidence.At.Sub(image.PublishingSince) >= imagePublishingGrace {
			return tabStatus{State: tabNeedsAttention, Reason: "Image is still missing: " + image.Reference + "."}
		}
		return tabStatus{State: tabInProgress, Reason: "Waiting for image " + image.Reference + " to publish."}
	default:
		return tabStatus{State: tabPending}
	}
}
