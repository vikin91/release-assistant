package app

import (
	"maps"
	"os"
	"path/filepath"
	"testing"

	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

func TestComponentTagChoicesSurviveRestartAndReleaseSwitch(t *testing.T) {
	s, f, config := mergedPinFixture(t, true)
	path := filepath.Join(t.TempDir(), "state.json")
	legacy := []byte(`{"releases":{"4.11.1":{"stackrox/fact#12":"included"}}}`)
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	open := func() *ReleaseState {
		t.Helper()
		state, err := OpenReleaseState(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		return state
	}
	s.options.ReleaseState = open()
	fact := f.repos["fact"]
	factPR := release.Target{Repo: fact.Source.Repo, PR: 12}
	fact.PRs = []release.PR{{Target: factPR, State: "open", Base: fact.Source.Branch, SHA: "pr-head", Evidence: observed()}}
	fact.Tags = append(fact.Tags, release.Tag{Name: "0.3.78", SHA: "newer", Belongs: release.Yes, Evidence: observed()})
	f.repos["fact"] = fact
	refreshAll(t, s)
	apply(t, s, Action{Kind: "tag", Component: "fact", Tag: "0.3.77", SHA: "fact-head"})
	apply(t, s, Action{Kind: "tag", Component: "fact", Tag: "0.3.78", SHA: "newer"})
	apply(t, s, Action{Kind: "tag", Component: "collector", Tag: "3.25.8", SHA: "collector-head"})
	apply(t, s, Action{Kind: "tag", Component: "scanner", Tag: "v2.40.1", SHA: "scanner-tag"})

	config.Version = "v4.11.1"
	restarted := New(f, f, f, dispatch.New(f, f, f, dispatch.Options{}), nil, Options{Initial: config, ReleaseState: open()})
	want := map[string]string{"fact": "0.3.78", "collector": "3.25.8", "scanner": "v2.40.1"}
	startup := restarted.Snapshot()
	if !maps.Equal(startup.PendingTags, want) || len(startup.Choices.Tags) != 0 {
		t.Fatalf("startup tags = %v pending, %v verified", startup.PendingTags, startup.Choices.Tags)
	}
	if startup.Choices.PRs[factPR] != release.Included {
		t.Fatal("saving tags removed the PR choice from the existing state file")
	}
	before := len(f.requests)
	if err := restarted.fetchPoll(t.Context(), restarted.generation, restarted.workRevision, config.Components[0], restarted.tagPollJob(config.Components[0])); err != nil {
		t.Fatal(err)
	}
	if len(f.requests) != before+1 || f.requests[before].ConfirmedTag != want["fact"] {
		t.Fatalf("first tag poll did not look up the saved tag by name: %+v", f.requests[before:])
	}
	refreshAll(t, restarted)
	if got := restarted.Snapshot(); !maps.Equal(got.Choices.Tags, want) || len(got.PendingTags) != 0 {
		t.Fatalf("observed tags = %v verified, %v pending", got.Choices.Tags, got.PendingTags)
	}
	apply(t, restarted, Action{Kind: "choice", Target: factPR, Choice: release.Excluded})
	apply(t, restarted, Action{Kind: "clear-tag", Component: "collector"})
	other := config
	other.Version = "4.11.2"
	apply(t, restarted, Action{Kind: "configure", Config: other})
	if got := restarted.Snapshot().PendingTags; len(got) != 0 {
		t.Fatalf("different patch inherited tag choices: %v", got)
	}
	apply(t, restarted, Action{Kind: "configure", Config: config})
	refreshAll(t, restarted)
	delete(want, "collector")
	if got := restarted.Snapshot().Choices.Tags; !maps.Equal(got, want) {
		t.Fatalf("returning to patch restored %v, want %v", got, want)
	}
	fresh := New(f, f, f, dispatch.New(f, f, f, dispatch.Options{}), nil, Options{Initial: config, ReleaseState: open()})
	refreshAll(t, fresh)
	if got := fresh.Snapshot(); !maps.Equal(got.Choices.Tags, want) || got.Choices.PRs[factPR] != release.Excluded {
		t.Fatalf("restart after PR change and tag clear restored tags %v and PR choices %v", got.Choices.Tags, got.Choices.PRs)
	}
}

func TestComponentTagSaveFailureLeavesSelectionInPlace(t *testing.T) {
	s, _, config := mergedPinFixture(t, false)
	state, err := OpenReleaseState(filepath.Join(t.TempDir(), "state.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	s.options.ReleaseState = state
	refreshAll(t, s)
	apply(t, s, Action{Kind: "tag", Component: "fact", Tag: "0.3.77", SHA: "fact-head"})
	state.path = t.TempDir()
	err = s.Apply(t.Context(), Action{Kind: "clear-tag", Component: "fact", Generation: s.generation})
	if err == nil {
		t.Fatal("clearing a tag succeeded despite a failed save")
	}
	if got := s.Snapshot().Choices.Tags["fact"]; got != "0.3.77" || state.file.ComponentTags[config.Version]["fact"].Tag != got {
		t.Fatalf("failed save changed the selected tag to %q", got)
	}
}

func TestSavedTagNeedsFreshMatchingEvidence(t *testing.T) {
	for name, tc := range map[string]struct {
		update  func(*release.Tag)
		pending bool
	}{
		"should reject a moved tag":        {func(tag *release.Tag) { tag.SHA = "moved" }, false},
		"should reject a different branch": {func(tag *release.Tag) { tag.Belongs = release.No }, false},
		"should wait after a read failure": {func(tag *release.Tag) { tag.Evidence.Known = false }, true},
	} {
		t.Run(name, func(t *testing.T) {
			_, f, config := mergedPinFixture(t, false)
			state, err := OpenReleaseState(filepath.Join(t.TempDir(), "state.json"), nil)
			if err != nil {
				t.Fatal(err)
			}
			fact := f.repos["fact"]
			tag := &fact.Tags[0]
			if err := state.saveComponentTag(config.Version, fact.Source, tag.Name, tag.SHA); err != nil {
				t.Fatal(err)
			}
			tc.update(tag)
			s := New(f, f, f, dispatch.New(f, f, f, dispatch.Options{}), nil, Options{Initial: config, ReleaseState: state})
			refreshAll(t, s)
			got := s.Snapshot()
			if got.Choices.Tags["fact"] != "" || (got.PendingTags["fact"] != "") != tc.pending {
				t.Fatalf("saved tag became %q verified and %q pending", got.Choices.Tags["fact"], got.PendingTags["fact"])
			}
		})
	}
}
