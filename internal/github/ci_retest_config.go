package github

import (
	"slices"
	"strings"

	"release-assistant/internal/release"
)

const (
	stackroxBuildWorkflow       = ".github/workflows/build.yaml"
	stackroxE2EDispatchWorkflow = ".github/workflows/e2e-dispatch.yaml"
	waitForActionsImagesJob     = "wait-for-images"
)

// Hardcoded ci-retest ownership configuration. Keys identify the reporting
// provider and caller workflow in one repository. Prow uses its status context
// as the job. A present key with a nil job list covers the entire source.
type (
	retrySource struct{ repo, provider, workflow string }
	retryList   map[retrySource][]string
)

var (
	// retryAllowList supports required jobs and their prerequisites in these CI workflows.
	retryAllowList = retryList{
		{"stackrox/stackrox", "github-actions", stackroxBuildWorkflow}:         nil,
		{"stackrox/collector", "github-actions", ".github/workflows/main.yml"}: nil,
		{"stackrox/fact", "github-actions", ".github/workflows/ci.yml"}:        nil,
		{"stackrox/stackrox", "github-actions", ".github/workflows/style.yaml"}: {
			"check-generated-files",
			"golangci-lint",
			"style-check",
		},
		{"stackrox/stackrox", "github-actions", ".github/workflows/unit-tests.yaml"}: {
			`go (GOTAGS="")`,
			"local-roxctl-tests",
		},
		{"stackrox/stackrox", "github-actions", stackroxE2EDispatchWorkflow}: {
			"e2e-nongroovy-tests-gke / run / gke-nongroovy-e2e-tests",
			waitForActionsImagesJob,
		},
	}
	// retryOptionalList also permits these jobs when the CLI includes optional checks.
	// A branch rule or prerequisite can bring any of them into the default report.
	retryOptionalList = retryList{
		{"stackrox/collector", "github-actions", ".github/workflows/main.yml"}: {
			"integration-tests / amd64-integration-tests (rhel) / Testing rhel",
			"integration-tests / arm64-integration-tests (rhel-arm64) / Testing rhel-arm64",
		},
		{"stackrox/fact", "github-actions", ".github/workflows/ci.yml"}: {
			"integration-tests / integration-tests (rhcos)",
			"unit-tests / unit-tests (rhcos)",
		},
		{"stackrox/stackrox", "github-actions", stackroxE2EDispatchWorkflow}: {
			"e2e-gke-upgrade-tests / gke-upgrade-tests-postgres",
		},
		{"stackrox/stackrox", "github-actions", ".github/workflows/scan-image-vulnerabilities.yaml"}: nil,
		// openshift/release at 5fa6a272, stackrox-stackrox-release-5.0-presubmits.yaml.
		{"stackrox/stackrox", "Prow", ""}: {
			"ci/prow/aks-qa-e2e-tests",
			"ci/prow/aro-qa-e2e-tests",
			"ci/prow/eks-qa-e2e-tests",
			"ci/prow/gke-latest-nongroovy-e2e-tests",
			"ci/prow/gke-latest-operator-e2e-tests",
			"ci/prow/gke-latest-qa-e2e-tests",
			"ci/prow/gke-latest-ui-e2e-tests",
			"ci/prow/gke-nongroovy-compatibility-tests",
			"ci/prow/gke-nongroovy-e2e-tests",
			"ci/prow/gke-oldest-nongroovy-e2e-tests",
			"ci/prow/gke-oldest-operator-e2e-tests",
			"ci/prow/gke-oldest-qa-e2e-tests",
			"ci/prow/gke-oldest-ui-e2e-tests",
			"ci/prow/gke-operator-e2e-tests",
			"ci/prow/gke-qa-e2e-tests",
			"ci/prow/gke-race-condition-qa-e2e-tests",
			"ci/prow/gke-scale-tests",
			"ci/prow/gke-scanner-v4-install-tests",
			"ci/prow/gke-ui-e2e-tests",
			"ci/prow/gke-version-compatibility-tests",
			"ci/prow/ocp-4-12-compliance-e2e-tests",
			"ci/prow/ocp-4-12-nongroovy-e2e-tests",
			"ci/prow/ocp-4-12-operator-e2e-tests",
			"ci/prow/ocp-4-12-qa-e2e-tests",
			"ci/prow/ocp-4-12-scanner-v4-install-tests",
			"ci/prow/ocp-4-12-ui-e2e-tests",
			"ci/prow/ocp-4-14-vm-scanning-e2e-tests",
			"ci/prow/ocp-4-22-compliance-e2e-tests",
			"ci/prow/ocp-4-22-fips-qa-e2e-tests",
			"ci/prow/ocp-4-22-nongroovy-e2e-tests",
			"ci/prow/ocp-4-22-operator-e2e-tests",
			"ci/prow/ocp-4-22-qa-e2e-tests",
			"ci/prow/ocp-4-22-scanner-v4-install-tests",
			"ci/prow/ocp-4-22-ui-e2e-tests",
			"ci/prow/ocp-4-22-vm-scanning-e2e-tests",
			"ci/prow/ocp-dev-preview-compliance-e2e-tests",
			"ci/prow/ocp-dev-preview-fips-qa-e2e-tests",
			"ci/prow/ocp-dev-preview-nongroovy-e2e-tests",
			"ci/prow/ocp-dev-preview-operator-e2e-tests",
			"ci/prow/ocp-dev-preview-qa-e2e-tests",
			"ci/prow/ocp-dev-preview-scanner-v4-install-tests",
			"ci/prow/ocp-dev-preview-ui-e2e-tests",
			"ci/prow/ocp-dev-preview-vm-scanning-e2e-tests",
			"ci/prow/ocp-next-candidate-compliance-e2e-tests",
			"ci/prow/ocp-next-candidate-fips-qa-e2e-tests",
			"ci/prow/ocp-next-candidate-nongroovy-e2e-tests",
			"ci/prow/ocp-next-candidate-operator-e2e-tests",
			"ci/prow/ocp-next-candidate-qa-e2e-tests",
			"ci/prow/ocp-next-candidate-scanner-v4-install-tests",
			"ci/prow/ocp-next-candidate-ui-e2e-tests",
			"ci/prow/ocp-next-candidate-vm-scanning-e2e-tests",
			"ci/prow/osd-aws-qa-e2e-tests",
			"ci/prow/osd-gcp-qa-e2e-tests",
			"ci/prow/powervs-qa-e2e-tests",
			"ci/prow/rosa-fips-qa-e2e-tests",
			"ci/prow/rosa-hcp-qa-e2e-tests",
			"ci/prow/rosa-qa-e2e-tests",
		},
	}
	retryDenyList = retryList{
		{"stackrox/collector", "github-actions", ".github/workflows/konflux.yml"}:  nil,
		{"stackrox/fact", "github-actions", ".github/workflows/konflux-tests.yml"}: nil,
		{"stackrox/collector", "red-hat-konflux", ""}:                              nil,
		{"stackrox/fact", "red-hat-konflux", ""}:                                   nil,
		{"stackrox/scanner", "red-hat-konflux", ""}:                                nil,
		{"stackrox/stackrox", "red-hat-konflux", ""}:                               nil,
	}
)

func (l retryList) matches(source retrySource, job string) bool {
	jobs, ok := l[source]
	return ok && (jobs == nil || slices.Contains(jobs, job))
}

// classifyRetry warns about unlisted checks and leaves deny-listed checks to Konflux.
func classifyRetry(report *release.ManualRetryReport, provider, workflow, job string) bool {
	source := retrySource{strings.ToLower(report.Target.Repo), provider, workflow}
	switch {
	case retryDenyList.matches(source, job):
		return false
	case retryAllowList.matches(source, job), retryOptionalList.matches(source, job):
		return true
	}
	label := report.Target.Repo + " / " + provider
	if workflow != "" {
		label += " / " + workflow
	}
	report.OtherChecks = append(report.OtherChecks, label+" / "+job+" · Unknown check — patch ci-retest to classify it")
	return false
}
