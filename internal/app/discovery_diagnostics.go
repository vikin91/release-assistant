package app

import (
	"slices"
	"time"

	"release-assistant/internal/release"
)

// retainDiscoveryFailure keeps last values while retaining independently successful PR reads.
func retainDiscoveryFailure(previous, latest release.Repository, cause error) release.Repository {
	retained := cloneRepository(previous)
	evidence := latest.Evidence
	if evidence.At.IsZero() {
		evidence.At = time.Now()
	}
	evidence.Known = false
	if evidence.Error == "" {
		evidence.Error = cause.Error()
	}
	retained.Evidence = evidence
	retained.PinDiscovery = latest.PinDiscovery
	retained.PinDiscovery.Evidence = evidence
	if previous.Source.Name == "acs" || latest.Source.Name == "acs" {
		retained.Candidate = latest.Candidate
		if retained.Candidate.Evidence.At.IsZero() {
			retained.Candidate.Evidence = evidence
		}
	}
	retained.Pins.Evidence = evidence
	retained.Pins.Failures = nil
	if latest.Pins.Evidence.Error != "" {
		retained.Pins.Evidence = latest.Pins.Evidence
		retained.Pins.Evidence.Known = false
	}
	for i := range retained.PRs {
		retained.PRs[i].Evidence.Known = false
		retained.PRs[i].Evidence.Error = evidence.Error
		retained.PRs[i].ChecksEvidence.Known = false
		retained.PRs[i].CheckResultsEvidence.Known = false
	}
	for i := range retained.Tags {
		retained.Tags[i].Evidence.Known = false
	}
	retained.PinningPins.Evidence = evidence
	retained.PinningPins.Failures = nil
	retained.PinningEvidence = latest.PinningEvidence
	if retained.PinningEvidence.At.IsZero() {
		retained.PinningEvidence = evidence
	}
	if retained.PinningPR != nil {
		pr := *retained.PinningPR
		pr.Evidence, pr.ChecksEvidence = retained.PinningEvidence, retained.PinningEvidence
		pr.Evidence.Known, pr.ChecksEvidence.Known = false, false
		pr.CheckResultsEvidence.Known = false
		retained.PinningPR = &pr
	}
	if latest.PinningPR != nil {
		retained.PinningPR = latest.PinningPR
		retained.PinningPins = latest.PinningPins
		if latest.PinningPR.Evidence.Fresh() {
			index := slices.IndexFunc(retained.PRs, func(pr release.PR) bool { return pr.Target == latest.PinningPR.Target })
			if index < 0 {
				retained.PRs = append(retained.PRs, *latest.PinningPR)
			} else {
				retained.PRs[index] = *latest.PinningPR
			}
		}
	}
	return retained
}
