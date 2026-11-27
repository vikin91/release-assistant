package web

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"release-assistant/internal/release"
)

type (
	vulnerabilityReview struct {
		release.RCVulnerabilities
		Images               []imageReview
		HoldSummary          holdCVESummary
		Available, Scheduled int
		Pending, Hold, Open  bool
	}

	holdCVESummary struct {
		Count  int
		Groups []holdCVEGroup
	}

	holdCVEGroup struct {
		Label      string
		ImageCount int
		CVEs       []holdCVE
	}

	holdCVE struct {
		Number  int
		ID, URL string
		Images  []string
		Fixes   []holdCVEFix
	}

	holdCVEFix struct {
		Detail string
		Images []string
	}

	imageReview struct {
		release.ImageVulnerabilities
		Rows                                                            []imageFinding
		Critical, ImportantAbove, ImportantBelow, ImportantBelowFixable int
		Summary                                                         string
		MeetsHold                                                       bool
	}

	imageFinding struct {
		release.Vulnerability
		HoldReason string
	}

	findingGroup int

	findingCounts map[string]bool
)

const (
	criticalFindings findingGroup = iota
	fixableImportantAbove
	importantBelow
	importantAt
	importantUnscored
	importantAboveWithoutFix
	moderateFindings
	lowFindings
	unknownFindings
	findingGroupCount
)

// HasVulnerabilityReview reports whether a configured release has an RC to review.
func (p pageModel) HasVulnerabilityReview() bool {
	return p.Configured && len(p.Snapshot.RCVulnerabilities) > 0
}

func (p pageModel) vulnerabilityStatus() tabStatus {
	if !p.HasVulnerabilityReview() {
		return tabStatus{}
	}
	current := p.Snapshot.RCVulnerabilities[0]
	ids := make(map[string]struct{})
	for _, image := range current.Images {
		for _, finding := range image.ReviewFindings() {
			if finding.SuggestsHold() {
				ids[finding.ID] = struct{}{}
			}
		}
	}
	count := len(ids)
	if count == 0 {
		return tabStatus{}
	}
	reason := fmt.Sprintf("%d distinct CVEs meet the internal hold criteria for %s.", count, current.Target.Tag)
	if count == 1 {
		reason = "1 CVE meets the internal hold criteria for " + current.Target.Tag + "."
	}
	return tabStatus{State: tabNeedsAttention, Reason: reason, Count: count}
}

// VulnerabilityReviews prepares current and previous results for a shared UI.
func (p pageModel) VulnerabilityReviews() []vulnerabilityReview {
	var reviews []vulnerabilityReview
	for i, rc := range p.Snapshot.RCVulnerabilities {
		review := vulnerabilityReview{RCVulnerabilities: rc, Open: i == 0 || !p.Snapshot.RCVulnerabilities[0].Complete()}
		for _, image := range rc.Images {
			row := reviewImageFindings(image)
			review.Hold = review.Hold || row.MeetsHold
			review.Pending = review.Pending || image.Status == "Pending"
			if image.Status != "Not scheduled" {
				review.Scheduled++
			}
			if image.Status == "Available" {
				review.Available++
			}
			review.Images = append(review.Images, row)
		}
		slices.SortStableFunc(review.Images, func(a, b imageReview) int {
			if a.MeetsHold == b.MeetsHold {
				return 0
			}
			if a.MeetsHold {
				return -1
			}
			return 1
		})
		if i == 0 {
			review.HoldSummary = summarizeHoldCVEs(review.Images)
		}
		reviews = append(reviews, review)
	}
	return reviews
}

