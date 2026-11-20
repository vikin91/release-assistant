// Package app coordinates one in-memory release session.
package app

import (
	"context"
	"release-assistant/internal/release"
)

type (
	// DiscoveryScope selects independently publishable repository observations.
	DiscoveryScope uint8
	// DiscoveryRequest scopes reads and preserves ignored and previously selected targets.
	DiscoveryRequest struct {
		Source           release.Source
		ConfirmedTag     string
		Tracked, Ignored []release.Target
		// PreviouslyOpen identifies PRs whose transition out of the open list needs verification.
		PreviouslyOpen []int
		// CoverageTag belongs to CoverageHead and supplies the selected commit for ancestry reads.
		CoverageTag  release.Tag
		CoverageHead string
		// PreviousCandidate supplies the verified tag and commit range for post-RC PR discovery.
		PreviousCandidate release.Candidate
		PinningPR         int
		DetectPinningPR   bool
		PreviousPinFiles  map[int]release.PinFileMatch
		Version           string
		Scope             DiscoveryScope
		// Progress publishes verified tags while the remaining inventory is still loading.
		Progress func(release.Repository)
		// CandidateProgress publishes verified ancestry before post-RC PR and pin reads finish.
		CandidateProgress func(head string, candidate release.Candidate)
	}
	// Discoverer returns partial observations alongside errors safe for diagnostic display.
	Discoverer interface {
		Discover(context.Context, DiscoveryRequest) (release.Repository, error)
	}
	// DiscoveryCache exposes controls for optional reusable discovery data.
	DiscoveryCache interface {
		Enabled() bool
		SetEnabled(bool)
		Clear()
	}
	// Planner delegates all retry eligibility to its adapter.
	Planner interface {
		Plan(context.Context, release.Target) (release.Plan, error)
	}
	// ManualRetryReader discovers instructions that only the engineer can execute.
	ManualRetryReader interface {
		ManualRetries(context.Context, release.Target) (release.ManualRetryReport, error)
	}
	// TagCheckReader resolves a release tag and reads checks on its commit.
	TagCheckReader interface {
		ReadTagChecks(context.Context, release.Target) (release.Validation, error)
	}
	// ComponentInspector defers release candidate reads while component prerequisites are monitored.
	ComponentInspector interface {
		InspectComponents(context.Context, release.Config, map[string]string) (release.Inventory, error)
	}
	// Inspector checks release images and reported build outputs without source reads.
	// The tags map carries the observed ACS candidate under "acs" when available.
	Inspector interface {
		Inspect(context.Context, release.Config, map[string]string) (release.Inventory, error)
		InspectReference(context.Context, string) (release.Image, error)
	}
)

const (
	// ReadAll preserves complete discovery for callers without an explicit scope.
	ReadAll DiscoveryScope = 0
	// ReadTags observes tag identity and ancestry without PR or build reads.
	ReadTags DiscoveryScope = 1 << (iota - 1)
	// ReadPRs lists open PRs on the configured branch.
	ReadPRs
	// ReadTargets refreshes explicitly tracked PR identities.
	ReadTargets
	// ReadChecks refreshes checks for the requested PRs.
	ReadChecks
	// ReadPins observes ACS branch and pinning-PR versions.
	ReadPins
	// ReadCoverage verifies selected merges against the requested tags.
	ReadCoverage
	// ReadPreviousPins observes the component baseline at the previous ACS patch tag.
	ReadPreviousPins
	// ReadRC lists patch and RC tags and verifies the current RC against the branch head.
	ReadRC

	// TagBatchRequests bounds the inventory work after resolving the branch head.
	TagBatchRequests = 32
)

// Reads distinguishes omitted work from a requested observation.
func (r DiscoveryRequest) Reads(scope DiscoveryScope) bool {
	return r.Scope == ReadAll || r.Scope&scope != 0
}
