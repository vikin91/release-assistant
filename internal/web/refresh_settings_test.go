package web

import (
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"release-assistant/internal/app"
	"release-assistant/internal/dispatch"
)

func TestRefreshIntervalsSettings(t *testing.T) {
	session := app.New(nil, nil, nil, dispatch.New(nil, nil, nil, dispatch.Options{}), nil, app.Options{})
	b := newBrowser(t, session)
	body := b.getBody(t)
	if !strings.Contains(body, `<table class="settings-table" aria-label="Application settings">`) {
		t.Fatal("application settings must use a table")
	}
	for _, heading := range []string{"Components", "Before tag selection", "After tag selection", "ACS"} {
		if !strings.Contains(body, `colspan="2">`+heading+"<") {
			t.Errorf("settings missing %q group", heading)
		}
	}
	values := url.Values{"kind": {"application-settings"}, "tab": {"settings"}, "generation": {fmt.Sprint(session.Snapshot().Generation)}}
	expected := app.DefaultRefreshIntervals(0, 0)
	for key, duration := range expected {
		name := "refresh_" + string(key)
		want := fmt.Sprintf(`name="%s" type="number" min="1" step="1" required value="%d"`, name, duration/time.Minute)
		if !strings.Contains(body, want) {
			t.Errorf("default interval field missing %q", want)
		}
		values.Set(name, strconv.Itoa(int(duration/time.Minute)+1))
		expected[key] += time.Minute
	}
	if response := b.post(values); response.Code != http.StatusSeeOther {
		t.Fatalf("save settings: HTTP %d", response.Code)
	}
	if got := session.Snapshot().RefreshIntervals; !maps.Equal(got, expected) {
		t.Fatalf("intervals = %v, want %v", got, expected)
	}
	body = b.getBody(t)
	for key, duration := range expected {
		want := fmt.Sprintf(`name="refresh_%s" type="number" min="1" step="1" required value="%d"`, key, duration/time.Minute)
		if !strings.Contains(body, want) {
			t.Errorf("saved interval missing %q", want)
		}
	}

	for name, invalid := range map[string][]string{
		"zero": {"0"}, "negative": {"-3"}, "fractional": {"1.2"},
		"overflow": {"153722868"}, "duplicate": {"3", "4"},
	} {
		t.Run(name, func(t *testing.T) {
			values.Set("refresh_after_prs", "37")
			values["refresh_before_prs"] = invalid
			if response := b.post(values); response.Code != http.StatusBadRequest {
				t.Fatalf("invalid form returned HTTP %d", response.Code)
			}
			if got := session.Snapshot().RefreshIntervals; !maps.Equal(got, expected) {
				t.Fatalf("invalid form modified intervals: %v", got)
			}
		})
	}
}

func TestFractionalMinuteCLIOverrideSurvivesSettingsSave(t *testing.T) {
	session := app.New(nil, nil, nil, dispatch.New(nil, nil, nil, dispatch.Options{}), nil,
		app.Options{RefreshIntervals: app.DefaultRefreshIntervals(250*time.Millisecond, 0)})
	b := newBrowser(t, session)
	if body := b.getBody(t); !strings.Contains(body, `id="refresh-help-before_pr_checks">Refresh GitHub CI results for tracked component PRs. The current interval is 250ms.`) {
		t.Fatal("the field should show the precise CLI override until the user enters a whole number of minutes")
	}
	values := url.Values{"kind": {"application-settings"}, "generation": {fmt.Sprint(session.Snapshot().Generation)},
		"refresh_before_pr_checks": {""}, "refresh_acs_tags": {"23"}}
	if response := b.post(values); response.Code != http.StatusSeeOther {
		t.Fatalf("save settings with a CLI override: HTTP %d", response.Code)
	}
	got := session.Snapshot().RefreshIntervals
	if got[app.BeforePRChecks] != 250*time.Millisecond || got[app.ACSTags] != 23*time.Minute {
		t.Fatalf("unexpected saved intervals: %v", got)
	}
}
