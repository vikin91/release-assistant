package github

import (
	"testing"

	"release-assistant/internal/release"
)

func TestRetryDependencyRules(t *testing.T) {
	for repo, dependencies := range retryDependencies {
		for _, dependency := range dependencies {
			if !release.ValidRepo(repo) || dependency.Workflow == "" || dependency.Job == "" ||
				dependency.Needs == "" || dependency.Name == "" || len(dependency.RequiredChecks) == 0 {
				t.Errorf("incomplete retry dependency for %s: %+v", repo, dependency)
			}
		}
	}
}

func TestRequiredRetryWorkflows(t *testing.T) {
	target := release.Target{Repo: "stackrox/stackrox", PR: 7}
	pr := retryPR{Number: 7, Head: retryBranch{SHA: "head"}, Base: retryBranch{Ref: "master"}}
	dispatch := retryRun{
		workflowRun: workflowRun{ID: 21, WorkflowID: 22, SuiteID: 23, SHA: "head", Event: "pull_request"},
		Path:        stackroxE2EDispatchWorkflow,
		Number:      5,
		Attempt:     1,
		PRs:         []retryPR{pr},
	}
	for name, tc := range map[string]struct {
		required, waitResult string
		suiteID              int64
		want                 bool
	}{
		"should include Build when a required test still needs images":  {"e2e-gke-upgrade-tests / gke-upgrade-tests-postgres", "failure", 23, true},
		"should exclude Build after the image wait passes":              {waitForActionsImagesJob, "success", 23, false},
		"should ignore an image wait from another suite":                {waitForActionsImagesJob, "success", 24, true},
		"should exclude Build when its output has no required consumer": {"style-check", "failure", 23, false},
	} {
		t.Run(name, func(t *testing.T) {
			wait := commitCheckRun{Name: waitForActionsImagesJob, HeadSHA: "head", Status: "completed", Conclusion: tc.waitResult}
			wait.App.Slug, wait.Suite.ID = "github-actions", tc.suiteID
			scope := retryScope{required: []requiredCheck{{tc.required, githubActionsAppID}}}
			got, err := (&Client{}).requiredRetryWorkflows(t.Context(), target, pr, scope, []retryRun{dispatch}, []commitCheckRun{wait})
			if err != nil || got[stackroxBuildWorkflow] != tc.want {
				t.Fatalf("required workflows = %v, error = %v, want Build required = %v", got, err, tc.want)
			}
		})
	}
}
