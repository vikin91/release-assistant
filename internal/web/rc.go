package web

import (
	"net/url"

	"release-assistant/internal/release"
)

type (
	rcModel struct {
		TagURL, ChecksURL, WorkflowURL string
		Plan                           *planModel
		Permission                     *tagModel
		OtherRetries                   []rcRetryModel
	}
	rcBuildModel struct {
		Status, Class, Link, LinkLabel string
		Retry                          *rcRetryModel
	}
	rcRetryModel struct {
		Candidate *buildRetryCandidate
		Actions   *retryOfferActions
	}
)

func (p *pageModel) prepareRC() {
	target := p.Snapshot.Config.RC0Target()
	repoURL := "https://github.com/" + target.Repo
	p.RC.WorkflowURL = repoURL + "/actions/workflows/start-release.yml"
	groups := p.BuildRetries()
	if len(groups) == 0 || groups[0].Plan == nil {
		return
	}
	group := groups[0]
	p.RC.Plan = group.Plan
	plan := group.Plan.Plan
	if plan.SHA == "" {
		return
	}

	p.RC.TagURL = repoURL + "/tree/refs/tags/" + url.PathEscape(target.Tag)
	p.RC.ChecksURL = repoURL + "/commit/" + url.PathEscape(plan.SHA) + "/checks"
	if group.Authority != nil && group.Authority.SHA == plan.SHA && p.Snapshot.WritesEnabled &&
		(len(plan.Proposals) > 0 || group.Authority.Granted) {
		p.RC.Permission = &tagModel{
			Tag: release.Tag{Name: target.Tag, SHA: plan.SHA}, Repo: target.Repo,
			Authority: group.Authority, WritesEnabled: p.Snapshot.WritesEnabled,
			Tab: p.Tab, CSRF: p.CSRF, Generation: p.Snapshot.Generation,
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
	for i, group := range p.ReleaseImages.Groups {
		for j, row := range group.Rows {
			if row.Konflux == nil {
				continue
			}
			job := row.Name + "-on-push"
			run := runs[job]
			build := rcBuild(run, p.RC.ChecksURL)
			if retry := retries[job]; retry != nil && retry.Proposal.SHA == plan.SHA && retry.Proposal.RunID == run.ID && run.Eligible {
				build.Retry = new(p.rcRetry(retry))
			}
			delete(retries, job)
			image := *row.Konflux
			image.RCBuild = &build
			p.ReleaseImages.Groups[i].Rows[j].Konflux = &image
		}
	}
	for _, candidate := range group.Candidates {
		run := runs[candidate.Proposal.Job]
		if retries[run.Job] == candidate && candidate.Proposal.SHA == plan.SHA &&
			run.ID != "" && candidate.Proposal.RunID == run.ID && run.Eligible {
			p.RC.OtherRetries = append(p.RC.OtherRetries, p.rcRetry(candidate))
		}
	}
}

func (p pageModel) rcRetry(candidate *buildRetryCandidate) rcRetryModel {
	retry := rcRetryModel{Candidate: candidate}
	if p.Snapshot.WritesEnabled && candidate.Offer != nil {
		retry.Actions = new(p.RetryOfferActions(*candidate.Offer))
	}
	return retry
}

func rcBuild(run release.Run, checksURL string) rcBuildModel {
	build := rcBuildModel{Status: "No verified build", Class: "unknown", Link: checksURL, LinkLabel: "View tag checks"}
	if run.ID == "" {
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
	case run.Outcome == "success":
		build.Status, build.Class = "Build succeeded", "yes"
		if run.URL != "" {
			build.LinkLabel = "View build output"
		}
	default:
		build.Status = "Build status unknown"
	}
	return build
}
