package web

import (
	"net/http/httptest"
	"strings"
	"testing"

	"release-assistant/internal/release"
)

func TestRCVulnerabilitySummaryDeduplicatesFindings(t *testing.T) {
	snapshot := workflowStatusSnapshot()
	rc := snapshot.Candidate()
	const id = "CVE-2026-119373"
	const link = "https://access.redhat.com/security/cve/" + id
	finding := release.Vulnerability{
		ID: id, Severity: "Important", CVSS: new(7.8), Package: "sqlite-libs", Fixed: "2", URL: link,
	}
	variant := finding
	variant.Fixed = "3"
	snapshot.RCVulnerabilities = []release.RCVulnerabilities{
		{Target: rc.Target, SHA: rc.SHA, Images: []release.ImageVulnerabilities{
			{Findings: []release.Vulnerability{finding, {ID: "CVE-2026-999999", Severity: "Critical"}}},
			{Findings: []release.Vulnerability{finding, variant}},
		}},
	}

	body := newBrowser(t, &fakeApplication{snapshot: snapshot}).getBody(t)
	summary := between(t, body, "Copy CVE summary</button>", "</pre>")
	want := "1. <a href=\"" + link + "\">" + id + "</a> | sqlite-libs | CVSS 7.8 | fixed version 2\n" +
		"   sqlite-libs | CVSS 7.8 | fixed version 3"
	if !strings.Contains(summary, want) || strings.Count(summary, ">"+id+"</a>") != 1 ||
		strings.Contains(summary, "CVE-2026-999999") {
		t.Errorf("unexpected CVE summary: %s", summary)
	}
}

