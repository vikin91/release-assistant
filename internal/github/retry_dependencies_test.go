package github

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"release-assistant/internal/release"
)

func TestRetryDependencyRules(t *testing.T) {
	for repo, dependencies := range retryDependencies {
		for _, dependency := range dependencies {
			if !release.ValidRepo(repo) || dependency.Workflow == "" || dependency.Job == "" ||
				dependency.Needs == "" || dependency.Name == "" || len(dependency.RequiredChecks) == 0 || len(dependency.Producers) == 0 {
				t.Errorf("incomplete retry dependency for %s: %+v", repo, dependency)
			}
		}
	}
}

func TestAuditedRetryTreesContainTheirPrerequisites(t *testing.T) {
	for _, repo := range []string{"stackrox/stackrox", "stackrox/collector", "stackrox/fact", "stackrox/scanner"} {
		workflows := retryWorkflowTrees[repo]
		if len(workflows) == 0 {
			t.Errorf("missing tree for %s", repo)
		}
		for workflow, trees := range workflows {
			for _, tree := range trees {
				for _, root := range tree.Roots {
					if _, ok := tree.Jobs[root]; !ok {
						t.Errorf("%s %s is missing root %s", repo, workflow, root)
					}
				}
				for job, needs := range tree.Jobs {
					for _, ancestor := range needs {
						if _, ok := tree.Jobs[ancestor]; !ok {
							t.Errorf("%s %s is missing prerequisite %s for %s", repo, workflow, ancestor, job)
						}
					}
					if call := tree.Calls[job]; call != "" && len(workflows[strings.TrimPrefix(call, "./")]) == 0 {
						t.Errorf("%s %s is missing called workflow %s for %s", repo, workflow, call, job)
					}
				}
			}
		}
	}
}

func TestRetryWorkflowInputs(t *testing.T) {
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
		"should exclude Build when the image wait is skipped":           {waitForActionsImagesJob, "skipped", 23, false},
		"should ignore an image wait from another suite":                {waitForActionsImagesJob, "success", 24, true},
		"should exclude Build when its output has no required consumer": {"style-check", "failure", 23, false},
	} {
		t.Run(name, func(t *testing.T) {
			wait := commitCheckRun{Name: waitForActionsImagesJob, HeadSHA: "head", Status: "completed", Conclusion: tc.waitResult}
			wait.App.Slug, wait.Suite.ID = "github-actions", tc.suiteID
			scope := retryScope{required: []requiredCheck{{tc.required, githubActionsAppID}}}
			got, err := (&Client{}).retryWorkflowInputs(t.Context(), target, pr, scope, []retryRun{dispatch}, []commitCheckRun{wait})
			if err != nil || (len(got[waitForActionsImagesJob].pending) > 0) != tc.want {
				t.Fatalf("required workflows = %v, error = %v, want Build required = %v", got, err, tc.want)
			}
		})
	}
}

