package web

import (
	"cmp"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"release-assistant/internal/app"
	"release-assistant/internal/release"
)

type (
	rcModel struct {
		Candidate                         release.Candidate
		TagURL, ChecksURL, WorkflowURL    string
		CommitURL                         string
		CutRCURL, CutRCBlocker            string
		Plan                              *planModel
		Permission                        *tagModel
		ReadyForTag                       bool
		NextNumber                        uint64
		ValidationRows                    []rcValidationRow
		ValidationObserved                bool
		UnverifiedJobs                    []release.Run
		LookupTimedOut                    bool
		CIAvailable, CITotal              int
		KonfluxMissing, MissingAfterBuild int
	}
	rcBuildModel struct {
		Status, Class, Link, LinkLabel string
		Retry                          *rcRetryModel
	}
	rcRetryModel struct {
		Candidate *buildRetryCandidate
		Queued    bool
	}
)

func (p *pageModel) cutRCState() (link, blocker string) {
	rc := p.RC.Candidate
	milestone := rc.NextMilestone
	if rc.Evidence.Fresh() && milestone.Fresh() {
		if link := safeURL(milestone.URL); link != "" {
			return link, ""
		}
	}

	switch {
	case rc.Evidence.Error != "":
		return "", "Could not verify the current RC on GitHub. " + rc.Evidence.Error
	case !rc.Evidence.Fresh() && p.ViewingInactive:
		return "", "Activate this release to refresh RC and milestone information."
	case !rc.Evidence.Fresh():
		return "", "Waiting for GitHub to verify the current RC and release branch."
	}

	version := strings.TrimPrefix(p.Snapshot.Config.Version, "v")
	if p.ACS != nil {
		for _, tag := range p.ACS.Tags {
			if tag.Evidence.Historical {
				continue
			}
			name := strings.TrimPrefix(tag.Name, "v")
			if name == version {
				return "", "The final release tag already exists."
			}
			if suffix, ok := strings.CutPrefix(name, version+"-rc."); ok {
				if number, err := strconv.ParseUint(suffix, 10, 64); err == nil && number > rc.Number {
					return "", "A newer RC tag already exists on GitHub."
				}
			}
		}
	}
	switch {
	case rc.Target.Valid() && rc.Ahead <= 0:
		return "", "No new commits on the release branch since the current RC."
	case milestone.Error != "":
		return "", "Could not verify the next RC milestone on GitHub. " + milestone.Error
	case milestone.Fresh() && milestone.URL == "":
		return "", "GitHub has no open milestone for the next RC."
	case milestone.Fresh():
		return "", "GitHub returned an unusable milestone link."
	default:
		return "", "Waiting for GitHub to verify the next RC milestone."
	}
}

