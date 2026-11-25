package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

type createPinningFixture struct {
	*fixture
	live   release.PinDiscovery
	writes []release.PinningRequest
}

func (f *createPinningFixture) ReadPinningDiffs(context.Context, release.PinningRequest, map[int]release.PinFileMatch) (release.PinDiscovery, error) {
	return f.live, nil
}

func (f *createPinningFixture) OpenPinningPR(_ context.Context, req release.PinningRequest) (int, error) {
	f.writes = append(f.writes, req)
	return 42, nil
}

func TestCreatePinningPR(t *testing.T) {
	for name, tc := range map[string]struct {
		readOnly, scanner, noChange, pending, stale, duplicateAtClick bool
		wantBlock                                                     string
	}{
		"opens a 5.0 PR and restores the receipt":   {},
		"opens a 4.11 PR with a scanner change":     {scanner: true},
		"blocks writes in read-only mode":           {readOnly: true, wantBlock: "--enable-writes"},
		"blocks PRs without changes":                {noChange: true, wantBlock: "already match"},
		"blocks unverified images":                  {pending: true, wantBlock: "Waiting for Fact"},
		"rejects a stale preview":                   {stale: true, wantBlock: "proposed pins changed"},
		"rejects a duplicate found during dispatch": {duplicateAtClick: true, wantBlock: "already proposes"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := configuration()
			_, fixture := newFixture()
			f := &createPinningFixture{fixture: fixture, live: release.PinDiscovery{Evidence: observed()}}
			if tc.scanner {
				cfg.Version, cfg.Branch = "4.11.1", "release-4.11"
				cfg.Components = append(cfg.Components, release.Source{Name: "scanner", Repo: "stackrox/scanner", Branch: "release-2.40", Stream: "2.40"})
			}
			statePath := filepath.Join(t.TempDir(), "state.json")
			open := func() *ReleaseState {
				t.Helper()
				state, err := OpenReleaseState(statePath, nil)
				if err != nil {
					t.Fatal(err)
				}
				return state
			}
			state := open()
			if err := state.SetInitialRelease(cfg, false); err != nil {
				t.Fatal(err)
			}
			makeSession := func(state *ReleaseState) *Session {
				dispatcher := dispatch.New(f, f, f, dispatch.Options{EnableWrites: !tc.readOnly, PinningWriter: f})
				return New(f, f, f, dispatcher, nil, Options{Profiles: true, ReleaseState: state})
			}
			readChoices := func() []byte {
				t.Helper()
				data, err := os.ReadFile(statePath)
				if err != nil {
					t.Fatal(err)
				}
				return data
			}
			choices := readChoices()
			s := makeSession(state)
			acs := f.repos["acs"]
			acs.Source = cfg.ACS()
			acs.Pins.Contents = map[string]string{"fact": "0.3.76\n", "collector": "3.25.7\n"}
			acs.PinDiscovery.Evidence = observed()
			s.choices.Tags = map[string]string{"fact": "0.3.77", "collector": "3.25.7"}
			if tc.noChange {
				s.choices.Tags["fact"] = "0.3.76"
			}
			if tc.scanner {
				s.choices.Tags["scanner"] = "2.40.2"
				acs.Pins.Contents["scanner"], acs.Pins.Versions["scanner"] = "2.40.1\n", "2.40.1"
			}
			s.repos["acs"] = acs
			s.inventory = release.Inventory{ReleaseVersion: cfg.Version, Versions: s.choices.Tags, Evidence: observed()}
			for component := range s.choices.Tags {
				for _, family := range []string{"ci", "konflux"} {
					if tc.pending && component == "fact" && family == "konflux" {
						continue
					}
					s.inventory.Images = append(s.inventory.Images, release.Image{
						Component: component, Family: family, Reference: "quay.io/fixture/image:tag", Present: release.Yes, Evidence: observed(),
					})
				}
			}
			p := s.Snapshot().PinningPreview
			if tc.readOnly || tc.noChange || tc.pending {
				if !strings.Contains(strings.Join(p.Reasons, " "), tc.wantBlock) {
					t.Fatalf("blockers = %v", p.Reasons)
				}
			} else if len(p.Reasons) > 0 || p.Duplicate != 0 {
				t.Fatalf("unexpected blockers: %+v", p)
			}
			if tc.duplicateAtClick {
				f.live.Evidence = observed()
				f.live.Files = map[int]release.PinFileMatch{9: {Diff: release.PinSignature(p.Request.Changes)}}
			}
			if tc.stale {
				p.ID = "earlier-preview"
			}
			err := s.Apply(t.Context(), Action{Kind: "create-pinning-pr", ID: p.ID, Generation: s.generation, ProfileID: s.activeProfile})
			if tc.wantBlock != "" {
				if err == nil || len(f.writes) != 0 {
					t.Fatalf("err=%v writes=%v", err, f.writes)
				}
				if tc.duplicateAtClick && s.Snapshot().PinningPreview.Duplicate != 9 {
					t.Fatal("fresh duplicate missing from the page")
				}
				if tc.readOnly {
					ctx := s.dispatcher.WithScope(t.Context())
					if _, _, err := s.dispatcher.CreatePinningPR(ctx, p.Request, nil, func() bool { return true }); err == nil || len(f.writes) != 0 {
						t.Fatal("read-only dispatch admitted the writer")
					}
				}
				return
			}
			if err != nil || len(f.writes) != 1 {
				t.Fatalf("err=%v writes=%v", err, f.writes)
			}
			want := []release.PinChange{{Path: "FACT_VERSION", Before: "0.3.76\n", After: "0.3.77\n"}}
			if tc.scanner {
				want = append(want, release.PinChange{Path: "SCANNER_VERSION", Before: "2.40.1\n", After: "2.40.2\n"})
			}
			if !slices.Equal(f.writes[0].Changes, want) {
				t.Fatalf("wrong files: %+v", f.writes[0].Changes)
			}
			if _, _, err := s.dispatcher.CreatePinningPR(t.Context(), p.Request, nil, func() bool { return true }); err == nil || len(f.writes) != 1 {
				t.Fatal("unscoped dispatch admitted the writer")
			}
			s.flushDashboards()
			if !bytes.Equal(choices, readChoices()) {
				t.Fatal("creation changed state.json")
			}
			restored := makeSession(open()).Snapshot()
			if restored.CreatedPinningPR.Number != 42 || restored.CreatedPinningPR.Branch != cfg.Branch ||
				!slices.Equal(restored.CreatedPinningPR.Changes, want) || restored.Config.PinningPR != 0 || len(restored.PinningPreview.Reasons) == 0 {
				t.Fatalf("restored receipt or authorization was wrong: %+v", restored.CreatedPinningPR)
			}
		})
	}
}
