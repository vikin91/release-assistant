package github

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"go.yaml.in/yaml/v3"

	"release-assistant/internal/release"
)

func TestWorkflowPrerequisites(t *testing.T) {
	const caller = ".github/workflows/ci.yml"
	files := make(map[string]retryWorkflow)
	for name, data := range map[string]string{
		caller: `jobs:
  compile:
    name: Compile
    strategy:
      matrix: {arch: [amd64, arm64]}
  package:
    needs: CompileJob
  CompileJob:
    needs: [compile, experimental]
  experimental:
    continue-on-error: true
  verify:
    needs: package
    uses: ./.github/workflows/verify.yml
    strategy:
      matrix: {os: [linux, mac]}
  unrelated:
    needs: compile
`,
		".github/workflows/verify.yml": `jobs:
  setup: {}
  checks:
    name: Check ${{ matrix.arch }}
    needs: setup
    strategy:
      matrix: {arch: [amd64, arm64]}
`,
	} {
		var workflow retryWorkflow
		if err := yaml.Unmarshal([]byte(data), &workflow); err != nil {
			t.Fatal(err)
		}
		files[name] = workflow
	}
	const required = "verify (linux) / Check amd64"
	for failed, want := range map[string]bool{
		"Compile (arm64)":              true,
		"package":                      true,
		"verify (linux) / setup":       true,
		"verify (mac) / setup":         false,
		"verify (linux) / Check arm64": false,
		"experimental":                 false,
		"unrelated":                    false,
	} {
		t.Run(failed, func(t *testing.T) {
			got := workflowPrerequisite(caller, required, failed, func(file string) retryWorkflow { return files[file] })
			if got != want {
				t.Fatalf("prerequisite = %v, want %v", got, want)
			}
		})
	}
	if workflowPrerequisite(caller, "verify (windows) / Check amd64", "Compile (arm64)", func(file string) retryWorkflow { return files[file] }) {
		t.Fatal("nonexistent caller matrix leg established a prerequisite")
	}
	if workflowPrerequisite(caller, "verify (linux) / missing", "Compile (arm64)", func(file string) retryWorkflow { return files[file] }) {
		t.Fatal("missing nested check established a prerequisite")
	}
}

func TestRequiredRetryScopeMatchesProvider(t *testing.T) {
	scope := retryScope{required: []requiredCheck{{"same name", 42}, {"any reporter", 0}}}
	if !scope.matches("same name", 42) || !scope.matches("any reporter", 123) || scope.matches("same name", 123) || scope.matches("same name", 0) {
		t.Fatal("required status check did not respect its app binding")
	}
}

func TestOptionalWorkflowDependents(t *testing.T) {
	const workflow = ".github/workflows/main.yml"
	var graph retryWorkflow
	if err := yaml.Unmarshal([]byte(`jobs:
  test: {}
  notify:
    if: always() && contains(join(needs.*.result, ','), 'failure')
    needs: test
  build: {}
  publish:
    if: always() && !contains(needs.*.result, 'failure')
    needs: build
`), &graph); err != nil {
		t.Fatal(err)
	}
	load := func(string) retryWorkflow { return graph }
	if workflowCheckHasDependents(workflow, "test", load) {
		t.Fatal("the failure notification depends on completion only")
	}
	if !workflowCheckHasDependents(workflow, "build", load) {
		t.Fatal("publishing depends on a successful build")
	}
}

func TestRetryPrerequisitesInOrder(t *testing.T) {
	const workflow = `jobs:
  job1: {}
  job2:
    needs: job1
  job3:
    needs: job2
  job4:
    needs: job3
  unrelated: {}
`
	const caller = ".github/workflows/main.yml"
	target := release.Target{Repo: "stackrox/collector", PR: 7}
	pr := retryPR{Number: 7, MergeSHA: "merge", Head: retryBranch{SHA: "head"}, Base: retryBranch{SHA: "base"}}
	run := retryRun{workflowRun: workflowRun{SuiteID: 1}, Path: caller, PRs: []retryPR{pr}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/stackrox/collector/contents/"+caller || r.URL.Query().Get("ref") != "merge" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"encoding":"base64","content":%q}`, base64.StdEncoding.EncodeToString([]byte(workflow)))
	}))
	defer server.Close()
	client := New(server.URL, "", server.Client())
	for name, tc := range map[string]struct {
		results []string
		want    []string
	}{
		"should offer job1 when nothing passed":                 {[]string{"failure", "failure", "failure", "failure"}, []string{"job1"}},
		"should offer job2 after job1 passes":                   {[]string{"success", "failure", "failure", "failure"}, []string{"job2"}},
		"should offer job3 after job2 passes":                   {[]string{"success", "success", "failure", "failure"}, []string{"job3"}},
		"should offer job4 after job3 passes":                   {[]string{"success", "success", "success", "failure"}, []string{"job4"}},
		"should wait when a prerequisite is running":            {[]string{"", "failure", "failure", "failure"}, nil},
		"should wait when a prerequisite is missing":            {[]string{"absent", "failure", "failure", "failure"}, nil},
		"should find a blocker behind skipped jobs":             {[]string{"failure", "skipped", "skipped", "failure"}, []string{"job1"}},
		"should omit unrelated jobs in default mode":            {[]string{"success", "success", "success", "success", "failure"}, nil},
		"should omit ancestors after the required check passes": {[]string{"failure", "failure", "failure", "success"}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			names := []string{"job1", "job2", "job3", "job4", "unrelated"}
			var checks, failed []commitCheckRun
			for i, outcome := range tc.results {
				if outcome == "absent" {
					continue
				}
				check := commitCheckRun{Name: names[i], HeadSHA: "head", Status: "completed", Conclusion: outcome}
				check.App.Slug, check.App.ID, check.Suite.ID = "github-actions", 15368, 1
				if outcome == "" {
					check.Status = "in_progress"
				}
				if outcome == "failure" {
					failed = append(failed, check)
				}
				checks = append(checks, check)
			}
			scope := retryScope{required: []requiredCheck{{"job4", 15368}}}
			selected, _, err := client.scopedRunChecks(t.Context(), target, pr, run, scope, failed, checks)
			var got []string
			for _, check := range selected {
				got = append(got, check.Name)
			}
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("offers = %v, error = %v, want %v", got, err, tc.want)
			}
		})
	}
}
