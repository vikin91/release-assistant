package app

import (
	"path/filepath"
	"testing"

	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

func TestPinningChoicePersistsAndRequiresFreshBranchEvidence(t *testing.T) {
	_, f, config := newPinningSession(t)
	target := config.PinningTarget()
	config.PinningPR = 0
	path := filepath.Join(t.TempDir(), "state.json")
	start := func(initial release.Config, override bool) *Session {
		t.Helper()
		state, err := OpenReleaseState(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		return New(f, f, f, dispatch.New(f, f, f, dispatch.Options{EnableWrites: true}), nil,
			Options{Initial: initial, ReleaseState: state, PinningPROverride: override})
	}
	original := start(config, false)
	refreshAll(t, original)
	apply(t, original, Action{Kind: "choice", Target: target, Choice: release.Excluded})
	apply(t, original, Action{Kind: "use-pinning-pr", Target: target, SHA: "pin-head"})

	second := config
	second.Version = "5.0.2"
	apply(t, original, Action{Kind: "configure", Config: second})
	if got := original.Snapshot(); got.Config.PinningPR != 0 || got.PendingPinningPR != 0 {
		t.Fatal("a different patch inherited the pinning choice")
	}
	config.Version = "v" + config.Version
	apply(t, original, Action{Kind: "configure", Config: config})
	if got := original.Snapshot(); got.PendingPinningPR != target.PR {
		t.Fatal("switching back did not restore the saved choice")
	}

	restarted := start(config, false)
	if got := restarted.Snapshot(); got.PendingPinningPR != target.PR ||
		got.Config.PinningPR != 0 || got.Choices.PRs[target] != release.Excluded {
		t.Fatalf("unverified state applied the pinning role: pending=%d config=%d choices=%v",
			got.PendingPinningPR, got.Config.PinningPR, got.Choices.PRs)
	}
	unchangedForm := config
	unchangedForm.PinningPR = target.PR
	apply(t, restarted, Action{Kind: "configure", Config: unchangedForm})
	if got := restarted.Snapshot(); got.Config.PinningPR != 0 || got.PendingPinningPR != target.PR {
		t.Fatal("saving unchanged settings bypassed pinning verification")
	}
	otherBranch := config
	otherBranch.Branch = "release-other"
	if got := start(otherBranch, false).Snapshot(); got.PendingPinningPR != 0 {
		t.Fatal("the saved choice was applied to a different branch configuration")
	}
	if got := start(config, true).Snapshot(); got.PendingPinningPR != 0 || got.Config.PinningPR != 0 {
		t.Fatal("the saved choice overrode an explicit request for discovery")
	}

	acs := f.repos["acs"]
	pr := *acs.PinningPR
	for _, invalid := range []release.PR{
		{Target: pr.Target, SHA: pr.SHA, Base: "master", State: "open", Evidence: observed()},
		{Target: pr.Target, SHA: pr.SHA, Base: pr.Base, State: "open"},
	} {
		acs.PinningPR = &invalid
		f.repos["acs"] = acs
		refreshAll(t, restarted)
		if got := restarted.Snapshot(); got.Config.PinningPR != 0 || got.PendingPinningPR != target.PR {
			t.Fatal("invalid or stale evidence activated the saved PR")
		}
	}
	pr.SHA = "updated-head"
	acs.PinningPR = &pr
	acs.PRs = []release.PR{pr}
	f.repos["acs"] = acs
	refreshAll(t, restarted)
	if got := restarted.Snapshot(); got.PendingPinningPR != 0 ||
		got.Config.PinningPR != target.PR || got.Choices.PRs[target] != release.Included {
		t.Fatalf("fresh branch evidence did not restore pinning choice: pending=%d config=%d choices=%v",
			got.PendingPinningPR, got.Config.PinningPR, got.Choices.PRs)
	}
	for _, view := range restarted.Snapshot().Authority {
		if view.Granted {
			t.Fatal("restoring a pinning choice granted write permission")
		}
	}

	apply(t, restarted, Action{Kind: "configure", Config: config})
	if got := start(config, false).Snapshot(); got.PendingPinningPR != 0 || got.Choices.PRs[target] != release.Excluded {
		t.Fatal("clearing the pinning choice did not preserve discovery and the ordinary PR choice")
	}

	config.DetectPinningPR, config.ManualPinningPR = true, target.PR
	apply(t, restarted, Action{Kind: "configure", Config: config})
	config.DetectPinningPR, config.ManualPinningPR = false, 0
	if got := start(config, false).Snapshot(); !got.Config.DetectPinningPR || got.Config.ManualPinningPR != target.PR ||
		got.Config.PinningPR != 0 || got.PendingPinningPR != 0 {
		t.Fatal("discovery mode lost its inactive PR number")
	}
}

func TestPinningChoiceWriteFailureLeavesActivePRInPlace(t *testing.T) {
	s, _, config := newPinningSession(t)
	state, err := OpenReleaseState(filepath.Join(t.TempDir(), "state.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	s.options.ReleaseState = state
	state.path = t.TempDir()
	cleared := config
	cleared.PinningPR = 0
	if err := s.Apply(t.Context(), Action{Kind: "configure", Config: cleared}); err == nil {
		t.Fatal("clearing a pinning choice succeeded despite a failed save")
	}
	if got := s.Snapshot(); got.Config.PinningPR != config.PinningPR || got.Choices.PRs[config.PinningTarget()] != release.Included {
		t.Fatal("failed save changed the active pinning PR")
	}
}
