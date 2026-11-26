package web

import (
	"net/http/httptest"
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
			for _, want := range []string{"above 7.0", "Suggested hold", test.label, "Findings that meet the internal hold criteria", "Other findings", "No known fix"} {
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
			if !strings.Contains(body, `href="/?tab=rc-vulnerabilities" data-workflow-tab="rc-vulnerabilities"`) {
				t.Fatal("missing vulnerability submenu tab")
			}
		})
	}
}

func TestACSRCAndVulnerabilityViewsAreSeparate(t *testing.T) {
	snapshot := workflowStatusSnapshot()
	application := &fakeApplication{snapshot: snapshot}
	browser := newBrowser(t, application)
	get := func(tab string) string {
		t.Helper()
		w := httptest.NewRecorder()
		browser.handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), "GET", "http://127.0.0.1:8080/?tab="+tab, nil))
		if w.Code != 200 {
			t.Fatalf("GET %s returned %d", tab, w.Code)
		}
		return w.Body.String()
	}
	original := get("acs-rcs")
	if strings.Contains(original, `id="panel-rc-vulnerabilities"`) || strings.Contains(original, `id="tab-rc-vulnerabilities"`) {
		t.Fatal("vulnerability submenu rendered without an RC observation")
	}
	if body := get("rc-vulnerabilities"); !strings.Contains(body, `data-active-tab="acs-rcs"`) {
		t.Fatal("a missing vulnerability review should fall back to ACS RCs")
	}

	rc := snapshot.Candidate()
	application.snapshot.RCVulnerabilities = []release.RCVulnerabilities{{Target: rc.Target, SHA: rc.SHA}}
	acs := get("acs-rcs")
	panel := activityPanel(t, acs, "acs-rcs")
	if strings.Join(strings.Fields(panel), " ") != strings.Join(strings.Fields(activityPanel(t, original, "acs-rcs")), " ") || strings.Contains(panel, "Vulnerability review") {
		t.Fatal("the ACS RCs panel changed when vulnerability review became available")
	}
	review := get("rc-vulnerabilities")
	if !strings.Contains(review, `data-active-tab="rc-vulnerabilities"`) || !strings.Contains(review, `aria-controls="panel-rc-vulnerabilities" aria-selected="true"`) ||
		!strings.Contains(activityPanel(t, review, "rc-vulnerabilities"), `id="rc-vulnerabilities"`) {
		t.Fatal("the vulnerability URL did not select its own panel")
	}
}
