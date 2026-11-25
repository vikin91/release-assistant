package web

import (
	"fmt"
	"slices"
	"strings"

	"release-assistant/internal/release"
)

type prRetryStatus struct {
	Page               pageModel
	PR                 release.PR
	Konflux            release.Plan
	CI                 release.ManualRetryReport
	Anchor             string
	Comments, Commands string
	Queueable          bool
}

func prRetryAnchor(target release.Target) string {
	return fmt.Sprintf("pr-retry-%s-%d", strings.ReplaceAll(target.Repo, "/", ":"), target.PR)
}

// PRRetryStatus shows queued commands and the latest provider observations for an included PR.
func (p pageModel) PRRetryStatus(pr release.PR, branch string) *prRetryStatus {
	if p.Snapshot.Choices.PRs[pr.Target] != release.Included || pr.State != "open" ||
		pr.Merged || pr.Base != branch || pr.SHA == "" {
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
		if report.Target == pr.Target && report.SHA == pr.SHA &&
			(status.CI.At.IsZero() || report.At.After(status.CI.At)) {
			status.CI = report
		}
	}
	if pr.CheckResultsEvidence.Fresh() && pr.CheckResultsEvidence.At.After(status.CI.At) &&
		!slices.ContainsFunc(pr.Checks, func(check release.Check) bool { return check.Result() == release.No }) {
		status.CI = release.ManualRetryReport{Target: pr.Target, SHA: pr.SHA, At: pr.CheckResultsEvidence.At}
	}

	var comments, commands []string
	for _, item := range p.Snapshot.RetestQueue {
		if item.Target != pr.Target || item.Payload == "" {
			continue
		}
		status.Queueable = true
		switch item.Kind {
		case "konflux", "prow":
			comments = append(comments, item.Payload)
		case "actions":
			commands = append(commands, item.Payload)
		}
	}
	slices.Sort(comments)
	slices.Sort(commands)
	status.Comments = strings.Join(slices.Compact(comments), "\n")
	status.Commands = strings.Join(slices.Compact(commands), "\n")
	return &status
}
