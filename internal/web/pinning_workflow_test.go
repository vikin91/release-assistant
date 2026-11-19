package web

import (
	"strings"
	"testing"
	"time"

	"release-assistant/internal/app"
	"release-assistant/internal/release"
)

func TestPinningWorkflowPrioritizesOpenPRVersions(t *testing.T) {
	for name, tc := range map[string]struct {
		factPin string
		merged  bool
	}{
		"should show matching PR versions as pending merge": {factPin: "0.2.18"},
		"should highlight only a mismatched PR version":     {factPin: "0.2.17"},
		"should check actual branch pins after merge":       {factPin: "0.2.18", merged: true},
	} {
		t.Run(name, func(t *testing.T) {
			config := prefilledConfig("4.10.12")
			config.Branch, config.PinningPR = "release-4.10", 23958
			observed := release.Evidence{Known: true, At: time.Now()}
			pr := release.PR{Target: config.PinningTarget(), State: "open", Base: config.Branch, Evidence: observed}
			if tc.merged {
				pr.State, pr.Merged = "closed", true
			}
			snapshot := app.Snapshot{
				Config: config, Choices: release.Choices{Tags: map[string]string{"fact": "0.2.18", "collector": "3.24.17", "scanner": "2.39.28"}},
				Repositories: []release.Repository{{
					Source: config.ACS(), Head: "acs-head", Evidence: observed, PinningPR: &pr,
					Pins:        release.Pins{Versions: map[string]string{"fact": "0.2.14", "collector": "3.24.15", "scanner": "2.39.24"}, Evidence: observed},
					PinningPins: release.Pins{Versions: map[string]string{"fact": tc.factPin, "collector": "3.24.17", "scanner": "2.39.28"}, Evidence: observed},
				}},
			}
			snapshot.Assessment = release.Assess(release.Input{Config: config, Choices: snapshot.Choices, Repositories: snapshot.Repositories})
			body := newBrowser(t, &fakeApplication{snapshot: snapshot}).getBody(t)
			section := pinningSection(t, body)
			if tc.merged {
				if !strings.Contains(section, `class="pill no">ACS pins differ from selected versions</span>`) || strings.Count(section, `class="pin-differs"`) != 3 {
					t.Fatal("merged PR did not restore branch pin verification", section)
				}
				return
			}
			for _, unwanted := range []string{"Actual ACS branch pins differ", "fact actual", "collector actual", "scanner actual", "ACS pins: versions differ"} {
				if strings.Contains(body, unwanted) {
					t.Errorf("open PR presents expected branch differences as a problem: %q", unwanted)
				}
			}
			if !strings.Contains(body, `class="pill unknown" href="/?tab=acs-prs#pinning-details"`) || !strings.Contains(body, "ACS pins: pending PR merge") {
				t.Error("branch pill does not explain the pending merge")
			}
			if !strings.Contains(section, "Release-branch pins will be verified after merge.") {
				t.Error("branch verification phase is not explained")
			}
			wantDifferences := 0
			if tc.factPin != "0.2.18" {
				wantDifferences = 1
				want := "The pinning PR sets FACT_VERSION to 0.2.17; your selected Fact version is 0.2.18."
				if !strings.Contains(section, want) || !strings.Contains(body, "Pin PR #23958: Needs update") {
					t.Error("PR mismatch is not explained with the file and exact versions", section)
				}
			}
			if got := strings.Count(section, `class="pin-differs"`); got != wantDifferences {
				t.Errorf("highlighted differences = %d, want %d", got, wantDifferences)
			}
			if strings.Index(section, snapshot.Assessment.Pins.PRSummary) > strings.Index(section, `<table class="pin-comparison"`) {
				t.Error("pinning PR assessment is buried below branch comparisons")
			}
		})
	}
}