func (p *pageModel) prepareRC() {
	p.RC.Candidate = p.Snapshot.Candidate()
	p.RC.NextNumber = p.RC.Candidate.Number + 1
	target := p.RC.Candidate.Target
	repoURL := "https://github.com/" + p.Snapshot.Config.ACS().Repo
	p.RC.WorkflowURL = repoURL + "/actions/workflows/start-release.yml"
	if target.Valid() {
		p.RC.TagURL = repoURL + "/tree/refs/tags/" + url.PathEscape(target.Tag)
	}
	p.RC.CutRCURL, p.RC.CutRCBlocker = p.cutRCState()
	assessment := p.Snapshot.Assessment
	p.RC.ReadyForTag = p.RC.Candidate.Evidence.Fresh() && (p.RC.TagURL == "" || p.RC.Candidate.Ahead > 0) &&
		p.ACS != nil && !p.ACS.TagsPending && !slices.ContainsFunc(p.ACS.Tags, func(tag release.Tag) bool {
		return strings.TrimPrefix(tag.Name, "v") == strings.TrimPrefix(p.Snapshot.Config.Version, "v")
	}) &&
		assessment.Ready && !assessment.PatchMayBeUnnecessary &&
		p.TabStatuses["component-prs"].State == tabComplete &&
		p.TabStatuses["component-releases"].State == tabComplete &&
		p.TabStatuses["acs-prs"].State == tabComplete
	for _, check := range p.Snapshot.Assessment.Images {
		if check.Image.Component != "acs" {
			continue
		}
		switch check.Image.Family {
		case "ci":
			p.RC.CITotal++
			if check.Status == release.Yes {
				p.RC.CIAvailable++
			}
		case "konflux":
			if check.Status == release.No {
				p.RC.KonfluxMissing++
			}
		}
	}
	groups := p.BuildRetries()
	if len(groups) == 0 || groups[0].Plan == nil {
		return
	}
	group := groups[0]
	p.RC.Plan = group.Plan
	plan := group.Plan.Plan
	p.RC.LookupTimedOut = strings.Contains(plan.Evidence.Error, "PipelineRun identity") &&
		strings.Contains(plan.Evidence.Error, "context deadline exceeded")
	if plan.SHA == "" || plan.SHA != p.RC.Candidate.SHA {
		return
	}

	p.RC.CommitURL = repoURL + "/commit/" + url.PathEscape(plan.SHA)
	p.RC.ChecksURL = p.RC.CommitURL + "/checks"
	if group.Authority != nil && group.Authority.SHA == plan.SHA && p.AutomaticRetryPermissions() &&
		(len(plan.Proposals) > 0 || group.Authority.Granted) {
		p.RC.Permission = &tagModel{
			Tag: release.Tag{Name: target.Tag, SHA: plan.SHA}, Repo: target.Repo,
			Authority: group.Authority,
			Tab:       p.Tab, CSRF: p.CSRF, Generation: p.Snapshot.Generation, GrantAnchor: "rc-validation",
		}
	}
	runs := make(map[string]release.Run, len(plan.Runs))
	for _, run := range plan.Runs {
		runs[run.Job] = run
	}
	retries := make(map[string]*buildRetryCandidate, len(group.Candidates))
	for _, candidate := range group.Candidates {
		retries[candidate.Proposal.Job] = candidate
	}
	imageJobs := make(map[string]bool)
	for i, group := range p.ReleaseImages.Groups {
		for j, row := range group.Rows {
			if row.Konflux == nil {
				continue
			}
			job := cmp.Or(row.Konflux.Check.Image.BuildJob, row.Name+"-on-push")
			run := runs[job]
			build := rcBuild(run, p.RC.ChecksURL)
			if row.Konflux.Check.Status == release.No && build.Class == "yes" {
				p.RC.MissingAfterBuild++
			}
			if retry := retries[job]; rcRetryMatches(retry, run, plan.SHA) {
				build.Retry = new(p.rcRetry(retry))
			}
			imageJobs[job] = true
			image := *row.Konflux
			image.RCBuild = &build
			p.ReleaseImages.Groups[i].Rows[j].Konflux = &image
		}
	}
	p.prepareRCValidation(plan, runs, retries, imageJobs)
	for _, run := range plan.Runs {
		if run.Status == "unknown" && run.ID == "" && !imageJobs[run.Job] {
			p.RC.UnverifiedJobs = append(p.RC.UnverifiedJobs, run)
		}
	}
}

func rcRetryMatches(candidate *buildRetryCandidate, run release.Run, sha string) bool {
	check := release.Check{Status: run.Status, Conclusion: run.Outcome}
	return candidate != nil && run.ID != "" && run.Eligible && check.Result() == release.No &&
		candidate.Proposal.SHA == sha && candidate.Proposal.RunID == run.ID
}

func (p pageModel) rcRetry(candidate *buildRetryCandidate) rcRetryModel {
	return rcRetryModel{
		Candidate: candidate,
		Queued: candidate.Offer != nil && candidate.Offer.Queueable ||
			slices.ContainsFunc(p.Snapshot.RetestQueue, func(item app.RetestItem) bool {
				return item.MatchesProposal(candidate.Proposal)
			}),
	}
}

func rcBuild(run release.Run, checksURL string) rcBuildModel {
	build := rcBuildModel{Status: "No verified build", Class: "unknown", Link: checksURL, LinkLabel: "View tag checks"}
	if run.ID == "" {
		if run.Status == "unknown" {
			build.Status = "Build unverified"
			if run.URL != "" {
				build.Link, build.LinkLabel = run.URL, "View unverified check"
			}
		}
		return build
	}
	check := release.Check{Status: run.Status, Conclusion: run.Outcome}
	if run.URL != "" {
		build.Link, build.LinkLabel = run.URL, "Open in Konflux"
	}
	switch {
	case check.Result() == release.No:
		build.Status, build.Class = "Build failed", "no"
	case check.Running():
		build.Status = "Build queued"
		if run.Status == "in_progress" {
			build.Status = "Build running"
		}
	case run.Status == "completed" && run.Outcome == "success":
		build.Status, build.Class = "Build succeeded", "yes"
		if run.URL != "" {
			build.LinkLabel = "Check build output"
		}
	default:
		build.Status = "Build status unknown"
	}
	return build
}

// CIIn returns the CI checks for the namespace named by the table column.
func (row componentImageRow) CIIn(namespace string) []imageModel {
	var checks []imageModel
	for _, image := range row.CI {
		if image.Namespace == namespace {
			checks = append(checks, image)
		}
	}
	return checks
}

// LastChecked reports the most recent image check attempt in the row.
func (row componentImageRow) LastChecked() time.Time {
	var latest time.Time
	for _, image := range row.CI {
		if at := image.Check.Image.Evidence.At; at.After(latest) {
			latest = at
		}
	}
	if row.Konflux != nil && row.Konflux.Check.Image.Evidence.At.After(latest) {
		latest = row.Konflux.Check.Image.Evidence.At
	}
	return latest
}