func TestImagePublishersUseCurrentJobResults(t *testing.T) {
	target := release.Target{Repo: "stackrox/stackrox", PR: 7}
	pr := retryPR{Number: 7, Head: retryBranch{SHA: "head"}, Base: retryBranch{Ref: "master"}}
	dispatch := retryRun{
		workflowRun: workflowRun{ID: 21, WorkflowID: 22, SuiteID: 23, SHA: "head", Event: "pull_request"},
		Path:        stackroxE2EDispatchWorkflow, Number: 1, Attempt: 1, PRs: []retryPR{pr},
	}
	build := dispatch
	build.ID, build.WorkflowID, build.SuiteID, build.Path = 31, 32, 33, stackroxBuildWorkflow
	build.Conclusion = "failure"
	older := build
	older.ID, older.SuiteID, build.Number = 30, 34, 2

	check := func(name, conclusion string, suite int64) commitCheckRun {
		check := commitCheckRun{Name: name, HeadSHA: "head", Status: "completed", Conclusion: conclusion}
		check.Suite.ID, check.App.Slug = suite, "github-actions"
		return check
	}
	checks := []commitCheckRun{
		check(waitForActionsImagesJob, "failure", 23),
		check("build-and-push-db", "success", 33),
		check("build-operator-bundle", "success", 33),
		check("push-main-manifests (RHACS_BRANDING)", "success", 33),
		check("push-operator-manifests (RHACS_BRANDING, amd64,arm64)", "success", 33),
		check("push-scanner-manifests (default, quay.io/rhacs-eng)", "success", 33),
		check("push-matching-collector-scanner (STACKROX_BRANDING)", "success", 33),
		check("push-matching-collector-scanner (RHACS_BRANDING)", "success", 34),
		check("unrelated", "failure", 33),
	}
	scope := retryScope{required: []requiredCheck{{waitForActionsImagesJob, githubActionsAppID}}}
	inputs, err := (&Client{}).retryWorkflowInputs(t.Context(), target, pr, scope, []retryRun{dispatch, older, build}, checks)
	if input := inputs[waitForActionsImagesJob]; err != nil || input.build.ID != 31 || len(input.pending) != 1 ||
		input.pending[0].Check != "push-matching-collector-scanner (RHACS_BRANDING)" {
		t.Fatalf("expected the matching Collector and FACT publication to be missing from the new run, got %+v (%v)", input, err)
	}

	checks = append(checks, check("push-matching-collector-scanner (RHACS_BRANDING)", "success", 33))
	inputs, err = (&Client{}).retryWorkflowInputs(t.Context(), target, pr, scope, []retryRun{dispatch, older, build}, checks)
	if input := inputs[waitForActionsImagesJob]; err != nil || input.build.ID != 31 || len(input.pending) != 0 {
		t.Fatalf("expected all publishers to pass despite the unrelated failure, got %+v (%v)", input, err)
	}
}

func TestImagePublisherPrerequisites(t *testing.T) {
	const workflow = ".github/workflows/build.yaml"
	const publisherJob = "push-scanner-manifests"
	const publisherCheck = publisherJob + " (default, quay.io/rhacs-eng)"
	const data = `jobs:
  push-scanner-manifests:
    needs: build-and-push-scanner
    strategy:
      matrix: ${{ fromJSON(needs.setup.outputs.matrix) }}
  build-and-push-scanner:
    needs: compile
  compile: {}
  unrelated: {}
`
	var w retryWorkflow
	if err := yaml.Unmarshal([]byte(data), &w); err != nil {
		t.Fatal(err)
	}
	load := func(string) retryWorkflow { return w }
	publisher := retryWorkflowProducer{Check: publisherCheck}
	for name, tc := range map[string]struct {
		check string
		want  bool
	}{
		"should include the image publisher":             {publisherCheck, true},
		"should include the build":                       {"build-and-push-scanner", true},
		"should include the first failed prerequisite":   {"compile", true},
		"should exclude unrelated failures":              {"unrelated", false},
		"should exclude publication to another registry": {publisherJob + " (default, quay.io/stackrox-io)", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := publisher.neededFor(tc.check, workflow, load); got != tc.want {
				t.Errorf("neededFor(%q) = %v, want %v", tc.check, got, tc.want)
			}
		})
	}
}

func TestApplyAuditedRetryTree(t *testing.T) {
	const data = "jobs: {compile: {}, test: {}}"
	trees := []retryWorkflowTree{{
		Fingerprints: []string{fmt.Sprintf("%x", sha256.Sum256([]byte(data)))},
		Jobs:         map[string][]string{"compile": nil, "test": {"compile"}},
	}}
	for name, tc := range map[string]struct {
		yaml string
		want []string
	}{
		"should apply the tree to the audited workflow":       {data, []string{"compile"}},
		"should read needs from the current changed workflow": {"jobs: {compile: {}, lint: {}, test: {needs: lint}}", []string{"lint"}},
	} {
		t.Run(name, func(t *testing.T) {
			var w retryWorkflow
			if err := yaml.Unmarshal([]byte(tc.yaml), &w); err != nil {
				t.Fatal(err)
			}
			w.applyAuditedTree(trees, []byte(tc.yaml))
			if got := w.directNeeds("test"); !slices.Equal(got, tc.want) {
				t.Errorf("test prerequisites = %v, want %v", got, tc.want)
			}
		})
	}
}