func summarizeHoldCVEs(images []imageReview) holdCVESummary {
	var entries []holdCVE
	index := make(map[string]int)
	for _, image := range images {
		for _, finding := range image.Rows {
			if !finding.SuggestsHold() {
				continue
			}
			i, exists := index[finding.ID]
			if !exists {
				i = len(entries)
				index[finding.ID] = i
				entries = append(entries, holdCVE{ID: finding.ID})
			}
			entry := &entries[i]
			if !slices.Contains(entry.Images, image.Image) {
				entry.Images = append(entry.Images, image.Image)
			}
			if entry.URL == "" {
				entry.URL = safeURL(finding.URL)
			}
			fix := fmt.Sprintf("%s | CVSS %s | fixed version %s",
				cmp.Or(finding.Package, "unavailable"),
				finding.CVSSLabel(),
				cmp.Or(finding.Fixed, "unavailable"),
			)
			fixIndex := slices.IndexFunc(entry.Fixes, func(existing holdCVEFix) bool { return existing.Detail == fix })
			if fixIndex < 0 {
				fixIndex = len(entry.Fixes)
				entry.Fixes = append(entry.Fixes, holdCVEFix{Detail: fix})
			}
			fixImages := &entry.Fixes[fixIndex].Images
			if !slices.Contains(*fixImages, image.Image) {
				*fixImages = append(*fixImages, image.Image)
			}
		}
	}

	summary := holdCVESummary{Count: len(entries)}
	groupIndexes := make(map[string]int)
	for _, entry := range entries {
		slices.Sort(entry.Images)
		imageSet := strings.Join(entry.Images, " + ")
		i, exists := groupIndexes[imageSet]
		if !exists {
			i = len(summary.Groups)
			groupIndexes[imageSet] = i
			label := imageSet
			if len(entry.Images) == 1 {
				label += " only"
			}
			summary.Groups = append(summary.Groups, holdCVEGroup{Label: label, ImageCount: len(entry.Images)})
		}
		for j := range entry.Fixes {
			fix := &entry.Fixes[j]
			if len(fix.Images) < len(entry.Images) {
				slices.Sort(fix.Images)
				fix.Detail += " | " + strings.Join(fix.Images, ", ")
			}
		}
		group := &summary.Groups[i]
		entry.Number = len(group.CVEs) + 1
		group.CVEs = append(group.CVEs, entry)
	}
	slices.SortFunc(summary.Groups, func(a, b holdCVEGroup) int {
		return cmp.Or(cmp.Compare(len(b.CVEs), len(a.CVEs)), cmp.Compare(b.ImageCount, a.ImageCount), cmp.Compare(a.Label, b.Label))
	})
	return summary
}

func groupFinding(finding release.Vulnerability) findingGroup {
	switch strings.ToLower(finding.Severity) {
	case "critical":
		return criticalFindings
	case "important", "high":
		switch {
		case finding.CVSS == nil:
			return importantUnscored
		case *finding.CVSS < release.ImportantHoldCVSS:
			return importantBelow
		case *finding.CVSS == release.ImportantHoldCVSS:
			return importantAt
		case finding.HasFix():
			return fixableImportantAbove
		default:
			return importantAboveWithoutFix
		}
	case "moderate", "medium", "major":
		return moderateFindings
	case "low", "minor":
		return lowFindings
	default:
		return unknownFindings
	}
}

func reviewImageFindings(image release.ImageVulnerabilities) imageReview {
	row := imageReview{ImageVulnerabilities: image}
	var counts [findingGroupCount]findingCounts
	var importantRows []imageFinding
	for _, finding := range image.ReviewFindings() {
		group := groupFinding(finding)
		if counts[group] == nil {
			counts[group] = make(findingCounts)
		}
		counts[group][finding.ID] = counts[group][finding.ID] || finding.HasFix()

		meetsHold := finding.SuggestsHold()
		row.MeetsHold = row.MeetsHold || meetsHold
		if group != criticalFindings && group != fixableImportantAbove {
			continue
		}
		reason := "No · no known fix"
		switch {
		case meetsHold && group == criticalFindings:
			reason = "Yes · fixable Critical"
		case meetsHold:
			reason = "Yes · fixable Important above " + release.ImportantHoldCVSSLabel()
		case finding.HasFix():
			reason = "No · outside the internal hold criteria"
		}
		display := imageFinding{Vulnerability: finding, HoldReason: reason}
		if group == criticalFindings {
			row.Rows = append(row.Rows, display)
		} else {
			importantRows = append(importantRows, display)
		}
	}

	row.Rows = append(row.Rows, importantRows...)
	row.Critical = len(counts[criticalFindings])
	row.ImportantAbove = len(counts[fixableImportantAbove])
	row.ImportantBelow = len(counts[importantBelow])
	row.ImportantBelowFixable = counts[importantBelow].fixable()
	threshold := release.ImportantHoldCVSSLabel()
	var summary []string
	for _, entry := range []struct {
		Group     findingGroup
		Label     string
		ShowFixes bool
	}{
		{importantBelow, "Important below " + threshold, true},
		{importantAt, "Important at " + threshold, true},
		{importantUnscored, "Important without a CVSS score", true},
		{importantAboveWithoutFix, "Important above " + threshold + " without a known fix", false},
		{moderateFindings, "Moderate", true},
		{lowFindings, "Low", false},
		{unknownFindings, "of unknown severity", false},
	} {
		count := counts[entry.Group]
		if len(count) == 0 {
			continue
		}
		text := fmt.Sprintf("%d %s", len(count), entry.Label)
		if entry.ShowFixes {
			if fixable := count.fixable(); fixable > 0 {
				text += fmt.Sprintf(", %d fixable", fixable)
			} else {
				text += ", none fixable"
			}
		}
		summary = append(summary, text+".")
	}
	row.Summary = strings.Join(summary, " ")
	return row
}

func (counts findingCounts) fixable() int {
	total := 0
	for _, fixable := range counts {
		if fixable {
			total++
		}
	}
	return total
}
