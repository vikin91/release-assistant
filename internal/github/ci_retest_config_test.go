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

func TestFailedPresubmitOwnership(t *testing.T) {
	const context = "ci/prow/gke-qa-e2e-tests"
	for name, tc := range map[string]struct {
		repo, context string
		reporter      int64
		wantAllowed   bool
	}{
		"verified presubmit":                 {"StackRox/StackRox", context, openshiftCIReporterID, true},
		"same context in another repository": {"stackrox/fact", context, openshiftCIReporterID, false},
		"unlisted presubmit":                 {"stackrox/stackrox", "ci/prow/new", openshiftCIReporterID, false},
		"same context from another reporter": {"stackrox/stackrox", context, 123, false},
	} {
		t.Run(name, func(t *testing.T) {
			report := ManualRetryReport{Target: release.Target{Repo: tc.repo, PR: 7}}
			status := commitStatus{State: "failure", Context: tc.context, URL: "https://prow.ci.openshift.org/view/gs/test-platform-results-public/pr-logs/pull/" + strings.ToLower(strings.ReplaceAll(tc.repo, "/", "_")) + "/7/job/123"}
			status.Creator.ID = tc.reporter
			failed := failedPresubmits(&report, []commitStatus{status}, retryScope{required: []requiredCheck{{context: tc.context}}})
			if tc.wantAllowed {
				if failed[tc.context] != (failedPresubmit{Name: "job", URL: status.URL}) || len(report.OtherChecks) != 0 {
					t.Fatalf("failed = %v, other checks = %v", failed, report.OtherChecks)
				}
			} else if len(failed) != 0 || len(report.OtherChecks) != 1 || !strings.Contains(report.OtherChecks[0], tc.context+" · Unknown check") {
				t.Fatalf("failed = %v, other checks = %v", failed, report.OtherChecks)
			}
		})
	}
}
