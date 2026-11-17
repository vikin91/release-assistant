package app

import (
	"errors"
	"testing"

	"release-assistant/internal/release"
)

func TestDiscoveryFailuresPreserveTheirSpecificSource(t *testing.T) {
	_, f, config := newPinningSession(t)
	previous := cloneRepository(f.repos["acs"])
	previous.PinningPins = release.Pins{Versions: map[string]string{"fact": "0.3.77"}, Evidence: observed()}
	for name, metadataOK := range map[string]bool{"metadata request failed": false, "branch failed but metadata succeeded": true} {
		t.Run(name, func(t *testing.T) {
			latest := release.Repository{Source: config.ACS(), Evidence: release.Evidence{At: observed().At, Error: "DNS lookup failed."}, PinningEvidence: release.Evidence{At: observed().At, Error: "Authentication failed (HTTP 401)."}}
			if metadataOK {
				pr := *previous.PinningPR
				pr.SHA = "new-pin-head"
				latest.PinningPR = &pr
				latest.PinningEvidence = observed()
				latest.PinningPins = release.Pins{Versions: map[string]string{"fact": "0.3.78"}, Evidence: observed()}
			}
			retained := retainDiscoveryFailure(previous, latest, errors.New("joined request diagnostics"))
			if retained.Pins.Evidence.Fresh() || retained.Pins.Evidence.Error != "DNS lookup failed." {
				t.Fatal("branch failure cause lost", retained.Pins)
			}
			if metadataOK {
				if !retained.PinningPR.Evidence.Fresh() || retained.PinningPR.SHA != "new-pin-head" || retained.PinningPins.Versions["fact"] != "0.3.78" {
					t.Fatal("independent pin reads were discarded")
				}
			} else if retained.PinningPR.Evidence.Fresh() || retained.PinningEvidence.Error != "Authentication failed (HTTP 401)." || retained.PinningPins.Versions["fact"] != "0.3.77" {
				t.Fatal("metadata error or retained prior version lost")
			}
			if previous.PinningPR.SHA != "pin-head" || !previous.PinningPR.Evidence.Fresh() {
				t.Fatal("previous observation mutated")
			}
		})
	}
}

func TestPinFailureMapsAreSnapshotCopies(t *testing.T) {
	s, f, config := newPinningSession(t)
	repo := f.repos["acs"]
	repo.Pins.Failures = map[string]string{"fact": "branch cause"}
	repo.PinningPins.Failures = map[string]string{"fact": "PR cause"}
	f.repos["acs"] = repo
	if err := s.Refresh(t.Context(), s.Snapshot().Generation, config.ACS(), true); err != nil {
		t.Fatal(err)
	}
	for _, r := range s.Snapshot().Repositories {
		if r.Source.Name == "acs" {
			r.Pins.Failures["fact"] = "changed"
			r.PinningPins.Failures["fact"] = "changed"
		}
	}
	for _, r := range s.Snapshot().Repositories {
		if r.Source.Name == "acs" && (r.Pins.Failures["fact"] != "branch cause" || r.PinningPins.Failures["fact"] != "PR cause") {
			t.Fatal("snapshot mutates pin failures")
		}
	}
}
