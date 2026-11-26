package web

import (
	"strings"
	"testing"

	"release-assistant/internal/release"
)

func TestRCVulnerabilityReviewShowsPartialEvidenceAndPreviousCandidate(t *testing.T) {
	for name, test := range map[string]struct {
		status                 string
		conforma, previousOpen bool
		label                  string
	}{
		"scan running":                     {"Pending", true, true, "Scan pending"},
		"finished without scan report":     {"Unavailable", true, true, "Scan report unavailable"},
		"finished without Conforma report": {"Available", false, true, "No Conforma report found"},
		"all reports available":            {"Available", true, false, "Reports available"},
	} {
		t.Run(name, func(t *testing.T) {
			snapshot := workflowStatusSnapshot()
			rc := snapshot.Candidate()
			previous := release.Target{Repo: rc.Target.Repo, Tag: "5.0.0-rc.0"}
			snapshot.RCVulnerabilities = []release.RCVulnerabilities{
				{Target: rc.Target, SHA: rc.SHA, Images: []release.ImageVulnerabilities{
					{Image: "release-main", Status: test.status},
					{Image: "release-scanner-v4", Status: "Available", ConformaReported: test.conforma, Findings: []release.Vulnerability{
						{ID: "CVE-test-hold", Severity: "Important", CVSS: new(7.8), Fixed: "2"},
						{ID: "CVE-test-other", Severity: "Important", CVSS: new(8.5)},
					}},
				}},
				{Target: previous, SHA: "old", Images: []release.ImageVulnerabilities{{Image: "release-main", Status: "Available", ConformaReported: true}}},
			}
			body := newBrowser(t, &fakeApplication{snapshot: snapshot}).getBody(t)
			section := between(t, body, `id="rc-vulnerabilities"`, "</section>")
			for _, want := range []string{"above 7.0", "Suggested hold", test.label, "Findings behind the suggested hold", "Other findings", "No known fix"} {
				if !strings.Contains(section, want) {
					t.Errorf("review missing %q", want)
				}
			}
			if strings.Index(section, "CVE-test-hold") > strings.Index(section, "CVE-test-other") {
				t.Fatal("hold findings should be listed first")
			}
			previousKey := `data-fold-key="rc-vuln:` + previous.String() + `:old:1:`
			if got := strings.Contains(section, previousKey+`true" open`); got != test.previousOpen {
				t.Errorf("previous expanded = %v, want %v", got, test.previousOpen)
			}
			if !strings.Contains(body, `#rc-vulnerabilities" data-open-section`) {
				t.Fatal("missing sidebar shortcut")
			}
		})
	}
}

func TestVulnerabilityReviewDoesNotRenderWithoutCandidate(t *testing.T) {
	snapshot := workflowStatusSnapshot()
	body := newBrowser(t, &fakeApplication{snapshot: snapshot}).getBody(t)
	if strings.Contains(body, `id="rc-vulnerabilities"`) || strings.Contains(body, `#rc-vulnerabilities"`) {
		t.Fatal("vulnerability review rendered without a candidate observation")
	}
}
