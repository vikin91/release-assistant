package app

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

func TestPRStateRestoresChoicesByPatchRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	var output bytes.Buffer
	state, err := OpenReleaseState(path, NewLog(&output, false))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Info: No state file found") || !strings.Contains(output.String(), "Writing a new one.") {
		t.Fatalf("missing first-start message in %q", output.String())
	}
	if data, err := os.ReadFile(path); err != nil || !strings.Contains(string(data), `"releases": {}`) {
		t.Fatalf("missing initial state file: %s, %v", data, err)
	}
	_, services := newFixture()
	config := configuration()
	fact := release.Target{Repo: config.Components[0].Repo, PR: 137}
	collector := release.Target{Repo: config.Components[1].Repo, PR: 137}
	acs := release.Target{Repo: config.ACS().Repo, PR: 137}
	hidden := release.Target{Repo: config.ACS().Repo, PR: 138}
	for name, targets := range map[string][]release.Target{"fact": {fact}, "collector": {collector}, "acs": {acs, hidden}} {
		repo := services.repos[name]
		for _, target := range targets {
			repo.PRs = append(repo.PRs, release.PR{Target: target, State: "open", Base: repo.Source.Branch, SHA: "head", Evidence: observed()})
		}
		services.repos[name] = repo
	}
	session := func(state *ReleaseState, initial release.Config) *Session {
		return New(services, services, services, dispatch.New(services, services, services, dispatch.Options{}), nil,
			Options{Initial: initial, ReleaseState: state})
	}
	original := session(state, release.Config{})
	apply(t, original, Action{Kind: "configure", Config: config})
	refreshAll(t, original)
	apply(t, original, Action{Kind: "component-pr-choices", Component: "fact", Choice: release.Included})
	apply(t, original, Action{Kind: "choice", Target: collector, Choice: release.Excluded})
	apply(t, original, Action{Kind: "acs-pr-choices", Choice: release.Ignored})
	apply(t, original, Action{Kind: "choice", Target: acs, Choice: release.Excluded})

	next := config
	next.Version = "5.0.2"
	apply(t, original, Action{Kind: "configure", Config: next})
	if got := original.Snapshot().Choices.PRs; len(got) != 0 {
		t.Fatalf("new patch release inherited choices: %v", got)
	}
	refreshAll(t, original)
	apply(t, original, Action{Kind: "choice", Target: fact, Choice: release.Excluded})

	reloaded, err := OpenReleaseState(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	config.Version = "v5.0.1"
	config.PinningPR = acs.PR
	restarted := session(reloaded, config)
	want := map[release.Target]release.Choice{fact: release.Included, collector: release.Excluded, acs: release.Included, hidden: release.Ignored}
	if got := restarted.Snapshot().Choices.PRs; !maps.Equal(got, want) {
		t.Fatalf("startup choices = %v, want %v", got, want)
	}
	config.PinningPR = 0
	apply(t, restarted, Action{Kind: "configure", Config: config})
	want[acs] = release.Excluded
	if got := restarted.Snapshot().Choices.PRs; !maps.Equal(got, want) {
		t.Fatalf("choices after clearing pinning role = %v, want %v", got, want)
	}

	apply(t, restarted, Action{Kind: "choice", Target: hidden, Choice: release.Included})
	if got := restarted.Snapshot().Choices.PRs[hidden]; got != release.Included {
		t.Fatalf("restored hidden PR = %q, want included", got)
	}
	apply(t, restarted, Action{Kind: "configure", Config: next})
	if got := restarted.Snapshot().Choices.PRs; !maps.Equal(got, map[release.Target]release.Choice{fact: release.Excluded}) {
		t.Fatalf("choices after switching back to %s = %v", next.Version, got)
	}
}

func TestPRStateSaveFailureLeavesBulkChoicesIntact(t *testing.T) {
	s, f := newFixture()
	c := configuration()
	path := filepath.Join(t.TempDir(), "state.json")
	state, err := OpenReleaseState(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.options.ReleaseState = state
	apply(t, s, Action{Kind: "configure", Config: c})
	repo := f.repos["fact"]
	for _, number := range []int{12, 13} {
		repo.PRs = append(repo.PRs, release.PR{
			Target: release.Target{Repo: repo.Source.Repo, PR: number}, State: "open", Base: repo.Source.Branch, Evidence: observed(),
		})
	}
	f.repos["fact"] = repo
	refreshAll(t, s)
	apply(t, s, Action{Kind: "component-pr-choices", Component: "fact", Choice: release.Included})
	before := s.Snapshot()
	state.path = t.TempDir()
	err = s.Apply(t.Context(), Action{
		Kind: "component-pr-choices", Component: "fact", Generation: before.Generation, Choice: release.Ignored,
	})
	if err == nil || !strings.Contains(err.Error(), "save release state") {
		t.Fatalf("bulk action returned %v", err)
	}
	after := s.Snapshot()
	if !maps.Equal(before.Choices.PRs, after.Choices.PRs) {
		t.Fatalf("failed bulk action changed choices from %v to %v", before.Choices.PRs, after.Choices.PRs)
	}
	disk, err := OpenReleaseState(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, pr := range repo.PRs {
		if got := disk.file.Releases[c.Version][pr.Target.String()]; got != release.Included {
			t.Fatalf("saved choice for %s = %q, want included", pr.Target, got)
		}
	}
}