func TestRCVulnerabilityReviewShowsPartialEvidenceAndPreviousCandidate(t *testing.T) {
	const workflow = "https://github.com/stackrox/stackrox/actions/runs/42"
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
				{Target: rc.Target, SHA: rc.SHA, WorkflowURL: workflow, Images: []release.ImageVulnerabilities{
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
			for _, want := range []string{`aria-describedby="new-tab-description" href="` + workflow + `" target="_blank" rel="noreferrer">Scan workflow</a>`, "above 7.0", "Meets hold criteria", test.label, "Other findings", "1 Important above 7.0 without a known fix.", `<strong>fixable Critical CVE or a fixable Important CVE with a CVSS score above 7.0</strong>`, "This is stricter than defined in the release manual.", "Release Assistant does not block any further step of the release."} {
				if !strings.Contains(section, want) {
					t.Errorf("review missing %q", want)
				}
			}
			if !strings.Contains(section, "CVE-test-hold") || strings.Contains(section, "CVE-test-other") {
				t.Fatal("expanded table should include only critical and qualifying Important CVEs")
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

func TestVulnerabilitySidebarWarnsForDistinctHoldCVEsInNewestCandidate(t *testing.T) {
	snapshot := workflowStatusSnapshot()
	rc := snapshot.Candidate()
	repeated := release.Vulnerability{ID: "CVE-repeated", Severity: "Critical", Fixed: "2"}
	snapshot.RCVulnerabilities = []release.RCVulnerabilities{
		{Target: rc.Target, SHA: rc.SHA, Images: []release.ImageVulnerabilities{
			{Image: "release-main", Status: "Available", Findings: []release.Vulnerability{repeated}},
			{Image: "release-roxctl", Status: "Available", Findings: []release.Vulnerability{
				repeated,
				{ID: "CVE-second", Severity: "Important", CVSS: new(7.8), Fixed: "2"},
				{ID: "CVE-at-threshold", Severity: "Important", CVSS: new(7.0), Fixed: "2"},
			}},
			{Image: "release-operator", Status: "Pending"},
		}},
		{Target: release.Target{Repo: rc.Target.Repo, Tag: "5.0.0-rc.0"}, SHA: "old", Images: []release.ImageVulnerabilities{
			{Image: "release-main", Status: "Available", Findings: []release.Vulnerability{
				{ID: "CVE-previous-only", Severity: "Critical", Fixed: "2"},
			}},
		}},
	}

	for _, test := range []struct {
		name   string
		images int
		marker string
		reason string
	}{
		{"partial scans with distinct qualifying CVEs", 3, "⚠︎ 2", "2 distinct CVEs meet"},
		{"one qualifying CVE", 1, "⚠︎ 1", "1 CVE meets"},
		{"only previous RC qualifies", 0, "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot.RCVulnerabilities[0].Images = snapshot.RCVulnerabilities[0].Images[:test.images]
			body := newBrowser(t, &fakeApplication{snapshot: snapshot}).getBody(t)
			tab := between(t, body, `id="tab-rc-vulnerabilities"`, "</a>")
			if test.marker == "" {
				if strings.Contains(tab, "needs-attention") {
					t.Fatal("previous RC caused a current sidebar warning")
				}
				return
			}
			if !strings.Contains(tab, `class="tab-status needs-attention" aria-hidden="true">`+test.marker+"</span>") ||
				!strings.Contains(tab, `aria-label="Vulnerability review · Needs attention · `+test.reason) {
				t.Fatalf("sidebar warning or accessible count missing: %s", tab)
			}
		})
	}
}

func TestVulnerabilityImageShowsOnlyColorPillsUntilExpanded(t *testing.T) {
	snapshot := workflowStatusSnapshot()
	rc := snapshot.Candidate()
	repeated := release.Vulnerability{ID: "CVE-important-below", Severity: "Important", CVSS: new(6.9), Fixed: "2"}
	snapshot.RCVulnerabilities = []release.RCVulnerabilities{
		{Target: rc.Target, SHA: rc.SHA, Images: []release.ImageVulnerabilities{
			{Image: "release-main", Status: "Available", Findings: []release.Vulnerability{
				{ID: "CVE-important-fix", Severity: "Important", CVSS: new(7.1), Fixed: "2"},
				{ID: "CVE-critical-open", Severity: "Critical"},
				{ID: "CVE-critical-fix", Severity: "Critical", FixAvailable: true},
				repeated, repeated,
				{ID: "CVE-important-at", Severity: "Important", CVSS: new(7.0), Fixed: "2"},
				{ID: "CVE-important-unscored", Severity: "Important"},
				{ID: "CVE-important-open", Severity: "Important", CVSS: new(8.7)},
				{ID: "CVE-moderate-fix", Severity: "Moderate", Fixed: "2"},
				{ID: "CVE-moderate-open", Severity: "Moderate"},
				{ID: "CVE-low", Severity: "Low"},
			}},
		}},
	}
	body := newBrowser(t, &fakeApplication{snapshot: snapshot}).getBody(t)
	review := between(t, body, `id="rc-vulnerabilities"`, "</section>")
	if !strings.Contains(review, `aria-label="Vulnerability pill legend"`) {
		t.Fatal("missing color legend")
	}
	image := between(t, review, `class="rc-vulnerability-image"`, "</details>")
	header, details, ok := strings.Cut(image, "</summary>")
	if !ok {
		t.Fatal("missing image summary")
	}
	if strings.Contains(header, `" open`) || strings.Count(header, `class="pill `) != 3 ||
		!strings.Contains(header, `vulnerability-critical">2 Critical</span>`) ||
		!strings.Contains(header, `vulnerability-important-above" title="Fixable Important CVEs with CVSS above 7.0">1 Important`) ||
		!strings.Contains(header, `vulnerability-important-below" title="1 Important below 7.0, 1 fixable">1 Important`) {
		t.Fatalf("unexpected collapsed image: %s", header)
	}
	table := between(t, details, `class="rc-vulnerability-table"`, "</table>")
	if strings.Count(table, "<tr>") != 4 || strings.Index(table, "CVE-critical-open") > strings.Index(table, "CVE-important-fix") {
		t.Fatalf("expected three red and orange rows, with Critical first: %s", table)
	}
	for _, want := range []string{"Yes · fixable Critical", "Yes · fixable Important above 7.0", "No · no known fix"} {
		if !strings.Contains(table, want) {
			t.Errorf("table missing %q", want)
		}
	}
	for _, want := range []string{"1 Important below 7.0, 1 fixable.", "1 Important at 7.0, 1 fixable.",
		"1 Important without a CVSS score, none fixable.", "1 Important above 7.0 without a known fix.", "2 Moderate, 1 fixable.", "1 Low."} {
		if !strings.Contains(details, want) || strings.Contains(table, want) {
			t.Errorf("expanded summary has incorrect placement for %q", want)
		}
	}
}
