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
		Source       release.Source
		ConfirmedTag string
		// AlternateTag provides the alternate spelling of a tag named by a pin.
		AlternateTag string
		// TagCandidates limits verification to explicit tag names.
		TagCandidates    []string
		Tracked, Ignored []release.Target
		// PreviouslyOpen identifies PRs whose transition out of the open list needs verification.
		PreviouslyOpen []int
		// PreviousCandidate supplies the verified tag and commit range for post-RC PR discovery.
		PreviousCandidate release.Candidate
		PinningPR         int
		DetectPinningPR   bool
		// ComparePinDiffs retains exact pin-only signatures for PR creation.
		ComparePinDiffs  bool
		PreviousPinFiles map[int]release.PinFileMatch
		Version          string
		Scope            DiscoveryScope
		// TagRefresh identifies a manual inventory restart across its bounded batches.
		TagRefresh string
		// Progress publishes verified tags while the remaining inventory is still loading.
		Progress func(release.Repository)
		// CandidateProgress publishes verified ancestry before post-RC PR and pin reads finish.
		CandidateProgress func(head string, candidate release.Candidate)
	}
	// Discoverer returns partial observations alongside errors safe for diagnostic display.
	Discoverer interface {
		Discover(context.Context, DiscoveryRequest) (release.Repository, error)
	}
	// ComponentTagNameReader lists versioned tag names without resolving their commits.
	ComponentTagNameReader interface {
		ReadComponentTagNames(context.Context, release.Source) ([]string, error)
	}
	// PRReader verifies one PR's metadata with a single GitHub read.
	PRReader interface {
		ReadPR(context.Context, release.Target) (release.PR, error)
	}
	// RetryPlanner reads eligibility for one target and may return verified offers with a partial-read error.
	RetryPlanner interface {
		PlanRetries(context.Context, release.Target) (release.RetryPlan, error)
	}
	// TagCheckReader resolves a release tag and reads checks on its commit.
	TagCheckReader interface {
		ReadTagChecks(context.Context, release.Target) (release.Validation, error)
	}
	// ComponentInspector defers release candidate reads while component prerequisites are monitored.
	ComponentInspector interface {
		InspectComponents(context.Context, release.Config, map[string]string) (release.Inventory, error)
	}
	// ProgressiveInspector reports inventory rows as each registry check completes.
	ProgressiveInspector interface {
		InspectImages(context.Context, release.Config, map[string]string, bool, func(release.Inventory)) (release.Inventory, error)
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
