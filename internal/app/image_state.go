package app

import (
	"maps"
	"slices"
	"time"

	"release-assistant/internal/release"
)

type savedImageObservation struct {
	Present release.Truth `json:"present"`
	At      time.Time     `json:"at"`
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
		observation := savedImageObservation{Present: image.Present, At: image.Evidence.At}
		previous := s.file.ImageObservations[image.Reference]
		if observation.At.Before(previous.At) || (observation.At.Equal(previous.At) && observation.Present == previous.Present) {
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
