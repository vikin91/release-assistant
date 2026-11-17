package app

import (
	"fmt"
	"strings"
	"testing"

	"release-assistant/internal/release"
)

func mergedPinFixture(t *testing.T, scanner bool) (*Session, *fixture, release.Config) {
	t.Helper()
	s, f := newFixture()
	config := configuration()
	config.PinningPR = 123
	if scanner {
		config.Version = "4.11.1"
		config.Branch = "release-4.11"
		source := release.Source{Name: "scanner", Repo: "stackrox/scanner", Branch: "release-2.40", Stream: "2.40"}
		config.Components = append(config.Components, source)
		f.repos["scanner"] = release.Repository{Source: source, Head: "scanner-head", Evidence: observed(), Tags: []release.Tag{{Name: "v2.40.1", SHA: "scanner-tag", Belongs: release.Yes, Evidence: observed()}}}
	}
	acs := f.repos["acs"]
	acs.Source = config.ACS()
	acs.PinningPR = &release.PR{Target: release.Target{Repo: config.ACS().Repo, PR: 123}, Base: config.Branch, State: "closed", Merged: true, SHA: "pr-head", MergeSHA: "merged-pin", Evidence: observed()}
	acs.PinningPins = release.Pins{Versions: map[string]string{"fact": "0.3.77", "collector": "v3.25.8", "scanner": "2.40.1"}, Evidence: observed()}
	f.repos["acs"] = acs
	apply(t, s, Action{Kind: "configure", Config: config})
	return s, f, config
}

func TestMergedPinningPRInitializesTagsInAnyDiscoveryOrder(t *testing.T) {
	for name, acsFirst := range map[string]bool{"ACS first": true, "components first": false} {
		t.Run(name, func(t *testing.T) {
			s, _, config := mergedPinFixture(t, true)
			sources := append([]release.Source{}, config.Components...)
			if acsFirst {
				sources = append([]release.Source{config.ACS()}, sources...)
			} else {
				sources = append(sources, config.ACS())
			}
			for _, source := range sources {
				if err := s.Refresh(t.Context(), s.Snapshot().Generation, source, true); err != nil {
					t.Fatal(err)
				}
			}
			snapshot := s.Snapshot()
			for component, want := range map[string]string{"fact": "0.3.77", "collector": "3.25.8", "scanner": "v2.40.1"} {
				if snapshot.Choices.Tags[component] != want {
					t.Errorf("%s selected=%q, want %q", component, snapshot.Choices.Tags[component], want)
				}
			}
			if snapshot.Assessment.Ready {
				t.Fatal("defaults were confused with current branch pins")
			}
			if !snapshot.Interactive {
				t.Fatal("default tag choice disabled interactive mode")
			}
			for _, authority := range snapshot.Authority {
				if authority.Granted {
					t.Fatal("default tag choice enabled write permission")
				}
			}
		})
	}
}

func TestMergedPinDefaultsPreserveUserChangesAndInvalidatedChoices(t *testing.T) {
	s, f, config := mergedPinFixture(t, false)
	fact := f.repos["fact"]
	fact.Tags = append(fact.Tags, release.Tag{Name: "0.3.78", SHA: "newer", Belongs: release.Yes, Evidence: observed()})
	f.repos["fact"] = fact
	refreshAll(t, s)
	if s.Snapshot().Choices.Tags["fact"] != "0.3.77" {
		t.Fatal("default did not use merged PR version")
	}
	apply(t, s, Action{Kind: "tag", Component: "fact", Tag: "0.3.78", SHA: "newer"})
	refreshAll(t, s)
	if s.Snapshot().Choices.Tags["fact"] != "0.3.78" {
		t.Fatal("poll replaced the user's chosen tag")
	}
	fact.Tags[1].SHA = "moved"
	f.repos["fact"] = fact
	if err := s.Refresh(t.Context(), s.Snapshot().Generation, config.Components[0], true); err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().Choices.Tags["fact"] != "" {
		t.Fatal("an invalidated user choice was silently restored from defaults")
	}
}

