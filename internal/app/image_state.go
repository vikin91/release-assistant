package app

import (
	"maps"
	"slices"
	"time"

	"release-assistant/internal/release"
)

type savedImageObservation struct {
	Present         release.Truth `json:"present"`
	At              time.Time     `json:"at"`
	Digest          string        `json:"digest,omitempty"`
	DigestChangedAt time.Time     `json:"digest_changed_at,omitzero"`
}

// saveImageObservations commits verified registry results without changing user choices.
func (s *ReleaseState) saveImageObservations(inventory release.Inventory) error {
	if s == nil {
		return nil
	}
	next := s.file
	var observations map[string]savedImageObservation
	for _, image := range slices.Concat(inventory.Images, inventory.ReleaseImages) {
		if image.Reference == "" || !image.Evidence.Fresh() ||
			(image.Present != release.Yes && image.Present != release.No) {
			continue
		}
		observation := savedImageObservation{Present: image.Present, At: image.Evidence.At, Digest: image.ManifestDigest, DigestChangedAt: image.DigestChangedAt}
		previous := s.file.ImageObservations[image.Reference]
		if observation.At.Before(previous.At) || (observation.At.Equal(previous.At) &&
			observation.Present == previous.Present && observation.Digest == previous.Digest && observation.DigestChangedAt.Equal(previous.DigestChangedAt)) {
			continue
		}
		if observations == nil {
			observations = make(map[string]savedImageObservation, len(s.file.ImageObservations)+1)
			maps.Copy(observations, s.file.ImageObservations)
		}
		observations[image.Reference] = observation
	}
	if observations == nil {
		return nil
	}
	next.ImageObservations = observations
	return s.commit(next)
}

// trackImageDigestChanges compares each tag with its last verified manifest without treating the first sighting as a change.
func (s *Session) trackImageDigestChanges(rows []release.Image) {
	saved := s.dashboardForUpdate()
	for index := range rows {
		image := &rows[index]
		if image.Reference == "" {
			continue
		}
		previous := savedImageObservation{}
		if s.activeProfile != "" {
			for _, old := range saved.Images[image.Component].Rows {
				if old.Reference == image.Reference {
					previous = savedImageObservation{At: old.Evidence.At, Digest: old.ManifestDigest, DigestChangedAt: old.DigestChangedAt}
					break
				}
			}
		} else if state := s.options.ReleaseState; state != nil {
			previous = state.file.ImageObservations[image.Reference]
		}
		for _, old := range s.inventory.Images {
			if old.Reference == image.Reference && old.ManifestDigest != "" && !old.Evidence.At.Before(previous.At) {
				previous = savedImageObservation{At: old.Evidence.At, Digest: old.ManifestDigest, DigestChangedAt: old.DigestChangedAt}
				break
			}
		}
		if previous.Digest == "" || image.Evidence.At.Before(previous.At) {
			continue
		}
		image.DigestChangedAt = previous.DigestChangedAt
		if !image.Evidence.Fresh() || image.ManifestDigest == "" {
			image.ManifestDigest = previous.Digest
		} else if image.ManifestDigest != previous.Digest {
			image.DigestChangedAt = image.Evidence.At
		}
	}
}

// restorePendingImages fills unresolved rows by exact reference. Saved data is display evidence.
func (s *ReleaseState) restorePendingImages(current, previous []release.Image) {
	for index, image := range current {
		if image.Reference == "" || !image.Evidence.At.IsZero() {
			continue
		}
		if previousIndex := slices.IndexFunc(previous, func(old release.Image) bool {
			return old.Reference == image.Reference && old.Component == image.Component && old.Family == image.Family
		}); previousIndex >= 0 && !previous[previousIndex].Evidence.At.IsZero() {
			current[index] = previous[previousIndex]
			continue
		}
		if s != nil {
			saved := s.file.ImageObservations[image.Reference]
			if !saved.At.IsZero() && (saved.Present == release.Yes || saved.Present == release.No) {
				current[index].Present, current[index].Evidence.At = saved.Present, saved.At
			}
		}
	}
}
