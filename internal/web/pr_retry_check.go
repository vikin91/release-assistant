package web

import (
	"fmt"
	"slices"
	"strings"

	"release-assistant/internal/release"
)

type prRetryStatus struct {
	Page     pageModel
	PR       release.PR
	Konflux  release.Plan
	CI       release.ManualRetryReport
	Anchor   string
	Commands string
}

func prRetryAnchor(target release.Target) string {
	return fmt.Sprintf("pr-retry-%s-%d", strings.ReplaceAll(target.Repo, "/", ":"), target.PR)
}

// PRRetryStatus exposes only commands for an included PR's current verified head.
func (p pageModel) PRRetryStatus(pr release.PR, branch string) *prRetryStatus {
	if p.Snapshot.Choices.PRs[pr.Target] != release.Included || pr.State != "open" ||
		pr.Merged || pr.Base != branch || pr.SHA == "" || !pr.Evidence.Fresh() {
		return nil
	}
	status := prRetryStatus{Page: p, PR: pr, Anchor: prRetryAnchor(pr.Target)}
	if check := p.Snapshot.PRRetryChecks[pr.Target]; check.SHA == pr.SHA {
		status.Konflux, status.CI = check.Konflux, check.CI
	}
	for _, plan := range p.Plans {
		if plan.Plan.Target == pr.Target && plan.Plan.SHA == pr.SHA &&
			(status.Konflux.Evidence.At.IsZero() || plan.Plan.Evidence.At.After(status.Konflux.Evidence.At)) {
			status.Konflux = plan.Plan
		}
	}
	for _, report := range p.ManualRetries {
		if report.Target == pr.Target && report.SHA == pr.SHA && !report.Pending &&
			(status.CI.At.IsZero() || report.At.After(status.CI.At)) {
			status.CI = report
		}
	}
	if pr.CheckResultsEvidence.Fresh() && pr.CheckResultsEvidence.At.After(status.CI.At) &&
		!slices.ContainsFunc(pr.Checks, func(check release.Check) bool { return check.Result() == release.No }) {
		status.CI = release.ManualRetryReport{Target: pr.Target, SHA: pr.SHA, At: pr.CheckResultsEvidence.At}
	}

	var commands []string
	for _, proposal := range status.Konflux.Proposals {
		if proposal.Target == pr.Target && proposal.SHA == pr.SHA {
			commands = append(commands, proposal.Payload)
		}
	}
	for _, job := range status.CI.Jobs {
		commands = append(commands, job.RerunCommand(pr.Target.Repo))
	}
	if prow := status.CI.ProwComment(); prow != "" {
		commands = append(commands, prow)
	}
	status.Commands = strings.Join(commands, "\n")
	return &status
}
