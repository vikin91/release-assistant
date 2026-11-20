package github

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"release-assistant/internal/release"
)

const githubActionsAppID = 15368

type retryWorkflowInput struct {
	consumerRunID int64
	build         retryRun
	pending       []retryWorkflowProducer
}

// retryWorkflowInputs identifies the publishers whose outputs a required check still needs.
func (c *Client) retryWorkflowInputs(ctx context.Context, t release.Target, pr retryPR, scope retryScope, runs []retryRun, checks []commitCheckRun) (map[string]retryWorkflowInput, error) {
	inputs := make(map[string]retryWorkflowInput)
	for _, dependency := range retryDependencies[strings.ToLower(t.Repo)] {
		if !slices.ContainsFunc(dependency.RequiredChecks, func(name string) bool {
			return scope.matches(name, githubActionsAppID)
		}) {
			continue
		}
		downstream := latestRetryWorkflow(t, pr, dependency.Workflow, runs)
		if downstream.ID == 0 {
			inputs[dependency.Job] = retryWorkflowInput{pending: dependency.Producers}
			continue
		}
		observed, err := c.retryWorkflowChecks(ctx, t, pr, downstream, checks)
		if err != nil {
			return nil, err
		}
		waitMatches := func(name string) bool { return name == dependency.Job }
		if retryCheckCompleted(observed, waitMatches, "success", "skipped") {
			inputs[dependency.Job] = retryWorkflowInput{consumerRunID: downstream.ID}
			continue
		}
		build := latestRetryWorkflow(t, pr, dependency.Needs, runs)
		observed, err = c.retryWorkflowChecks(ctx, t, pr, build, checks)
		if err != nil {
			return nil, err
		}
		input := retryWorkflowInput{consumerRunID: downstream.ID, build: build}
		for _, producer := range dependency.Producers {
			if !retryCheckCompleted(observed, producer.matches, "success") {
				input.pending = append(input.pending, producer)
			}
		}
		inputs[dependency.Job] = input
	}
	return inputs, nil
}

func (c *Client) retryWorkflowChecks(ctx context.Context, t release.Target, pr retryPR, run retryRun, checks []commitCheckRun) ([]commitCheckRun, error) {
	if run.ID == 0 || run.SuiteID == 0 {
		return nil, nil
	}
	if run.Attempt > 1 {
		latest, reason, err := c.latestRetryChecks(ctx, t, pr, run, checks)
		if reason != "" {
			return nil, fmt.Errorf("cannot verify workflow dependencies for run %d: %s", run.ID, reason)
		}
		return latest, err
	}
	var observed []commitCheckRun
	for _, check := range checks {
		if check.Suite.ID == run.SuiteID && check.HeadSHA == pr.Head.SHA && check.App.Slug == "github-actions" {
			observed = append(observed, check)
		}
	}
	return observed, nil
}

func retryCheckCompleted(checks []commitCheckRun, matches func(string) bool, conclusions ...string) bool {
	passed := false
	for _, check := range checks {
		if !matches(check.Name) {
			continue
		}
		if check.Status != "completed" || !slices.Contains(conclusions, check.Conclusion) || passed {
			return false
		}
		passed = true
	}
	return passed
}

func (producer retryWorkflowProducer) matches(name string) bool {
	if prefix, wildcard := strings.CutSuffix(producer.Check, "*)"); wildcard {
		return strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ")")
	}
	return name == producer.Check
}

func (producer retryWorkflowProducer) neededFor(failed, file string, load func(string) retryWorkflow) bool {
	if producer.matches(failed) {
		return true
	}
	job, _, _ := strings.Cut(producer.Check, " (")
	return workflowPrerequisite(file, job, failed, load)
}

func retryWorkflowDependencyProblem(t release.Target, scope retryScope, run retryRun, job string, load func(string) retryWorkflow) string {
	workflow := run.Path
	for _, dependency := range retryDependencies[strings.ToLower(t.Repo)] {
		if dependency.Workflow != workflow || (job != dependency.Job && !workflowPrerequisite(workflow, job, dependency.Job, load)) {
			continue
		}
		if !scope.workflowInputsVerified {
			return fmt.Sprintf("cannot verify the %s image publishers for this commit", dependency.Name)
		}
		input, waiting := scope.workflowInputs[dependency.Job]
		if !waiting {
			continue
		}
		if input.consumerRunID != 0 && input.consumerRunID != run.ID {
			return "cannot verify image publishers for this workflow run"
		}
		if len(input.pending) == 0 {
			continue
		}
		if input.build.ID == 0 {
			return fmt.Sprintf("cannot verify the %s run for this commit", dependency.Name)
		}
		jobs := make([]string, 0, len(input.pending))
		for _, producer := range input.pending {
			jobs = append(jobs, fmt.Sprintf("%s for %s", producer.Check, producer.Images))
		}
		return fmt.Sprintf("waiting for %s image publishers %s at https://github.com/%s/actions/runs/%d",
			dependency.Name, strings.Join(jobs, ", "), t.Repo, input.build.ID)
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
