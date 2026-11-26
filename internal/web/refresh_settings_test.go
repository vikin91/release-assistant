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
	if !strings.Contains(body, `<table class="settings-table" aria-labelledby="refresh-settings-heading">`) {
		t.Fatal("refresh intervals must have a named table")
	}
	for _, heading := range []string{"Components", "Before tag selection", "After tag selection", "ACS"} {
		tag := "h4"
		if strings.HasSuffix(heading, "tag selection") {
			tag = "h5"
		}
		refresh := between(t, body, `id="refresh-settings"`, "</section>")
		if !strings.Contains(refresh, `colspan="2"><`+tag+">"+heading+"</"+tag+">") {
			t.Errorf("settings missing %q group", heading)
		}
	}
	values := url.Values{"kind": {"application-settings"}, "tab": {"settings"},
		"generation": {fmt.Sprint(session.Snapshot().Generation)}, "settings_section": {"refresh-settings"}}
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
	} else if got := response.Header().Get("Location"); got != "/?tab=app-settings&saved=refresh-settings#refresh-settings" {
		t.Errorf("refresh save redirected to %q", got)
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

func TestSettingsSectionsFollowNavigationAndSubmitIndependently(t *testing.T) {
	snapshot := app.Snapshot{
		Config: validACS5Config(), WritesEnabled: true, StartupReadLimit: 3, OngoingReadLimit: 1,
		RefreshIntervals: app.DefaultRefreshIntervals(0, 0),
	}
	body := newBrowser(t, &fakeApplication{snapshot: snapshot}).getBody(t)
	type section struct{ id, field string }
	pages := []struct {
		tab, title string
		sections   []section
	}{
		{"settings", "Release Settings", []section{{"configuration-panel", "version"}, {"retry-settings", "interactive"}}},
		{"app-settings", "App Settings", []section{{"github-read-settings", "startup_read_limit"}, {"tag-choice-settings", "tag_choice_limit"}, {"refresh-settings", "refresh_acs_tags"}}},
	}
	for _, page := range pages {
		panel := activityPanel(t, body, page.tab)
		if !strings.Contains(panel, `<h2 class="page-title">`+page.title+`</h2>`) {
			t.Fatalf("missing %s heading", page.title)
		}
		nav := between(t, panel, `aria-label="Settings sections"`, "</nav>")
		lastSection, lastLink := -1, -1
		for _, section := range page.sections {
			marker := `id="` + section.id + `"`
			position := strings.Index(panel, marker)
			link := strings.Index(nav, `href="#`+section.id+`"`)
			if position <= lastSection || link <= lastLink {
				t.Fatalf("%s is missing or out of order in %s", section.id, page.title)
			}
			lastSection, lastLink = position, link
			content := between(t, panel, marker, "</section>")
			if strings.Count(content, "<table ") != 1 {
				t.Errorf("%s must have its own table", section.id)
			}
			for _, otherPage := range pages {
				for _, other := range otherPage.sections {
					if hasField := strings.Contains(content, `name="`+other.field+`"`); hasField != (other.id == section.id) {
						t.Errorf("field %s present in %s = %t", other.field, section.id, hasField)
					}
					if otherPage.tab != page.tab && strings.Contains(panel, `id="`+other.id+`"`) {
						t.Errorf("%s appears in %s", other.id, page.title)
					}
				}
			}
		}
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
