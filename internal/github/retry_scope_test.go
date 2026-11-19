package github

import (
	"testing"

	"go.yaml.in/yaml/v3"
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