func TestPinningPRDefaultsRequireMergedVerifiedMatchingEvidence(t *testing.T) {
	for name, mutate := range map[string]func(*fixture){
		"open PR": func(f *fixture) {
			r := f.repos["acs"]
			r.PinningPR.Merged = false
			r.PinningPR.State = "open"
			f.repos["acs"] = r
		},
		"closed unmerged PR":    func(f *fixture) { r := f.repos["acs"]; r.PinningPR.Merged = false; f.repos["acs"] = r },
		"wrong PR":              func(f *fixture) { r := f.repos["acs"]; r.PinningPR.Target.PR++; f.repos["acs"] = r },
		"wrong base":            func(f *fixture) { r := f.repos["acs"]; r.PinningPR.Base = "master"; f.repos["acs"] = r },
		"wrong repository":      func(f *fixture) { r := f.repos["acs"]; r.PinningPR.Target.Repo = "acme/elsewhere"; f.repos["acs"] = r },
		"unknown merge commit":  func(f *fixture) { r := f.repos["acs"]; r.PinningPR.MergeSHA = ""; f.repos["acs"] = r },
		"PR observation failed": func(f *fixture) { r := f.repos["acs"]; r.PinningPR.Evidence.Known = false; f.repos["acs"] = r },
		"pin read failed":       func(f *fixture) { r := f.repos["acs"]; r.PinningPins.Evidence.Known = false; f.repos["acs"] = r },
	} {
		t.Run(name, func(t *testing.T) {
			s, f, _ := mergedPinFixture(t, false)
			mutate(f)
			refreshAll(t, s)
			if len(s.Snapshot().Choices.Tags) != 0 {
				t.Fatal("unverified or unrelated PR initialized tags")
			}
		})
	}
}

func TestMergedPinDefaultsPreservePartialTagUncertainty(t *testing.T) {
	for name, mutate := range map[string]func(*fixture){
		"outside stream": func(f *fixture) { r := f.repos["acs"]; r.PinningPins.Versions["fact"] = "0.4.7"; f.repos["acs"] = r },
		"nonfinal version": func(f *fixture) {
			r := f.repos["acs"]
			r.PinningPins.Versions["fact"] = "0.3.77-rc.1"
			f.repos["acs"] = r
		},
		"missing tag":          func(f *fixture) { r := f.repos["fact"]; r.Tags = nil; f.repos["fact"] = r },
		"wrong branch":         func(f *fixture) { r := f.repos["fact"]; r.Tags[0].Belongs = release.No; f.repos["fact"] = r },
		"unknown tag identity": func(f *fixture) { r := f.repos["fact"]; r.Tags[0].SHA = ""; f.repos["fact"] = r },
		"stale tag":            func(f *fixture) { r := f.repos["fact"]; r.Tags[0].Evidence.Known = false; f.repos["fact"] = r },
	} {
		t.Run(name, func(t *testing.T) {
			s, f, _ := mergedPinFixture(t, false)
			mutate(f)
			refreshAll(t, s)
			snapshot := s.Snapshot()
			if snapshot.Choices.Tags["fact"] != "" || snapshot.Choices.Tags["collector"] != "3.25.8" {
				t.Fatalf("partial defaults were not preserved: %+v", snapshot.Choices.Tags)
			}
		})
	}
}

func TestMergedPinDefaultsDoNotReplaceEarlierManualSelection(t *testing.T) {
	s, f, config := mergedPinFixture(t, false)
	fact := f.repos["fact"]
	fact.Tags = append(fact.Tags, release.Tag{Name: "0.3.78", SHA: "manual", Belongs: release.Yes, Evidence: observed()})
	f.repos["fact"] = fact
	for _, source := range config.Components {
		if err := s.Refresh(t.Context(), s.Snapshot().Generation, source, true); err != nil {
			t.Fatal(err)
		}
	}
	apply(t, s, Action{Kind: "tag", Component: "fact", Tag: "0.3.78", SHA: "manual"})
	if err := s.Refresh(t.Context(), s.Snapshot().Generation, config.ACS(), true); err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().Choices.Tags["fact"] != "0.3.78" {
		t.Fatal("merged PR replaced an existing choice")
	}
	entries := s.Snapshot().LogEntries
	defaults := 0
	for _, entry := range entries {
		if entry.Decision == "tag-from-pinning-pr" {
			defaults++
			if !strings.Contains(entry.Result, fmt.Sprint(config.PinningPR)) {
				t.Fatal("tag default lacks PR provenance")
			}
		}
	}
	if defaults != 1 {
		t.Fatalf("got %d defaults, want only the untouched collector", defaults)
	}
}
