package web

import (
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"release-assistant/internal/app"
	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

func TestTagChoiceLimitSettingsPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	openSession := func() *app.Session {
		t.Helper()
		state, err := app.OpenReleaseState(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		return app.New(nil, nil, nil, dispatch.New(nil, nil, nil, dispatch.Options{}), nil, app.Options{ReleaseState: state})
	}
	session := openSession()
	b := newBrowser(t, session)
	settings := between(t, b.getBody(t), `id="tag-choice-settings"`, "</section>")
	for _, field := range []string{`min="1" max="100" step="1" required value="5"`, `aria-describedby="tag-choice-help tag-choice-unit"`} {
		if !strings.Contains(settings, field) {
			t.Errorf("settings missing %q", field)
		}
	}
	form := url.Values{"kind": {"tag-choice-settings"}, "tab": {"settings"}, "generation": {fmt.Sprint(session.Snapshot().Generation)}, "tag_choice_limit": {"2"}}
	response := b.post(form)
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/?tab=settings&saved=tag-choice-settings#tag-choice-settings" {
		t.Fatalf("save status=%d redirect=%q", response.Code, response.Header().Get("Location"))
	}
	for name, invalid := range map[string]struct {
		values []string
		status int
	}{
		"missing":     {nil, http.StatusBadRequest},
		"below range": {[]string{"0"}, http.StatusConflict},
		"above range": {[]string{"101"}, http.StatusConflict},
		"fractional":  {[]string{"1.5"}, http.StatusBadRequest},
		"duplicate":   {[]string{"2", "3"}, http.StatusBadRequest},
	} {
		form["tag_choice_limit"] = invalid.values
		if response := b.post(form); response.Code != invalid.status {
			t.Errorf("%s value returned HTTP %d, want %d", name, response.Code, invalid.status)
		}
	}
	if got := session.Snapshot().TagChoiceLimit; got != 2 {
		t.Fatalf("invalid input changed limit to %d", got)
	}
	if got := openSession().Snapshot().TagChoiceLimit; got != 2 {
		t.Fatalf("reopened limit = %d, want 2", got)
	}
}

func TestComponentTagChoicesShowNewestVersionsWithinLimit(t *testing.T) {
	config := prefilledConfig("4.11.1")
	config.Branch = "release-4.11"
	observed := release.Evidence{Known: true, At: time.Now()}
	for name, tc := range map[string]struct {
		limit, selectedPatch int
		want                 []string
		hidden               string
	}{
		"default":                            {want: []string{"14", "13", "12", "11", "10"}, hidden: "2 older tags hidden."},
		"changed":                            {limit: 2, want: []string{"14", "13"}, hidden: "5 older tags hidden."},
		"old selection is listed separately": {limit: 2, selectedPatch: 1, want: []string{"14", "13"}, hidden: "4 older tags hidden."},
		"new selection does not count toward limit": {limit: 2, selectedPatch: 14, want: []string{"13", "12"}, hidden: "4 older tags hidden."},
		"all choices fit": {limit: 7, want: []string{"14", "13", "12", "11", "10", "9", "1"}},
	} {
		t.Run(name, func(t *testing.T) {
			snapshot := app.Snapshot{Config: config, TagChoiceLimit: tc.limit,
				Choices:    release.Choices{Tags: map[string]string{}},
				Assessment: release.Assessment{ProposedTags: map[string]string{}},
			}
			for _, source := range config.Components {
				repo := release.Repository{Source: source, Evidence: observed}
				for _, patch := range []int{9, 14, 11, 1, 10, 13, 12} {
					tag := fmt.Sprintf("%s.%d", source.Stream, patch)
					repo.Tags = append(repo.Tags, release.Tag{Name: tag, SHA: fmt.Sprintf("sha-%d", patch), Belongs: release.Yes, Evidence: observed})
					if patch == tc.selectedPatch {
						snapshot.Choices.Tags[source.Name] = tag
					}
				}
				snapshot.Assessment.ProposedTags[source.Name] = source.Stream + ".9"
				snapshot.Repositories = append(snapshot.Repositories, repo)
			}
			body := newBrowser(t, &fakeApplication{snapshot: snapshot}).getBody(t)
			for _, source := range config.Components {
				card := between(t, body, `id="component-tags-`+source.Name+`"`, "</article>")
				choices := between(t, card, `tag choices"`, "</table>")
				var patches []string
				pattern := regexp.MustCompile(`<code>` + regexp.QuoteMeta(source.Stream) + `\.(\d+)</code>`)
				for _, match := range pattern.FindAllStringSubmatch(choices, -1) {
					patches = append(patches, match[1])
				}
				if !slices.Equal(patches, tc.want) {
					t.Errorf("%s tag patches = %v, want %v", source.Name, patches, tc.want)
				}
				if (tc.hidden == "" && strings.Contains(card, "hidden. Increase")) || (tc.hidden != "" && !strings.Contains(card, tc.hidden)) {
					t.Errorf("%s hidden notice does not match %q", source.Name, tc.hidden)
				}
				if tc.selectedPatch != 0 {
					selected := between(t, card, `release tags"`, "</table>")
					if !strings.Contains(selected, fmt.Sprintf("<code>%s.%d</code>", source.Stream, tc.selectedPatch)) ||
						!strings.Contains(selected, "Clear selection") ||
						!strings.Contains(card, `<details class="tag-evidence" data-transient data-fold-key="tag-switch:`) {
						t.Errorf("%s selected tag or native disclosure is missing", source.Name)
					}
				}
			}
		})
	}
}
