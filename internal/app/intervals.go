package app

import (
	"cmp"
	"fmt"
	"time"
)

type (
	// RefreshKey identifies one configurable polling interval.
	RefreshKey string
	// RefreshIntervals maps each kind of release data to its polling interval.
	RefreshIntervals map[RefreshKey]time.Duration
)

const (
	// BeforePRs controls component PR discovery before tag selection.
	BeforePRs RefreshKey = "before_prs"
	// BeforePRChecks controls component PR check reads before tag selection.
	BeforePRChecks RefreshKey = "before_pr_checks"
	// BeforeTags controls verification of saved component tags and tags named by merged pins.
	BeforeTags RefreshKey = "before_tags"
	// AfterPRs controls component PR discovery after tag selection.
	AfterPRs RefreshKey = "after_prs"
	// AfterPRChecks controls component PR check reads after tag selection.
	AfterPRChecks RefreshKey = "after_pr_checks"
	// AfterTags controls selected tag verification and retries for component tag PR history.
	AfterTags RefreshKey = "after_tags"
	// ComponentTagChecks controls GitHub checks for each selected component tag.
	ComponentTagChecks RefreshKey = "component_tag_checks"
	// ACSPRs controls PR discovery on the ACS release branch.
	ACSPRs RefreshKey = "acs_prs"
	// ACSPRChecks controls GitHub check reads for tracked ACS PRs.
	ACSPRChecks RefreshKey = "acs_pr_checks"
	// ACSTags controls discovery of ACS release tags.
	ACSTags RefreshKey = "acs_tags"
	// ACSTagChecks controls GitHub checks for the current ACS release candidate.
	ACSTagChecks RefreshKey = "acs_tag_checks"
	// KonfluxBuilds controls build status reads for tracked component and ACS targets.
	KonfluxBuilds RefreshKey = "konflux_builds"
	// ReleaseImages controls registry reads for release images.
	ReleaseImages RefreshKey = "release_images"
)

// DefaultRefreshIntervals returns independent session defaults. A positive poll
// or discovery override sets all status or discovery intervals, respectively.
func DefaultRefreshIntervals(poll, discovery time.Duration) RefreshIntervals {
	poll, discovery = max(0, poll), max(0, discovery)
	return RefreshIntervals{
		BeforePRs:          cmp.Or(discovery, 5*time.Minute),
		BeforePRChecks:     cmp.Or(poll, 5*time.Minute),
		BeforeTags:         cmp.Or(discovery, 10*time.Minute),
		AfterPRs:           cmp.Or(discovery, 15*time.Minute),
		AfterPRChecks:      cmp.Or(poll, 30*time.Minute),
		AfterTags:          cmp.Or(discovery, 10*time.Minute),
		ComponentTagChecks: cmp.Or(poll, 10*time.Minute),
		ACSPRs:             cmp.Or(discovery, 5*time.Minute),
		ACSPRChecks:        cmp.Or(poll, 5*time.Minute),
		ACSTags:            cmp.Or(discovery, 10*time.Minute),
		ACSTagChecks:       cmp.Or(poll, 5*time.Minute),
		KonfluxBuilds:      cmp.Or(poll, 5*time.Minute),
		ReleaseImages:      cmp.Or(poll, 10*time.Minute),
	}
}

// Validate checks the supplied intervals. Omitted keys retain their session values.
func (intervals RefreshIntervals) Validate() error {
	known := DefaultRefreshIntervals(0, 0)
	for key, duration := range intervals {
		if _, ok := known[key]; !ok {
			return fmt.Errorf("unknown refresh interval %q", key)
		}
		if duration <= 0 {
			return fmt.Errorf("refresh interval %q must be positive", key)
		}
	}
	return nil
}
