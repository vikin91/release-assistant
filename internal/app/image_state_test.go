package app

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"release-assistant/internal/release"
)

type progressiveImageReader struct {
	*fixture
	inspect func(bool, func(release.Inventory)) (release.Inventory, error)
}

func (r progressiveImageReader) InspectImages(_ context.Context, _ release.Config, _ map[string]string, force bool, progress func(release.Inventory)) (release.Inventory, error) {
	return r.inspect(force, progress)
}

func TestImageRefreshShowsSavedRowsUntilEachRegistryResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	state, err := OpenReleaseState(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	config := configuration()
	reference := "quay.io/rhacs-eng/fact:0.3.77"
	inventory := release.Inventory{
		ReleaseVersion: config.Version,
		Versions:       map[string]string{"fact": "0.3.77"},
		Evidence:       observed(),
		Images: []release.Image{{
			Component: "fact", Name: "fact", Family: "ci", Reference: reference,
			Present: release.Yes, Evidence: observed(),
		}, {
			Component: "fact", Name: "fact", Family: "konflux", Reference: "quay.io/rhacs-eng/release-fact:0.3.77-fast",
			Present: release.No, Evidence: observed(),
		}},
	}
	if err := state.saveImageObservations(inventory); err != nil {
		t.Fatal(err)
	}
	state, err = OpenReleaseState(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, f := newFixture()
	s.options.ReleaseState = state
	apply(t, s, Action{Kind: "configure", Config: config})
	apply(t, s, Action{Kind: "refresh-images"})
	before := inventory.Images[0]
	needsForce := true
	s.images = progressiveImageReader{fixture: f, inspect: func(force bool, progress func(release.Inventory)) (release.Inventory, error) {
		if force != needsForce {
			t.Fatalf("force=%t, want %t", force, needsForce)
		}
		pending := inventory
		pending.Images = slices.Clone(inventory.Images)
		for i := range pending.Images {
			pending.Images[i].Present = ""
			pending.Images[i].Evidence = release.Evidence{}
		}
		progress(pending)
		if needsForce {
			shown := s.Snapshot().Inventory.Images[0]
			if shown.Evidence.Fresh() || shown.Present != release.Yes || !shown.Evidence.At.Equal(before.Evidence.At) {
				t.Fatalf("saved result incorrectly displayed: %+v", shown)
			}
		}
		partial := pending
		partial.Images = slices.Clone(pending.Images)
		partial.Images[0] = inventory.Images[0]
		partial.Images[0].Present = release.No
		partial.Images[0].Evidence.At = before.Evidence.At.Add(time.Second)
		progress(partial)
		shown := s.Snapshot().Inventory.Images
		if !shown[0].Evidence.Fresh() || shown[0].Present != release.No || shown[1].Present != release.No ||
			!shown[1].Evidence.At.Equal(inventory.Images[1].Evidence.At) {
			t.Fatalf("refresh replaced an independent result: %+v", shown)
		}
		inventory.Images[0] = partial.Images[0]
		return inventory, nil
	}}
	source := config.ACS()
	for range 2 {
		for _, job := range s.pollJobs(source) {
			if job.kind == pollImages {
				s.runPoll(t.Context(), s.generation, s.workRevision, source, job)
			}
		}
		needsForce = false
		// Make the next ordinary check due without modifying the request counter.
		id := pollID{source.Repo, pollImages}
		record := s.polls[id]
		record.finished = time.Time{}
		s.polls[id] = record
	}
	state, err = OpenReleaseState(path, nil)
	if err != nil || state.file.ImageObservations[reference].Present != release.No {
		t.Fatalf("updated observation was not saved: %v", err)
	}
}

func TestImageDigestChangeSurvivesRestartsAndFailedReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := newFixture()
	s.config = configuration()
	firstSeen := time.Now().Add(-time.Hour)
	updated := firstSeen.Add(20 * time.Minute)
	const ref = "quay.io/rhacs-eng/scanner:2.39.28"

	for _, step := range []struct {
		name, digest string
		at, want     time.Time
		fail         bool
	}{
		{name: "first sighting establishes a baseline", digest: "first", at: firstSeen},
		{name: "failed read retains the baseline", at: firstSeen.Add(time.Minute), fail: true},
		{name: "new digest records the detection time", digest: "second", at: updated, want: updated},
		{name: "the same digest keeps its detection time", digest: "second", at: updated.Add(time.Minute), want: updated},
	} {
		t.Run(step.name, func(t *testing.T) {
			state, err := OpenReleaseState(path, nil)
			if err != nil {
				t.Fatal(err)
			}
			s.options.ReleaseState = state
			s.inventory = release.Inventory{}
			rows := []release.Image{{
				Reference: ref, Present: release.Yes, ManifestDigest: step.digest,
				Evidence: release.Evidence{At: step.at, Known: !step.fail},
			}}
			s.trackImageDigestChanges(rows)
			if !rows[0].DigestChangedAt.Equal(step.want) {
				t.Fatalf("got change time %v, want %v", rows[0].DigestChangedAt, step.want)
			}
			if err := state.saveImageObservations(release.Inventory{Images: rows}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
