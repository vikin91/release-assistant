package konflux

import (
	"strings"
	"testing"
)

func TestConformaReportRequiresTheVerifiedImageDigest(t *testing.T) {
	const image = "quay.io/rhacs-eng/release-scanner-v4"
	const report = `{"components":[
  {"name":"scanner-v4-4-10","containerImage":"quay.io/rhacs-eng/release-scanner-v4@sha256:index"},
  {"name":"scanner-v4-4-10-sha256:arch-amd64","containerImage":"quay.io/rhacs-eng/release-scanner-v4@sha256:arch","violations":[{"msg":"Important CVE","metadata":{"code":"cve.cve_blockers","term":"CVE-2026-99"}},{"metadata":{"code":"provenance.materials","term":"other"}}]},
  {"name":"other-sha256:arch-arm64","containerImage":"quay.io/rhacs-eng/release-operator@sha256:arch","warnings":[{"metadata":{"code":"cve.unpatched_cve_warnings","term":"CVE-2026-88"}}]}
 ]}`
	if got := parseConformaReport(strings.NewReader(report), image, "sha256:wrong"); got.Digest != "" || len(got.Findings) > 0 {
		t.Fatalf("mismatched digest found evidence: %+v", got)
	}
	got := parseConformaReport(strings.NewReader(report), image, "sha256:index")
	if got.Digest != "sha256:index" || len(got.Findings) != 1 || got.Findings[0].Architecture != "amd64" || got.Findings[0].ID != "CVE-2026-99" {
		t.Fatalf("unexpected report: %+v", got)
	}
}
