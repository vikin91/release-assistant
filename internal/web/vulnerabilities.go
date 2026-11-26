package web

import (
	"fmt"
	"slices"

	"release-assistant/internal/release"
)

type (
	vulnerabilityReview struct {
		release.RCVulnerabilities
		Images               []imageReview
		Available, Scheduled int
		Pending, Hold, Open  bool
	}

	imageReview struct {
		release.ImageVulnerabilities
		Hold, Other []release.Vulnerability
	}
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
			row := imageReview{ImageVulnerabilities: image}
			for _, finding := range image.ReviewFindings() {
				if finding.SuggestsHold() {
					row.Hold = append(row.Hold, finding)
				} else {
					row.Other = append(row.Other, finding)
				}
			}
			review.Hold = review.Hold || len(row.Hold) > 0
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
			if (len(a.Hold) > 0) == (len(b.Hold) > 0) {
				return 0
			}
			if len(a.Hold) > 0 {
				return -1
			}
			return 1
		})
		reviews = append(reviews, review)
	}
	return reviews
}
