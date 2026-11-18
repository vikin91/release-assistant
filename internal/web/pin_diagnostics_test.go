package web

import (
	"strings"
	"testing"
	"time"

	"release-assistant/internal/app"
	"release-assistant/internal/release"
)

func TestPinningDetailsShowConcreteCausesAndPendingStates(t *testing.T) {
	for name, cause := range map[string]string{"authentication": "Authentication failed (HTTP 401).", "not found": "Not found or inaccessible (HTTP 404).", "rate limit": "GitHub rate limit reached (HTTP 429).", "timeout": "Request timed out.", "pending": ""} {
		t.Run(name, func(t *testing.T) {
			config := validACS5Config()
			config.PinningPR = 123
			evidence := release.Evidence{}
			summary := "Pending: reading pinning PR #123"
			var problems []string
			if cause != "" {
				evidence = release.Evidence{At: time.Now(), Error: cause}
				summary = "Unable to get PR status and component versions from pinning PR #123"
				problems = []string{cause}
			}
			body := newBrowser(t, &fakeApplication{snapshot: app.Snapshot{Config: config, Repositories: []release.Repository{{Source: config.ACS(), PinningEvidence: evidence, PinningPins: release.Pins{Evidence: evidence}}}, Assessment: release.Assessment{Pins: release.PinSummary{PRNumber: 123, PRURL: "https://github.com/stackrox/stackrox/pull/123", PRSummary: summary, PRProblems: problems}}}}).getBody(t)
			section := pinningSection(t, body)
			if !strings.Contains(section, summary) || (cause != "" && !strings.Contains(section, cause)) {
				t.Fatal("concrete failure or pending state missing", section)
			}
			if cause != "" && !strings.Contains(section, `class="pin-problems"`) {
				t.Fatal("causes are not listed")
			}
			for _, forbidden := range []string{"not been compared", "incomplete proposed versions", "not checked", "Green pin status only means"} {
				if strings.Contains(section, forbidden) {
					t.Errorf("vague pin status %q", forbidden)
				}
			}
		})
	}
}

func TestPinComparisonHighlightsOnlyVerifiedDifferences(t *testing.T) {
	config := validACS5Config()
	config.PinningPR = 123
	observed := release.Evidence{Known: true, At: time.Now()}
	body := newBrowser(t, &fakeApplication{snapshot: app.Snapshot{
		Config:  config,
		Choices: release.Choices{Tags: map[string]string{"fact": "v3.25.1", "collector": "3.25.4"}},
		Repositories: []release.Repository{{
			Source:      config.ACS(),
			Pins:        release.Pins{Versions: map[string]string{"fact": "3.25.1", "collector": "3.25.3"}, Evidence: observed},
			PinningPins: release.Pins{Versions: map[string]string{"collector": "3.25.2"}},
		}},
	}}).getBody(t)
	table := between(t, pinningSection(t, body), `<table class="pin-comparison"`, "</table>")
	if strings.Count(table, `class="pin-differs"`) != 1 ||
		!strings.Contains(table, `class="pin-differs"><code>3.25.3</code>`) ||
		!strings.Contains(table, "3.25.2 (last observed)") {
		t.Fatalf("incorrect pin comparison: %s", table)
	}
}
