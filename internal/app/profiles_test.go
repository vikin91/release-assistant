package app

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

func TestReleaseProfilesResumeOnlyDuePolls(t *testing.T) {
	state, err := OpenReleaseState(filepath.Join(t.TempDir(), "state.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	a := configuration()
	b := configuration()
	b.Version, b.Branch = "5.1.1", "release-5.1"
	for _, c := range []release.Config{a, b, a} {
		if err := state.SetInitialRelease(c, false); err != nil {
			t.Fatal(err)
		}
	}

	synctest.Test(t, func(t *testing.T) {
		_, f := newFixture()
		d := dispatch.New(f, f, f, dispatch.Options{EnableWrites: true})
		s := New(f, f, f, d, nil, Options{Profiles: true, ReleaseState: state, Discovery: 10 * time.Minute, Poll: 10 * time.Minute})
		profiles, active, _ := s.ReleaseProfiles()
		if len(profiles) != 2 || active != profiles[0].ID {
			t.Fatalf("startup profiles = %+v, active = %s", profiles, active)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go s.Run(ctx)
		requests := func() []DiscoveryRequest {
			f.mu.Lock()
			defer f.mu.Unlock()
			return slices.Clone(f.requests)
		}
		synctest.Wait()
		initial := len(requests())
		if initial == 0 {
			t.Fatal("startup skipped the active release")
		}
		for _, r := range requests() {
			if r.Version != a.Version && r.Version != "" {
				t.Fatalf("startup read inactive release: %+v", r)
			}
		}
		before := s.Snapshot()
		d.Interactive(false)
		view, found := s.ProfileSnapshot(profiles[1].ID)
		synctest.Wait()
		if !found || view.Config.Version != b.Version || view.Monitoring || view.WritesEnabled || len(requests()) != initial {
			t.Fatalf("browsing inactive release: found=%v, config=%+v, monitoring=%v, writes=%v, new reads=%d",
				found, view.Config, view.Monitoring, view.WritesEnabled, len(requests())-initial)
		}
		apply(t, s, Action{Kind: "activate-release", ProfileID: profiles[1].ID})
		synctest.Wait()
		if len(requests()) <= initial {
			t.Fatal("explicit activation skipped initialization")
		}
		total := len(requests())
		apply(t, s, Action{Kind: "activate-release", ProfileID: profiles[0].ID})
		synctest.Wait()
		after := s.Snapshot()
		paused, found := s.ProfileSnapshot(profiles[1].ID)
		if !found || paused.Monitoring || paused.WritesEnabled || len(paused.Repositories) == 0 {
			t.Fatalf("paused release lost its prior observations")
		}
		if len(requests()) != total || len(before.Repositories) != len(after.Repositories) || !after.Interactive || after.ResumedAt.IsZero() {
			t.Fatalf("switch back got %d new reads, %d repositories, Interactive=%v, resumed=%v",
				len(requests())-total, len(after.Repositories), after.Interactive, after.ResumedAt)
		}
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		if len(requests()) == total {
			t.Fatal("returning release did not refresh when due")
		}
		for _, r := range requests()[total:] {
			if r.Version != a.Version && r.Version != "" {
				t.Fatalf("resumed release refreshed inactive profile: %+v", r)
			}
		}
	})
}

func TestReleaseProfilesSaveDraftsRejectDuplicateAndRemoveActive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	state, err := OpenReleaseState(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	config := configuration()
	if err := state.SetInitialRelease(config, false); err != nil {
		t.Fatal(err)
	}
	_, f := newFixture()
	s := New(f, f, f, dispatch.New(f, f, f, dispatch.Options{}), nil, Options{Profiles: true, ReleaseState: state})
	apply(t, s, Action{Kind: "add-release"})
	profiles, active, _ := s.ReleaseProfiles()
	draft := profiles[1].ID
	apply(t, s, Action{Kind: "open-release", ProfileID: draft})
	if after, ok := s.ProfileSnapshot(draft); !ok || after.Config.Version != "" || s.activeProfile != active {
		t.Fatalf("draft snapshot = %+v, active = %s", after.Config, s.activeProfile)
	}
	next := config
	next.Version = "5.0.2"
	err = s.Apply(t.Context(), Action{Kind: "configure", ProfileID: draft, Config: next, Generation: s.Snapshot().Generation})
	if err == nil || !strings.Contains(err.Error(), "5.0 stream") {
		t.Fatalf("duplicate stream error = %v", err)
	}
	if err := state.savePRChoices(config.Version, map[release.Target]string{{Repo: "stackrox/stackrox", PR: 4}: "sha"}, release.Included); err != nil {
		t.Fatal(err)
	}
	apply(t, s, Action{Kind: "configure", ProfileID: active, Config: next})
	if after := s.Snapshot(); after.Config.Version != next.Version || len(after.Choices.PRs) != 0 {
		t.Fatalf("rollover = %+v, choices = %+v", after.Config, after.Choices.PRs)
	}
	apply(t, s, Action{Kind: "remove-release", ProfileID: active})
	reopened, err := OpenReleaseState(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.file.Profiles) != 1 || reopened.file.Profiles[0].ID != draft || reopened.file.ActiveProfile != "" || len(reopened.file.Releases) != 0 {
		t.Fatalf("persisted state = %+v", reopened.file)
	}
	if err := s.Apply(t.Context(), Action{Kind: "remove-release", ProfileID: draft}); err == nil {
		t.Fatal("removed the last profile")
	}
}
