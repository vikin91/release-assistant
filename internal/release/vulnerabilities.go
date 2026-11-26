package release

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

type (
	// Vulnerability describes one affected package in an image scan.
	Vulnerability struct {
		ID, Severity, Package, Installed, Fixed, URL, AdvisoryURL string
		CVSS                                                      *float64
		FixAvailable                                              bool
	}

	// ImageVulnerabilities records the available evidence for one Konflux image.
	ImageVulnerabilities struct {
		Image, Status, ScanURL, ConformaURL, ConformaStatus string
		ArtifactID                                          int64
		ConformaReported                                    bool
		ConformaDetails                                     string
		ConformaAppID                                       int64
		ConformaReport                                      ConformaReport
		Findings                                            []Vulnerability
	}

	// RCScanWorkflow identifies the latest tag-triggered image scan run.
	RCScanWorkflow struct {
		ID     int64
		Status string
	}

	// RCVulnerabilities binds a vulnerability review to a tag and commit.
	RCVulnerabilities struct {
		Target                                   Target
		SHA, DescendantSHA, WorkflowURL, Problem string
		Images                                   []ImageVulnerabilities
	}

	// ConformaReport contains CVE observations bound to the verified image digest.
	ConformaReport struct {
		Digest, RunURL string
		Findings       []ConformaFinding
	}

	// ConformaFinding identifies the policy result for a CVE on one image architecture.
	ConformaFinding struct {
		ID, Architecture, Rule, Message string
	}

	holdRule struct {
		severity string
		above    *float64
	}
)

// ImportantHoldCVSS is the exclusive score threshold for fixable Important CVEs.
const ImportantHoldCVSS = 7.0

func holdRules() []holdRule {
	return []holdRule{{severity: "Critical"}, {severity: "Important", above: new(ImportantHoldCVSS)}}
}

// HoldCriteria returns the findings that qualify for an advisory hold.
func HoldCriteria() string {
	var criteria []string
	for _, rule := range holdRules() {
		criterion := "a fixable " + rule.severity + " CVE"
		if rule.above != nil {
			criterion += fmt.Sprintf(" with a CVSS score above %s", scoreText(*rule.above))
		}
		criteria = append(criteria, criterion)
	}
	return strings.TrimPrefix(strings.Join(criteria, " or "), "a ")
}

// ImportantHoldCVSSLabel formats the Important CVE threshold for the review.
func ImportantHoldCVSSLabel() string { return scoreText(ImportantHoldCVSS) }

func scoreText(score float64) string {
	value := fmt.Sprint(score)
	if !strings.Contains(value, ".") {
		value += ".0"
	}
	return value
}

// CVSSLabel distinguishes an unavailable score from a reported zero.
func (v Vulnerability) CVSSLabel() string {
	if v.CVSS == nil {
		return "—"
	}
	return scoreText(*v.CVSS)
}

// HasFix reports whether either source identified an available fix.
func (v Vulnerability) HasFix() bool { return v.Fixed != "" || v.FixAvailable }

// SuggestsHold reports whether the finding meets the advisory hold criteria.
func (v Vulnerability) SuggestsHold() bool {
	if !v.HasFix() {
		return false
	}
	return slices.ContainsFunc(holdRules(), func(rule holdRule) bool {
		return strings.EqualFold(v.Severity, rule.severity) &&
			(rule.above == nil || v.CVSS != nil && *v.CVSS > *rule.above)
	})
}

// SortVulnerabilities puts hold findings first, then orders each group by severity and score.
func SortVulnerabilities(findings []Vulnerability) {
	severity := func(value string) int {
		switch strings.ToLower(value) {
		case "critical":
			return 4
		case "important", "high":
			return 3
		case "moderate", "medium", "major":
			return 2
		case "low", "minor":
			return 1
		default:
			return 0
		}
	}
	score := func(v Vulnerability) float64 {
		if v.CVSS == nil {
			return -1
		}
		return *v.CVSS
	}
	slices.SortStableFunc(findings, func(a, b Vulnerability) int {
		if a.SuggestsHold() != b.SuggestsHold() {
			if a.SuggestsHold() {
				return -1
			}
			return 1
		}
		return cmp.Or(cmp.Compare(severity(b.Severity), severity(a.Severity)),
			cmp.Compare(score(b), score(a)), cmp.Compare(a.ID, b.ID), cmp.Compare(a.Package, b.Package))
	})
}

// ReviewFindings adds CVEs reported only by Conforma to the scan findings.
func (image ImageVulnerabilities) ReviewFindings() []Vulnerability {
	if len(image.ConformaReport.Findings) == 0 {
		return image.Findings
	}
	findings := slices.Clone(image.Findings)
	seen := make(map[string]bool, len(findings))
	for _, finding := range findings {
		seen[finding.ID] = true
	}
	for _, finding := range image.ConformaReport.Findings {
		if seen[finding.ID] {
			continue
		}
		var fixable bool
		switch finding.Rule {
		case "cve.cve_blockers", "cve.cve_warnings":
			fixable = true
		case "cve.unpatched_cve_blockers", "cve.unpatched_cve_warnings":
		default:
			continue
		}
		before, ok := strings.CutSuffix(finding.Message, " security level")
		_, severity, hasSeverity := strings.Cut(before, " vulnerability of ")
		if !ok || !hasSeverity {
			continue
		}
		switch severity {
		case "high":
			severity = "Important"
		case "critical":
			severity = "Critical"
		case "medium":
			severity = "Moderate"
		case "low":
			severity = "Low"
		default:
			severity = "Unknown"
		}
		seen[finding.ID] = true
		findings = append(findings, Vulnerability{ID: finding.ID, Severity: severity, FixAvailable: fixable})
	}
	SortVulnerabilities(findings)
	return findings
}

// ScansComplete reports whether every expected Konflux image has a scan report.
func (r RCVulnerabilities) ScansComplete() bool {
	return r.Problem == "" && slices.ContainsFunc(r.Images, func(image ImageVulnerabilities) bool {
		return image.Status == "Available"
	}) && !slices.ContainsFunc(r.Images, func(image ImageVulnerabilities) bool {
		return image.Status != "Available" && image.Status != "Not scheduled"
	})
}

// ConformaReported reports whether a developer check published a policy result.
func (r RCVulnerabilities) ConformaReported() bool {
	return slices.ContainsFunc(r.Images, func(image ImageVulnerabilities) bool { return image.ConformaReported })
}

// Complete reports whether all scans and at least one Conforma policy result are available.
func (r RCVulnerabilities) Complete() bool {
	return r.ScansComplete() && r.ConformaReported()
}

// PreviousCandidate selects the highest-numbered observed RC below the current tag.
// Its ancestry must be verified before using its results.
func PreviousCandidate(repo Repository) Candidate {
	current := repo.Candidate
	version, _, ok := strings.Cut(strings.TrimPrefix(current.Target.Tag, "v"), "-rc.")
	if !ok {
		return Candidate{}
	}
	var previous Candidate
	for _, tag := range repo.Tags {
		suffix, match := strings.CutPrefix(strings.TrimPrefix(tag.Name, "v"), version+"-rc.")
		number, err := strconv.ParseUint(suffix, 10, 64)
		if !match || err != nil || strconv.FormatUint(number, 10) != suffix || number >= current.Number || !tag.Evidence.Fresh() || tag.SHA == "" ||
			(previous.Target.Valid() && number <= previous.Number) {
			continue
		}
		previous = Candidate{Target: Target{Repo: current.Target.Repo, Tag: tag.Name}, SHA: tag.SHA, Number: number, Evidence: tag.Evidence}
	}
	return previous
}
