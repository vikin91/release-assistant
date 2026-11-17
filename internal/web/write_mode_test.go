package web

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"release-assistant/internal/app"
	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

func TestWriteModeControls(t *testing.T) {
	prTarget := release.Target{Repo: "acme/rox", PR: 12}
	tagTarget := release.Target{Repo: "acme/fact", Tag: "3.25.7"}
	proposal := release.Proposal{Target: prTarget, SHA: "head-sha", Job: "unit", RunID: "run-77", Payload: "/konflux-retest unit"}

	for _, tc := range []struct {
		name          string
		writesEnabled bool
		optOut        bool
		wantReadOnly  bool
		wantDisabled  bool
	}{
		{name: "startup read-only", wantReadOnly: true, wantDisabled: true},
		{name: "writes enabled", writesEnabled: true},
		{name: "writes enabled with opt-out", writesEnabled: true, optOut: true, wantDisabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			application := &fakeApplication{snapshot: app.Snapshot{
				Config: validACS5Config(), Generation: 9, WritesEnabled: tc.writesEnabled,
				Choices: release.Choices{
					Tags: map[string]string{"fact": "3.25.7", "collector": "3.25.4"},
					PRs:  map[release.Target]release.Choice{prTarget: release.Included},
				},
				Repositories: []release.Repository{
					{Source: release.Source{Name: "acs", Repo: "acme/rox", Branch: "release-5.0"}, PRs: []release.PR{{Target: prTarget, SHA: "head-sha", Title: "Included PR", State: "open", OptOut: tc.optOut, Evidence: release.Evidence{Known: true, At: time.Now()}}}},
					{Source: release.Source{Name: "fact", Repo: "acme/fact", Branch: "release-3.25", Stream: "3.25"}, Tags: []release.Tag{
						{Name: "3.25.7", SHA: "tag-sha", Belongs: release.Yes, Evidence: release.Evidence{Known: true, At: time.Now()}},
						{Name: "3.25.8", SHA: "next-tag-sha", Belongs: release.Yes, Evidence: release.Evidence{Known: true, At: time.Now()}},
					}},
				},
				Plans: []release.Plan{{Target: prTarget, SHA: proposal.SHA, Proposals: []release.Proposal{proposal}}},
				Authority: []dispatch.View{
					{Target: prTarget, SHA: proposal.SHA, Granted: true, OptOut: tc.optOut, Offers: []dispatch.Offer{{ID: "offer-1", Proposal: proposal, Allowed: true, CanApprove: true}}},
					{Target: tagTarget, SHA: "tag-sha", Granted: true, OptOut: tc.optOut},
				},
			}}

			b := newBrowser(t, application)
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "http://127.0.0.1:8080/", nil)
			r.Host = "127.0.0.1:8080"
			b.handler.ServeHTTP(w, r)
			if w.Code != 200 {
				t.Fatalf("GET status=%d body=%s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			if got := strings.Contains(body, "Read-only mode. Restart with"); got != tc.wantReadOnly {
				t.Errorf("read-only notice present=%t, want %t", got, tc.wantReadOnly)
			}

			prCard := prCardContaining(body, "Included PR")
			if prCard == "" {
				t.Fatal("included PR selection card missing")
			}
			prGrant := grantForm(t, prCard)
			assertCheckboxDisabled(t, "PR RW grant", prGrant, "enabled", tc.wantDisabled)
			assertButtonTextDisabled(t, "PR noscript grant", prGrant, "Apply RW permission", tc.wantDisabled)
			if !strings.Contains(prCard, `value="included" aria-pressed="true"`) || strings.Contains(prCard, `name="choice" value="included" aria-pressed="true" disabled`) || !strings.Contains(prCard, "Excluded") || !strings.Contains(prCard, "Ignore &amp; hide") {
				t.Error("PR inclusion, exclusion, and ignore choices must remain available")
			}

			tagCard := regexpTagCard(t, body, "tag-sha")
			tagGrant := grantForm(t, tagCard)
			assertCheckboxDisabled(t, "tag RW grant", tagGrant, "enabled", tc.wantDisabled)
			assertButtonTextDisabled(t, "tag noscript grant", tagGrant, "Apply RW permission", tc.wantDisabled)
			if !strings.Contains(body, "Switch to fact 3.25.8") {
				t.Error("tag switch must remain available")
			}

			if !strings.Contains(body, "/konflux-retest unit") {
				t.Error("copyable retry comment must remain available")
			}
			modeForm := formContaining(t, body, "Save mode")
			assertCheckboxDisabled(t, "interactive confirmation", modeForm, "enabled", !tc.writesEnabled)
			assertButtonTextDisabled(t, "Save mode", modeForm, "Save mode", !tc.writesEnabled)
			if tc.writesEnabled && !strings.Contains(modeForm, `name="enabled" value="true"`) {
				t.Error("interactive confirmation should be available when writes are enabled")
			}

			sendForm := formContaining(t, body, "Send one comment")
			assertCheckboxDisabled(t, "send confirmation", sendForm, "confirm", !tc.writesEnabled)
			assertButtonDisabled(t, "send button", sendForm, !tc.writesEnabled)
			if !strings.Contains(sendForm, `name="confirm"`) {
				t.Error("inconsistent CanApprove offer should remain visible with disabled controls")
			}
		})
	}
}

func grantForm(t *testing.T, body string) string {
	t.Helper()
	for _, form := range renderedFormRE.FindAllString(body, -1) {
		if strings.Contains(form, `name="kind" value="grant"`) {
			return form
		}
	}
	t.Fatal("RW grant form missing")
	return ""
}

func formContaining(t *testing.T, body, text string) string {
	t.Helper()
	for _, form := range renderedFormRE.FindAllString(body, -1) {
		if strings.Contains(form, text) {
			return form
		}
	}
	t.Fatalf("form containing %q missing", text)
	return ""
}

func assertCheckboxDisabled(t *testing.T, name, form, field string, want bool) {
	t.Helper()
	input := regexp.MustCompile(`<input type="checkbox" name="` + regexp.QuoteMeta(field) + `"[^>]*>`).FindString(form)
	if input == "" {
		t.Errorf("%s checkbox missing: %s", name, form)
		return
	}
	if got := strings.Contains(input, " disabled"); got != want {
		t.Errorf("%s disabled=%t, want %t: %s", name, got, want, input)
	}
}

func assertButtonDisabled(t *testing.T, name, form string, want bool) {
	t.Helper()
	assertButtonTextDisabled(t, name, form, "Send one comment", want)
}

func assertButtonTextDisabled(t *testing.T, name, form, label string, want bool) {
	t.Helper()
	button := regexp.MustCompile(`<button type="submit"[^>]*>` + regexp.QuoteMeta(label) + `</button>`).FindString(form)
	if button == "" {
		t.Errorf("%s missing: %s", name, form)
		return
	}
	if got := strings.Contains(button, " disabled"); got != want {
		t.Errorf("%s disabled=%t, want %t: %s", name, got, want, button)
	}
}

func regexpTagCard(t *testing.T, body, marker string) string {
	t.Helper()
	for _, card := range strings.Split(body, `<article class="tag-card`) {
		if strings.Contains(card, marker) {
			return `<article class="tag-card` + card
		}
	}
	t.Fatalf("tag card containing %q missing", marker)
	return ""
}
