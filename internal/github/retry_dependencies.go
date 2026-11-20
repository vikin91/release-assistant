package github

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"release-assistant/internal/release"
)

const githubActionsAppID = 15368

// requiredRetryWorkflows includes producers while a check still needs their output.
func (c *Client) requiredRetryWorkflows(ctx context.Context, t release.Target, pr retryPR, scope retryScope, runs []retryRun, checks []commitCheckRun) (map[string]bool, error) {
	required := make(map[string]bool)
	dependencies := retryDependencies[strings.ToLower(t.Repo)]
	for _, dependency := range dependencies {
		if !slices.ContainsFunc(dependency.RequiredChecks, func(name string) bool {
			return scope.matches(name, githubActionsAppID)
		}) {
			continue
		}
		downstream := latestRetryWorkflow(t, pr, dependency.Workflow, runs)
		if downstream.ID == 0 {
			continue
		}
		observed := checks
		if downstream.Attempt > 1 {
			latest, reason, err := c.latestRetryChecks(ctx, t, pr, downstream, checks)
			if err != nil {
				return nil, err
			}
			if reason != "" {
				return nil, fmt.Errorf("cannot verify workflow dependencies for run %d: %s", downstream.ID, reason)
			}
			observed = latest
		}
		if !slices.ContainsFunc(observed, func(check commitCheckRun) bool {
			return check.Suite.ID == downstream.SuiteID && check.HeadSHA == pr.Head.SHA &&
				check.App.Slug == "github-actions" && check.Name == dependency.Job &&
				check.Status == "completed" && check.Conclusion == "success"
		}) {
			required[dependency.Needs] = true
		}
	}
	return required, nil
}

func retryWorkflowDependencyProblem(t release.Target, pr retryPR, workflow, job string, runs []retryRun, load func(string) retryWorkflow) string {
	for _, dependency := range retryDependencies[strings.ToLower(t.Repo)] {
		if dependency.Workflow != workflow || (job != dependency.Job && !workflowPrerequisite(workflow, job, dependency.Job, load)) {
			continue
		}
		latest := latestRetryWorkflow(t, pr, dependency.Needs, runs)
		if latest.ID == 0 {
			return fmt.Sprintf("cannot verify the %s run for this commit", dependency.Name)
		}
		if latest.Status != "completed" || latest.Conclusion != "success" {
			return fmt.Sprintf("inspect the %s run for this commit (%s) at https://github.com/%s/actions/runs/%d",
				dependency.Name, cmp.Or(latest.Conclusion, latest.Status, "unknown"), t.Repo, latest.ID)
		}
	}
	return ""
}

func latestRetryWorkflow(t release.Target, pr retryPR, file string, runs []retryRun) retryRun {
	var latest retryRun
	for _, run := range runs {
		if run.Path != file || run.targetProblem(t, pr) != "" {
			continue
		}
		if latest.ID != 0 && (latest.WorkflowID != run.WorkflowID || latest.Number == run.Number) {
			return retryRun{}
		}
		if run.Number > latest.Number {
			latest = run
		}
	}
	return latest
}
