package github

import (
	"slices"
	"strings"
	"testing"

	"release-assistant/internal/release"
)

func TestRetryListsDoNotOverlap(t *testing.T) {
	for name, list := range map[string]retryList{"merge": retryAllowList, "optional": retryOptionalList} {
		for source, allowed := range list {
			denied, ok := retryDenyList[source]
			if !ok {
				continue
			}
			if allowed == nil || denied == nil || slices.ContainsFunc(allowed, func(job string) bool {
				return slices.Contains(denied, job)
			}) {
				t.Errorf("%s and deny lists overlap for %+v: allowed %v, denied %v", name, source, allowed, denied)
			}
		}
	}
}

func TestActionsRetryOwnership(t *testing.T) {
	for source, jobs := range map[retrySource][]string{
		{"stackrox/stackrox", "github-actions", ".github/workflows/style.yaml"}:        {"check-generated-files", "golangci-lint", "style-check"},
		{"stackrox/stackrox", "github-actions", ".github/workflows/unit-tests.yaml"}:   {`go (GOTAGS="")`, "local-roxctl-tests"},
		{"stackrox/stackrox", "github-actions", ".github/workflows/e2e-dispatch.yaml"}: {"e2e-nongroovy-tests-gke / run / gke-nongroovy-e2e-tests", "wait-for-images"},
		{"stackrox/scanner", "github-actions", ".github/workflows/ci.yaml"}:            {"db-integration-tests", "style-check", "unit-tests"},
	} {
		t.Run(source.repo+"/"+source.workflow, func(t *testing.T) {
			report := ManualRetryReport{Target: release.Target{Repo: source.repo, PR: 7}}
			for _, job := range jobs {
				if !classifyRetry(&report, source.provider, source.workflow, job) {
					t.Errorf("expected retry support for %s", job)
				}
			}
			if classifyRetry(&report, source.provider, source.workflow, "unclassified job") || len(report.OtherChecks) != 1 {
				t.Fatalf("unexpected check classifications: %v", report.OtherChecks)
			}
		})
	}
}

func TestKonfluxChecksAreDeniedInReleaseRepositories(t *testing.T) {
	for _, repo := range []string{"stackrox/stackrox", "stackrox/collector", "stackrox/fact", "stackrox/scanner"} {
		t.Run(repo, func(t *testing.T) {
			report := ManualRetryReport{Target: release.Target{Repo: repo, PR: 7}}
			if classifyRetry(&report, "red-hat-konflux", "", "Red Hat Konflux / checks") || len(report.OtherChecks) != 0 {
				t.Fatalf("expected Konflux to own the check, got %v", report.OtherChecks)
			}
		})
	}
}

func TestFailedPresubmitOwnership(t *testing.T) {
	const context = "ci/prow/gke-qa-e2e-tests"
	for name, tc := range map[string]struct {
		repo, context            string
		reporter, requiredApp    int64
		wantAllowed, wantUnknown bool
	}{
		"verified presubmit":                        {"StackRox/StackRox", context, openshiftCIReporterID, 0, true, false},
		"verified presubmit required from its app":  {"stackrox/stackrox", context, openshiftCIReporterID, 91280, true, false},
		"presubmit required from a different app":   {"stackrox/stackrox", context, openshiftCIReporterID, 123, false, false},
		"unverified reporter for an app-bound rule": {"stackrox/stackrox", context, 123, 91280, false, false},
		"same context in another repository":        {"stackrox/fact", context, openshiftCIReporterID, 0, false, true},
		"unlisted presubmit":                        {"stackrox/stackrox", "ci/prow/new", openshiftCIReporterID, 0, false, true},
		"same context from another reporter":        {"stackrox/stackrox", context, 123, 0, false, true},
	} {
		t.Run(name, func(t *testing.T) {
			report := ManualRetryReport{Target: release.Target{Repo: tc.repo, PR: 7}}
			status := commitStatus{State: "failure", Context: tc.context, URL: "https://prow.ci.openshift.org/view/gs/test-platform-results-public/pr-logs/pull/" + strings.ToLower(strings.ReplaceAll(tc.repo, "/", "_")) + "/7/job/123"}
			status.Creator.ID = tc.reporter
			failed := failedPresubmits(&report, []commitStatus{status}, retryScope{required: []requiredCheck{{context: tc.context, appID: tc.requiredApp}}})
			if tc.wantAllowed {
				if failed[tc.context] != (failedPresubmit{Name: "job", URL: status.URL}) || len(report.OtherChecks) != 0 {
					t.Fatalf("failed = %v, other checks = %v", failed, report.OtherChecks)
				}
				return
			}
			if len(failed) != 0 {
				t.Fatalf("failed = %v, other checks = %v", failed, report.OtherChecks)
			}
			if tc.wantUnknown {
				if len(report.OtherChecks) != 1 || !strings.Contains(report.OtherChecks[0], tc.context+" · Unknown check") {
					t.Fatalf("expected an unknown check for %s, got %v", tc.context, report.OtherChecks)
				}
			} else if len(report.OtherChecks) != 0 {
				t.Fatalf("unexpected check classifications: %v", report.OtherChecks)
			}
		})
	}
}
