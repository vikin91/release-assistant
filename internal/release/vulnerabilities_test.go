package release

import (
	"strings"
	"testing"
)

func TestVulnerabilityHoldCriteria(t *testing.T) {
	for name, test := range map[string]struct {
		finding Vulnerability
		want    bool
	}{
		"fixable critical without a score": {Vulnerability{Severity: "Critical", Fixed: "2"}, true},
		"fixable important above seven":    {Vulnerability{Severity: "Important", Fixed: "2", CVSS: new(7.1)}, true},
		"important exactly seven":          {Vulnerability{Severity: "Important", Fixed: "2", CVSS: new(7.0)}, false},
		"important without a score":        {Vulnerability{Severity: "Important", Fixed: "2"}, false},
		"critical without a fix":           {Vulnerability{Severity: "Critical", CVSS: new(10.0)}, false},
		"moderate above seven":             {Vulnerability{Severity: "Moderate", Fixed: "2", CVSS: new(8.0)}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := test.finding.SuggestsHold(); got != test.want {
				t.Fatalf("SuggestsHold() = %v, want %v", got, test.want)
			}
		})
	}
	text := HoldCriteria()
	if !strings.Contains(text, "fixable Critical CVE") || !strings.Contains(text, "fixable Important CVE with a CVSS score above 7.0") {
		t.Fatalf("unexpected hold criteria: %s", text)
	}
}

func TestSortVulnerabilitiesByHoldSeverityAndScore(t *testing.T) {
	rows := []Vulnerability{
		{ID: "important without score", Severity: "Important"},
		{ID: "critical without fix", Severity: "Critical", CVSS: new(9.2)},
		{ID: "important with score", Severity: "Important", CVSS: new(8.4)},
		{ID: "fixable important", Severity: "Important", Fixed: "2", CVSS: new(7.8)},
		{ID: "moderate", Severity: "Moderate", Fixed: "2", CVSS: new(9.0)},
	}
	SortVulnerabilities(rows)
	for i, want := range []string{"fixable important", "critical without fix", "important with score", "important without score", "moderate"} {
		if rows[i].ID != want {
			t.Fatalf("row %d = %s, want %s", i, rows[i].ID, want)
		}
	}
}

func TestReviewFindingsIncludeConformaOnlyCVEsWithoutInventingScores(t *testing.T) {
	image := ImageVulnerabilities{
		Findings: []Vulnerability{{ID: "CVE-scan", Severity: "Important", CVSS: new(8.0), Fixed: "2"}},
		ConformaReport: ConformaReport{Findings: []ConformaFinding{
			{ID: "CVE-scan", Rule: "cve.cve_blockers", Message: `Found "CVE-scan" vulnerability of high security level`},
			{ID: "CVE-policy", Rule: "cve.cve_blockers", Message: `Found "CVE-policy" vulnerability of critical security level`},
			{ID: "CVE-missing-score", Rule: "cve.cve_blockers", Message: `Found "CVE-missing-score" vulnerability of high security level`},
		}},
	}
	got := image.ReviewFindings()
	if len(got) != 3 || got[0].ID != "CVE-policy" || !got[0].SuggestsHold() || got[2].SuggestsHold() || got[2].CVSS != nil {
		t.Fatalf("unexpected combined findings %+v", got)
	}
}
