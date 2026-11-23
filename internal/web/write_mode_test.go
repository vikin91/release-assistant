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
		interactive   bool
		optOut        bool
		requireLabel  bool
		missingLabel  bool
		wantDisabled  bool

		wantHeader string
	}{
		{name: "startup read-only", interactive: true, wantHeader: "Read-only"},
		{name: "writes enabled", writesEnabled: true, interactive: true, wantHeader: "Interactive"},
		{name: "automatic with opt-out", writesEnabled: true, optOut: true, wantDisabled: true, wantHeader: "Full auto"},
		{name: "only labeled", writesEnabled: true, requireLabel: true, wantHeader: "Only labeled"},
		{name: "unlabeled PR", writesEnabled: true, requireLabel: true, missingLabel: true, wantHeader: "Only labeled"},
		{name: "interactive and labeled", writesEnabled: true, interactive: true, requireLabel: true, wantHeader: "Interactive · Only labeled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			application := &fakeApplication{snapshot: app.Snapshot{
				Config: validACS5Config(), Generation: 9, WritesEnabled: tc.writesEnabled, Interactive: tc.interactive, RequirePRLabel: tc.requireLabel,
				Choices: release.Choices{
					Tags: map[string]string{"fact": "3.25.7", "collector": "3.25.4"},
					PRs:  map[release.Target]release.Choice{prTarget: release.Included},
				},
				Repositories: []release.Repository{
					{Source: release.Source{Name: "acs", Repo: "acme/rox", Branch: "release-5.0"}, PRs: []release.PR{{Target: prTarget, SHA: "head-sha", Title: "Included PR", State: "open", Base: "release-5.0", OptOut: tc.optOut, CommentLabel: !tc.missingLabel, Evidence: release.Evidence{Known: true, At: time.Now()}}}},
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
			r := httptest.NewRequestWithContext(t.Context(), "GET", "http://127.0.0.1:8080/", nil)
			r.Host = "127.0.0.1:8080"
			b.handler.ServeHTTP(w, r)
			if w.Code != 200 {
				t.Fatalf("GET status=%d body=%s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			header := between(t, body, "<header", "</header>")
			if !strings.Contains(header, "<summary>Write mode · "+tc.wantHeader+"</summary>") {
				t.Errorf("header must display write mode %q", tc.wantHeader)
			}
			for _, mode := range []string{"Read-only", "Interactive", "Only labeled", "Full auto"} {
				if !strings.Contains(header, "<strong>"+mode+"</strong>") {
					t.Errorf("missing explanation of %s mode", mode)
				}
			}

			prCard := prCardContaining(body, "Included PR")
			if prCard == "" {
				t.Fatal("included PR selection card missing")
			}
			if got := strings.Contains(prCard, "Allowed by label"); got != (tc.requireLabel && !tc.missingLabel) {
				t.Errorf("label badge present = %t", got)
			}
			if !strings.Contains(prCard, `value="included" aria-pressed="true"`) || strings.Contains(prCard, `name="choice" value="included" aria-pressed="true" disabled`) || !strings.Contains(prCard, "Exclude</button>") || !strings.Contains(prCard, "Ignore &amp; hide") {
				t.Error("PR inclusion, exclusion, and ignore choices must remain available")
			}

			tagRow := tagRowContaining(body, "tag-sha")
			for target, markup := range map[string]string{"PR": prCard, "tag": tagRow} {
				if tc.writesEnabled && !tc.interactive {
					grant := grantForm(t, markup)
					assertCheckboxDisabled(t, target+" automatic grant", grant, "enabled", tc.wantDisabled)
					assertButtonTextDisabled(t, target+" noscript grant", grant, "Apply automatic posting permission", tc.wantDisabled)
				} else if strings.Contains(markup, `name="kind" value="grant"`) {
					t.Errorf("%s has automatic posting controls in %s mode", target, tc.wantHeader)
				}
			}
			if !strings.Contains(body, "Switch to fact 3.25.8") {
				t.Error("tag switch must remain available")
			}

			if !strings.Contains(body, "/konflux-retest unit") {
				t.Error("copyable retry comment must remain available")
			}
			modeForm := formContaining(t, body, `value="application-settings"`)
			assertCheckboxDisabled(t, "interactive confirmation", modeForm, "interactive", !tc.writesEnabled)
			assertCheckboxDisabled(t, "PR label requirement", modeForm, "require_pr_label", !tc.writesEnabled)
			assertButtonTextDisabled(t, "save settings", modeForm, "Save settings", !tc.writesEnabled)
			if tc.writesEnabled && !strings.Contains(modeForm, `name="interactive" value="true"`) {
				t.Error("interactive confirmation should be available when writes are enabled")
			}

			if strings.Contains(body, `name="confirm"`) || strings.Contains(body, `name="kind" value="send"`) {
				t.Error("the dashboard rendered a confirmation form outside the queue")
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
