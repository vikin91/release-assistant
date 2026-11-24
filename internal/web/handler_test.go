package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"release-assistant/internal/app"
	"release-assistant/internal/dispatch"
	"release-assistant/internal/github"
	"release-assistant/internal/githubread"
	"release-assistant/internal/release"
)

type fakeApplication struct {
	snapshot app.Snapshot
	actions  []app.Action
	err      error
}

func (f *fakeApplication) Snapshot() app.Snapshot { return f.snapshot }
func (f *fakeApplication) Apply(_ context.Context, action app.Action) error {
	f.actions = append(f.actions, action)
	return f.err
}

type browser struct {
	t            *testing.T
	handler      http.Handler
	csrf, cookie string
}

func validACS5Config() release.Config {
	return release.Config{Version: "5.0.1", Branch: "release-5.0", Components: []release.Source{
		{Name: "fact", Repo: "acme/fact", Branch: "release-3.25", Stream: "3.25"},
		{Name: "collector", Repo: "acme/collector", Branch: "release-3.25", Stream: "3.25"},
	}}
}

// prefilledConfig mirrors CLI startup for a version with known component streams.
func prefilledConfig(version string) release.Config {
	config := release.Config{Version: version}
	for _, source := range release.DefaultsFor(version) {
		source.Branch = "release-" + source.Stream
		config.Components = append(config.Components, source)
	}
	return config
}

func newBrowser(t *testing.T, application Application) browser {
	t.Helper()
	handler, err := New(application)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:8080/", nil)
	r.Host = "127.0.0.1:8080"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("GET did not issue CSRF cookie")
	}
	name, _, ok := strings.Cut(w.Body.String(), `name="csrf" value="`)
	_ = name
	if !ok {
		t.Fatal("CSRF input missing")
	}
	part := strings.SplitN(w.Body.String(), `name="csrf" value="`, 2)[1]
	csrf := strings.SplitN(part, "\"", 2)[0]
	return browser{t: t, handler: handler, csrf: csrf, cookie: cookies[0].Name + "=" + cookies[0].Value}
}

func (b browser) post(values url.Values) *httptest.ResponseRecorder {
	values.Set("csrf", b.csrf)
	r := httptest.NewRequestWithContext(b.t.Context(), http.MethodPost, "http://127.0.0.1:8080/action", strings.NewReader(values.Encode()))
	r.Host = "127.0.0.1:8080"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://127.0.0.1:8080")
	r.Header.Set("Cookie", b.cookie)
	w := httptest.NewRecorder()
	b.handler.ServeHTTP(w, r)
	return w
}

func TestSettingsControlsCommitWindowCache(t *testing.T) {
	cache := github.NewCommitWindowCache(nil)
	session := app.New(nil, nil, nil, dispatch.New(nil, nil, nil, dispatch.Options{}), nil, app.Options{Cache: cache})
	b := newBrowser(t, session)
	getSettings := func(address string) string {
		t.Helper()
		response := httptest.NewRecorder()
		path, _, _ := strings.Cut(address, "#")
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:8080"+path, nil)
		b.handler.ServeHTTP(response, req)
		if response.Code != http.StatusOK {
			t.Fatalf("GET status = %d", response.Code)
		}
		return response.Body.String()
	}
	const checked = `aria-describedby="discovery-cache-help" checked`
	if body := getSettings("/?tab=settings"); !strings.Contains(body, checked) || !strings.Contains(body, ">Clear cache</button>") {
		t.Fatal("cache controls should be available before configuring a release")
	}
	action := url.Values{"tab": {"settings"}, "generation": {fmt.Sprint(session.Snapshot().Generation)},
		"kind": {"application-settings"}, "settings_section": {"cache-settings"}, "cache": {"false"}}
	if response := b.post(action); response.Code != http.StatusSeeOther || cache.Enabled() {
		t.Fatalf("disable status=%d enabled=%t", response.Code, cache.Enabled())
	} else if location := response.Header().Get("Location"); !strings.HasSuffix(location, "#cache-settings") || !strings.Contains(getSettings(location), "Settings saved.") {
		t.Fatal("saving settings should return to the panel with a confirmation")
	}
	if body := getSettings("/?tab=settings"); strings.Contains(body, checked) {
		t.Fatal("disabled cache rendered as enabled")
	}
	action.Set("kind", "clear-cache")
	response := b.post(action)
	if response.Code != http.StatusSeeOther || cache.Enabled() {
		t.Fatalf("clear status=%d enabled=%t", response.Code, cache.Enabled())
	}
	if body := getSettings(response.Header().Get("Location")); !strings.Contains(body, "Cache cleared.") {
		t.Fatal("clear confirmation missing")
	}
	action.Set("kind", "application-settings")
	action.Set("cache", "true")
	if response := b.post(action); response.Code != http.StatusSeeOther || !cache.Enabled() {
		t.Fatalf("enable status=%d enabled=%t", response.Code, cache.Enabled())
	}
}

func TestSaveApplicationSettingsTogether(t *testing.T) {
	cache := github.NewCommitWindowCache(nil)
	session := app.New(nil, nil, nil, dispatch.New(nil, nil, nil, dispatch.Options{EnableWrites: true}), nil, app.Options{Cache: cache})
	if err := session.Apply(t.Context(), app.Action{Kind: "configure", Config: validACS5Config()}); err != nil {
		t.Fatal(err)
	}
	b := newBrowser(t, session)
	fields := url.Values{"kind": {"application-settings"}, "generation": {fmt.Sprint(session.Snapshot().Generation)}, "interactive": {"false"}, "cache": {"false"}}
	if resp := b.post(fields); resp.Code != http.StatusSeeOther || session.Snapshot().Interactive || cache.Enabled() {
		t.Fatal("save did not apply both unchecked settings")
	}
	fields.Add("interactive", "true")
	fields.Add("cache", "true")
	fields.Set("kind", "clear-cache")
	if resp := b.post(fields); resp.Code != http.StatusSeeOther || session.Snapshot().Interactive || cache.Enabled() {
		t.Fatal("clear cache applied pending checkbox changes")
	}
	fields.Set("kind", "application-settings")
	if resp := b.post(fields); resp.Code != http.StatusSeeOther || !session.Snapshot().Interactive || !cache.Enabled() {
		t.Fatal("save did not apply both checked settings")
	}
}

func TestSettingsSaveLogsOnlyCommittedChanges(t *testing.T) {
	var output bytes.Buffer
	log := app.NewLog(&output, false)
	session := app.New(nil, nil, nil, dispatch.New(nil, nil, nil, dispatch.Options{EnableWrites: true}), log, app.Options{
		Initial: validACS5Config(), GitHubReads: githubread.New(nil), Cache: github.NewCommitWindowCache(nil),
	})
	b := newBrowser(t, session)
	values := url.Values{
		"kind": {"application-settings"}, "generation": {fmt.Sprint(session.Snapshot().Generation)},
		"startup_read_limit": {"5"}, "ongoing_read_limit": {"2"}, "interactive": {"false"}, "cache": {"false"},
	}
	checkSave := func(status int, want ...string) {
		t.Helper()
		output.Reset()
		offset := len(log.Recent())
		if response := b.post(values); response.Code != status {
			t.Fatalf("save returned HTTP %d, want %d", response.Code, status)
		}
		var got []string
		for _, entry := range log.Recent()[offset:] {
			got = append(got, entry.Message())
			if entry.Level() != "Info" || !strings.Contains(output.String(), " Info: "+entry.Message()+"\n") {
				t.Errorf("missing console Info line for %q", entry.Message())
			}
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) || strings.Count(output.String(), "\n") != len(want) {
			t.Fatalf("activity = %q, console = %q, want %q", got, output.String(), want)
		}
	}
	checkSave(http.StatusSeeOther,
		`Setting changed: "Concurrent GitHub reads (first 5 minutes)" from '3' to '5'.`,
		`Setting changed: "Concurrent GitHub reads (after 5 minutes)" from '1' to '2'.`,
		`Setting changed: "Confirm each retry comment" from 'true' to 'false'.`,
		`Setting changed: "Reuse GitHub commit history in memory" from 'true' to 'false'.`,
	)
	checkSave(http.StatusSeeOther)
	values.Del("startup_read_limit")
	values.Set("interactive", "true")
	values.Set("ongoing_read_limit", "4")
	checkSave(http.StatusConflict)
	values.Set("ongoing_read_limit", "2")
	checkSave(http.StatusSeeOther, `Setting changed: "Confirm each retry comment" from 'false' to 'true'.`)
}

func TestGETDoesNotMutateAndRendersIndependentPRControls(t *testing.T) {
	target := release.Target{Repo: "acme/rox", PR: 12}
	appFake := &fakeApplication{snapshot: app.Snapshot{Config: validACS5Config(), Generation: 8, Repositories: []release.Repository{{Source: release.Source{Name: "acs", Repo: "acme/rox", Branch: "release"}, PRs: []release.PR{{Target: target, SHA: "head-1", Title: "A PR", State: "open", Base: "release", Evidence: release.Evidence{Known: true, At: time.Now()}}}}}}}
	b := newBrowser(t, appFake)
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:8080/?kind=interactive", nil)
	r.Host = "127.0.0.1:8080"
	w := httptest.NewRecorder()
	b.handler.ServeHTTP(w, r)
	body := w.Body.String()
	if len(appFake.actions) != 0 {
		t.Fatalf("GET applied actions: %+v", appFake.actions)
	}
	for _, needle := range []string{"Include</button>", "Exclude</button>", "Ignore &amp; hide", "#12 A PR"} {
		if !strings.Contains(body, needle) {
			t.Errorf("page missing %q", needle)
		}
	}
}

func TestPOSTForwardsInclusionGrantAndGenerationSeparately(t *testing.T) {
	target := release.Target{Repo: "acme/rox", PR: 12}
	appFake := &fakeApplication{snapshot: app.Snapshot{Generation: 9}}
	b := newBrowser(t, appFake)
	choice := url.Values{"kind": {"choice"}, "generation": {"9"}, "repo": {"acme/rox"}, "pr": {"12"}, "sha": {"head-2"}, "choice": {"excluded", "included"}}
	if w := b.post(choice); w.Code != http.StatusSeeOther {
		t.Fatalf("choice status=%d body=%s", w.Code, w.Body.String())
	}
	grant := url.Values{"kind": {"grant"}, "generation": {"9"}, "repo": {"acme/rox"}, "pr": {"12"}, "sha": {"head-2"}, "enabled": {"false", "true"}}
	if w := b.post(grant); w.Code != http.StatusSeeOther {
		t.Fatalf("grant status=%d body=%s", w.Code, w.Body.String())
	}
	if len(appFake.actions) != 2 {
		t.Fatalf("got %d actions", len(appFake.actions))
	}
	if appFake.actions[0].Kind != "choice" || appFake.actions[0].Target != target || appFake.actions[0].Choice != release.Included || appFake.actions[0].Generation != 9 || appFake.actions[0].SHA != "head-2" {
		t.Fatalf("choice action=%+v", appFake.actions[0])
	}
	if appFake.actions[1].Kind != "grant" || !appFake.actions[1].Enabled || appFake.actions[1].Generation != 9 || appFake.actions[1].SHA != "head-2" {
		t.Fatalf("grant action=%+v", appFake.actions[1])
	}
}

func TestConfigureForACS5OmitsScannerAndForwardsFields(t *testing.T) {
	appFake := &fakeApplication{}
	b := newBrowser(t, appFake)
	form := url.Values{"kind": {"configure"}, "version": {"5.0.1"}, "branch": {"release-5.0"}, "fact_repo": {"acme/fact"}, "fact_branch": {"release-3.25"}, "fact_stream": {"3.25"}, "collector_repo": {"acme/collector"}, "collector_branch": {"release-3.25"}, "collector_stream": {"3.25"}, "scanner_repo": {"acme/scanner"}, "scanner_branch": {"release-4.8"}, "scanner_stream": {"4.8"}, "pinning_pr": {"41"}}
	w := b.post(form)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("configure status=%d body=%s", w.Code, w.Body.String())
	}
	location := w.Header().Get("Location")
	if location != "/?tab=settings&saved=configuration#configuration-panel" {
		t.Fatalf("unexpected save redirect %q", location)
	}
	appFake.snapshot.Config = appFake.actions[0].Config
	path, _, _ := strings.Cut(location, "#")
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:8080"+path, nil)
	result := httptest.NewRecorder()
	b.handler.ServeHTTP(result, r)
	if !strings.Contains(result.Body.String(), "Configuration saved.") || strings.Contains(b.getBody(t), "Configuration saved.") {
		t.Fatal("expected save confirmation only on the successful redirect")
	}
	if len(appFake.actions) != 1 {
		t.Fatalf("actions=%+v", appFake.actions)
	}
	config := appFake.actions[0].Config
	if len(config.Components) != 2 || config.Components[0].Name != "fact" || config.Components[1].Name != "collector" || config.PinningPR != 41 {
		t.Fatalf("config=%+v", config)
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("configuration does not pass session validation: %v", err)
	}
}

func TestConfigureForACS4IncludesScanner(t *testing.T) {
	appFake := &fakeApplication{}
	b := newBrowser(t, appFake)
	form := url.Values{"kind": {"configure"}, "version": {"4.10.2"}, "branch": {"release-4.10"}, "fact_repo": {"acme/fact"}, "fact_branch": {"release-0.2"}, "fact_stream": {"0.2"}, "collector_repo": {"acme/collector"}, "collector_branch": {"release-3.24"}, "collector_stream": {"3.24"}, "scanner_repo": {"acme/scanner"}, "scanner_branch": {"release-2.39"}, "scanner_stream": {"2.39"}}
	if w := b.post(form); w.Code != http.StatusSeeOther {
		t.Fatalf("configure status=%d body=%s", w.Code, w.Body.String())
	}
	config := appFake.actions[0].Config
	if len(config.Components) != 3 || config.Components[2] != (release.Source{Name: "scanner", Repo: "acme/scanner", Branch: "release-2.39", Stream: "2.39"}) {
		t.Fatalf("config=%+v", config)
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("ACS 4 config does not pass session validation: %v", err)
	}
}

func TestCrossSiteHostOriginAndCSRFRequestsAreRejected(t *testing.T) {
	appFake := &fakeApplication{}
	b := newBrowser(t, appFake)
	request := func(host, origin, csrf string) *httptest.ResponseRecorder {
		form := url.Values{"kind": {"interactive"}, "enabled": {"true"}, "generation": {"0"}, "csrf": {csrf}}
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+host+"/action", strings.NewReader(form.Encode()))
		r.Host = host
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", origin)
		r.Header.Set("Cookie", b.cookie)
		w := httptest.NewRecorder()
		b.handler.ServeHTTP(w, r)
		return w
	}
	if got := request("example.com", "http://example.com", b.csrf).Code; got != http.StatusForbidden {
		t.Fatalf("foreign host status=%d", got)
	}
	if got := request("127.0.0.1:8080", "http://evil.test", b.csrf).Code; got != http.StatusForbidden {
		t.Fatalf("foreign origin status=%d", got)
	}
	if got := request("127.0.0.1:8080", "http://127.0.0.1:8080", "wrong").Code; got != http.StatusForbidden {
		t.Fatalf("bad CSRF status=%d", got)
	}
	if len(appFake.actions) != 0 {
		t.Fatalf("rejected request mutated app: %+v", appFake.actions)
	}
}

func TestOpaqueOriginRequiresSameOriginFetchMetadataAndCSRF(t *testing.T) {
	appFake := &fakeApplication{}
	b := newBrowser(t, appFake)
	request := func(origin, site string) *httptest.ResponseRecorder {
		form := url.Values{"kind": {"interactive"}, "enabled": {"true"}, "generation": {"0"}, "csrf": {b.csrf}}
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://127.0.0.1:8080/action", strings.NewReader(form.Encode()))
		r.Host = "127.0.0.1:8080"
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Cookie", b.cookie)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if site != "" {
			r.Header.Set("Sec-Fetch-Site", site)
		}
		w := httptest.NewRecorder()
		b.handler.ServeHTTP(w, r)
		return w
	}
	if got := request("null", "same-origin").Code; got != http.StatusSeeOther {
		t.Fatalf("opaque same-origin submission status=%d", got)
	}
	if got := request("", "same-origin").Code; got != http.StatusSeeOther {
		t.Fatalf("absent-origin same-origin submission status=%d", got)
	}
	if got := request("null", "").Code; got != http.StatusForbidden {
		t.Fatalf("opaque origin without fetch metadata status=%d", got)
	}
	if got := request("http://evil.test", "same-origin").Code; got != http.StatusForbidden {
		t.Fatalf("foreign origin with same-origin metadata status=%d", got)
	}
	if len(appFake.actions) != 2 {
		t.Fatalf("accepted actions=%d, want 2", len(appFake.actions))
	}
}

func TestPageEscapesExternalTextAndShowsUnknownEvidenceAndExactOffer(t *testing.T) {
	target := release.Target{Repo: "acme/rox", PR: 7}
	proposal := release.Proposal{Target: target, SHA: "head-sha", Job: "unit", RunID: "run-77", Payload: "/konflux-retest unit"}
	appFake := &fakeApplication{snapshot: app.Snapshot{
		Config:       validACS5Config(),
		Generation:   4,
		Interactive:  true,
		Choices:      release.Choices{PRs: map[release.Target]release.Choice{target: release.Included}},
		Repositories: []release.Repository{{Source: release.Source{Name: "acs", Repo: "acme/rox", Branch: "release"}, PRs: []release.PR{{Target: target, SHA: "head-sha", Title: `<script>alert("x")</script>`, URL: "javascript:alert(1)", State: "open", Base: "release", Evidence: release.Evidence{At: time.Now(), Error: "head observation partial"}}}}},
		Assessment: release.Assessment{
			Conditions:            []release.Condition{{Name: "included PR " + target.String(), Status: "", Reason: "Coverage cannot be established", Evidence: []release.Evidence{{Error: "compare API unavailable"}}}},
			PinningRecommendation: "Create an ACS pinning PR against release-5.0 to set COLLECTOR_VERSION to 3.25.7.",
		},
		Plans:     []release.Plan{{Target: target, SHA: "head-sha", Evidence: release.Evidence{Known: false, Error: "partial plan"}, Proposals: []release.Proposal{proposal}}},
		Authority: []dispatch.View{{Target: target, SHA: "head-sha", Granted: true, Warnings: []string{"slow retry threshold reached"}, Offers: []dispatch.Offer{{ID: "offer-1", Proposal: proposal, Allowed: true, CanApprove: true, Queueable: true}}}},
	}}
	b := newBrowser(t, appFake)
	body := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:8080/", nil)
	r.Host = "127.0.0.1:8080"
	b.handler.ServeHTTP(body, r)
	text := body.Body.String()
	if strings.Contains(text, `<script>alert("x")</script>`) {
		t.Fatal("external PR title was rendered as markup")
	}
	for _, needle := range []string{"&lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt;", "unknown", "compare API unavailable", "/konflux-retest unit", "run-77", "Open Retest Queue", "<p>Create an ACS pinning PR against release-5.0 to set COLLECTOR_VERSION to 3.25.7.</p>"} {
		if !strings.Contains(text, needle) {
			t.Errorf("rendered page missing %q", needle)
		}
	}
	if strings.Contains(text, "Pinning commands") || strings.Contains(text, "Copy commands") {
		t.Fatal("pinning guidance included a command block")
	}
	if strings.Contains(text, `href="javascript:alert(1)"`) {
		t.Fatal("unsafe external URL was emitted")
	}
	if !strings.Contains(text, "slow retry threshold reached") {
		t.Fatal("dispatcher warning was not rendered")
	}
}

func TestIgnoredPRIsHiddenAndConfirmedTagHasIndependentRWControl(t *testing.T) {
	prTarget := release.Target{Repo: "acme/rox", PR: 8}
	tagTarget := release.Target{Repo: "acme/fact", Tag: "3.25.7"}
	appFake := &fakeApplication{snapshot: app.Snapshot{
		Config: validACS5Config(), WritesEnabled: true,
		Choices: release.Choices{PRs: map[release.Target]release.Choice{prTarget: release.Ignored}, Tags: map[string]string{"fact": "3.25.7"}},
		Repositories: []release.Repository{
			{Source: release.Source{Name: "acs", Repo: "acme/rox", Branch: "release-5.0"}, PRs: []release.PR{{Target: prTarget, SHA: "ignored-sha", Title: "Ignored PR", State: "open"}}},
			{Source: release.Source{Name: "fact", Repo: "acme/fact", Branch: "release-3.25", Stream: "3.25"}, Tags: []release.Tag{{Name: "3.25.7", SHA: "tag-sha", Belongs: release.Yes, Evidence: release.Evidence{Known: true, At: time.Now()}}}},
		},
		Authority: []dispatch.View{{Target: prTarget, SHA: "ignored-sha", Ignored: true}, {Target: tagTarget, SHA: "tag-sha", Granted: true}},
	}}
	b := newBrowser(t, appFake)
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:8080/", nil)
	r.Host = "127.0.0.1:8080"
	w := httptest.NewRecorder()
	b.handler.ServeHTTP(w, r)
	body := w.Body.String()
	if strings.Contains(body, "Ignored PR") {
		t.Fatal("ignored PR remained visible")
	}
	tagRow := tagRowContaining(body, "tag-sha")
	if !strings.Contains(tagRow, `value="acme/fact"`) || !strings.Contains(tagRow, `value="3.25.7"`) || !strings.Contains(tagRow, retryCommentPermissionLabel) {
		t.Fatal("confirmed tag target is missing independent RW controls")
	}
}

func TestQueueFormsRequireAnIDAndForwardOnlyTheDecision(t *testing.T) {
	for _, kind := range []string{"queue-send", "queue-skip"} {
		t.Run(kind, func(t *testing.T) {
			appFake := &fakeApplication{}
			b := newBrowser(t, appFake)
			form := url.Values{"kind": {kind}, "tab": {"retest-queue"}, "generation": {"6"}}
			for _, invalid := range []string{"", "0", "-1", "fake"} {
				form.Set("queue_id", invalid)
				if w := b.post(form); w.Code != http.StatusBadRequest {
					t.Fatalf("invalid queue ID returned status %d", w.Code)
				}
			}
			form.Set("queue_id", "27")
			if w := b.post(form); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/?tab=retest-queue" {
				t.Fatalf("queue decision returned status %d, destination %q", w.Code, w.Header().Get("Location"))
			}
			if len(appFake.actions) != 1 || appFake.actions[0].Kind != kind || appFake.actions[0].QueueID != 27 || appFake.actions[0].Generation != 6 {
				t.Fatalf("queue decision = %+v", appFake.actions)
			}
		})
	}
}

func TestTagConfirmationForwardsExactIdentity(t *testing.T) {
	appFake := &fakeApplication{}
	b := newBrowser(t, appFake)
	form := url.Values{"kind": {"tag"}, "generation": {"3"}, "component": {"fact"}, "tag": {"3.25.7"}, "sha": {"exact-sha"}}
	if w := b.post(form); w.Code != http.StatusSeeOther {
		t.Fatalf("tag confirmation status=%d body=%s", w.Code, w.Body.String())
	}
	if len(appFake.actions) != 1 {
		t.Fatalf("actions=%+v", appFake.actions)
	}
	a := appFake.actions[0]
	if a.Kind != "tag" || a.Component != "fact" || a.Tag != "3.25.7" || a.SHA != "exact-sha" || a.Generation != 3 {
		t.Fatalf("tag action=%+v", a)
	}
}

func TestRecommendedTagConfirmationIsSeparateFromPRSelection(t *testing.T) {
	application := &fakeApplication{snapshot: app.Snapshot{
		Config: validACS5Config(), Generation: 17,
		Assessment: release.Assessment{ProposedTags: map[string]string{"fact": "3.25.9"}},
		Repositories: []release.Repository{
			{Source: release.Source{Name: "acs", Repo: "acme/rox"}, Tags: []release.Tag{{Name: "5.0.0"}}},
			{Source: release.Source{Name: "fact", Repo: "acme/fact", Stream: "3.25"},
				PRs: []release.PR{{Target: release.Target{Repo: "acme/fact", PR: 9}, Title: "A selectable change", State: "open"}},
				Tags: []release.Tag{
					{Name: "3.25.1", SHA: "old", Belongs: release.Yes, Evidence: release.Evidence{Known: true, At: time.Now()}},
					{Name: "3.25.9", SHA: "recommended-sha", Belongs: release.Yes, Evidence: release.Evidence{Known: true, At: time.Now()}},
				}},
		},
	}}
	b := newBrowser(t, application)
	w := httptest.NewRecorder()
	b.handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:8080/", nil))
	body := w.Body.String()
	tags := between(t, body, `<div id="tag-selection">`, `<section class="panel" id="component-images-panel">`)
	prs := between(t, body, `<section class="panel" aria-labelledby="pr-selection-heading"`, `</section>`)
	if strings.Contains(tags, "A selectable change") || !strings.Contains(prs, "A selectable change") {
		t.Fatal("component PRs and tags must have separate activity views")
	}
	if strings.Contains(body, "finish-selection") {
		t.Fatal("individual PR choices must not require extra confirmation")
	}
	if !strings.Contains(tags, `aria-label="Fact tag choices"`) {
		t.Fatal("component tag choices must identify the component")
	}
	if strings.Contains(tags, "5.0.0") {
		t.Fatal("ACS release tags must not be offered as component tags")
	}
	if strings.Contains(tags, `<details class="other-tags"`) || strings.Contains(tags, `<details class="component-tag-chooser"`) {
		t.Fatal("tag options must be visible without expanding a chooser")
	}
	form := ""
	for _, candidate := range regexp.MustCompile(`(?s)<form\b[^>]*>.*?</form>`).FindAllString(tags, -1) {
		if strings.Contains(candidate, "Confirm fact 3.25.9") {
			form = candidate
			break
		}
	}
	if form == "" {
		t.Fatal("recommended tag confirmation form missing")
	}
	values := url.Values{}
	for _, field := range []string{"csrf", "kind", "generation", "component", "tag", "sha"} {
		values.Set(field, inputValue(t, form, field))
	}
	if values.Get("csrf") != b.csrf {
		t.Fatal("nested tag form lost its CSRF token")
	}
	if result := b.post(values); result.Code != http.StatusSeeOther {
		t.Fatalf("rendered tag form rejected: %d %s", result.Code, result.Body.String())
	}
	action := application.actions[0]
	if action.Component != "fact" || action.Tag != "3.25.9" || action.SHA != "recommended-sha" || action.Generation != 17 {
		t.Fatalf("rendered form changed the recommended identity: %+v", action)
	}
}

func TestTagSelectionDoesNotAutoFoldAfterConfirmationAndWithholdsUnknownTags(t *testing.T) {
	for name, evidence := range map[string]release.Evidence{
		"unknown": {At: time.Now()},
		"stale":   {Known: true, At: time.Now().Add(-time.Hour), Error: "latest discovery failed"},
	} {
		t.Run(name, func(t *testing.T) {
			b := newBrowser(t, &fakeApplication{snapshot: app.Snapshot{
				Config:     validACS5Config(),
				Choices:    release.Choices{Tags: map[string]string{"fact": "3.25.9", "collector": "3.25.4"}},
				Assessment: release.Assessment{ProposedTags: map[string]string{"fact": "3.25.9"}},
				Repositories: []release.Repository{{Source: release.Source{Name: "fact", Repo: "acme/fact"},
					Tags: []release.Tag{{Name: "3.25.9", SHA: "tag-sha", Belongs: release.Yes, Evidence: evidence}}}},
			}})
			w := httptest.NewRecorder()
			b.handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:8080/", nil))
			body := w.Body.String()
			if !strings.Contains(body, `<div id="tag-selection">`) {
				t.Fatal("tag confirmation must not choose a new fold state")
			}
			if strings.Contains(body, "Confirm fact 3.25.9") || !strings.Contains(body, "Unknown or stale") {
				t.Fatal("unknown tag evidence must remain visible without a confirmation control")
			}
		})
	}
}

func TestComponentPRSelectionShowsAuthorsAndSummarizesUnverifiedChecks(t *testing.T) {
	target := release.Target{Repo: "acme/fact", PR: 1}
	b := newBrowser(t, &fakeApplication{snapshot: app.Snapshot{
		Config:  validACS5Config(),
		Choices: release.Choices{PRs: map[release.Target]release.Choice{target: release.Included}},
		Repositories: []release.Repository{{Source: release.Source{Name: "fact", Repo: "acme/fact"}, PRs: []release.PR{
			{Target: target, Author: "octocat", State: "open", SHA: "exact-pr-head", OptOut: true, Checks: []release.Check{{Name: "unit-tests", Conclusion: "failure"}}},
			{Target: release.Target{Repo: "acme/fact", PR: 2}, Author: "", State: "open", SHA: "other-head"},
			{Target: release.Target{Repo: "acme/fact", PR: 3}, Author: `<img src=x onerror=alert(1)>`, State: "open"},
		}}},
	}})
	w := httptest.NewRecorder()
	b.handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:8080/", nil))
	body := w.Body.String()
	for _, want := range []string{"@octocat", "Author unknown", "&lt;img src=x onerror=alert(1)&gt;", "PR identity unknown or stale", "no-release-assistant opt-out"} {
		if !strings.Contains(body, want) {
			t.Errorf("PR selection missing %q", want)
		}
	}
	if strings.Contains(body, `<img src=x`) {
		t.Fatal("author was rendered as markup")
	}
	card := regexp.MustCompile(`(?s)<article class="pr-card"[^>]*>.*?</article>`).FindString(body)
	if strings.Contains(card, "Evidence and checks") || strings.Contains(card, "unit-tests") ||
		!strings.Contains(card, "View checks") || !strings.Contains(card, "Checks unavailable") {
		t.Fatal("the component PR should show unverified status without exposing the check table")
	}
	if !strings.Contains(card, `name="sha" value="exact-pr-head"`) ||
		!strings.Contains(card, `value="included" aria-pressed="true"`) || strings.Contains(card, `name="kind" value="grant"`) {
		t.Fatal("Read-only mode should keep the selection controls and omit automatic posting controls")
	}
}

func TestPRViewHidesMergedAndClosedPRsWithoutDiscardingCoverageEvidence(t *testing.T) {
	for name, source := range map[string]release.Source{
		"ACS":       {Name: "acs", Repo: "acme/rox", Branch: "release-5.0"},
		"component": {Name: "fact", Repo: "acme/fact", Branch: "release-3.25", Stream: "3.25"},
	} {
		t.Run(name, func(t *testing.T) {
			merged := release.Target{Repo: source.Repo, PR: 2}
			application := &fakeApplication{snapshot: app.Snapshot{
				Config:  validACS5Config(),
				Choices: release.Choices{PRs: map[release.Target]release.Choice{merged: release.Included}},
				Repositories: []release.Repository{{Source: source, PRs: []release.PR{
					{Target: release.Target{Repo: source.Repo, PR: 1}, State: "open", Title: "Open change"},
					{Target: merged, State: "merged", Merged: true, MergeSHA: "merge-sha", Title: "Merged change"},
					{Target: release.Target{Repo: source.Repo, PR: 3}, State: "closed", Merged: true, Title: "Closed merged change"},
					{Target: release.Target{Repo: source.Repo, PR: 4}, State: "open", Merged: true, Title: "Merged flag takes precedence"},
					{Target: release.Target{Repo: source.Repo, PR: 5}, State: "closed", Title: "Closed unmerged change"},
				}, Tags: []release.Tag{{Name: "3.25.9", Contains: map[string]release.Truth{"merge-sha": release.Yes}}}}},
				Plans:     []release.Plan{{Target: merged, Message: "Old merged PR retry observations"}},
				Authority: []dispatch.View{{Target: merged, Message: "Old merged PR permission panel"}},
			}}
			b := newBrowser(t, application)
			w := httptest.NewRecorder()
			b.handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:8080/", nil))
			body := w.Body.String()
			if !strings.Contains(body, "Open change") || strings.Count(body, `<article class="pr-card"`) != 1 {
				t.Fatal("PR selection must show exactly the open, unmerged PR")
			}
			for _, hidden := range []string{
				"Closed merged change", "Merged flag takes precedence", "Closed unmerged change",
				"Old merged PR retry observations", "Old merged PR permission panel",
			} {
				if strings.Contains(body, hidden) {
					t.Errorf("inactive PR information still visible: %s", hidden)
				}
			}
			repo := application.snapshot.Repositories[0]
			if len(repo.PRs) != 5 || repo.PRs[1].MergeSHA != "merge-sha" ||
				application.snapshot.Choices.PRs[merged] != release.Included ||
				repo.Tags[0].Contains["merge-sha"] != release.Yes {
				t.Fatal("view filtering discarded merge identity, inclusion intent, or tag coverage")
			}
			application.snapshot.Repositories[0].PRs[0].State = "merged"
			application.snapshot.Repositories[0].PRs[0].Merged = true
			afterMerge := httptest.NewRecorder()
			b.handler.ServeHTTP(afterMerge, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:8080/", nil))
			if strings.Contains(afterMerge.Body.String(), `<article class="pr-card"`) ||
				!strings.Contains(afterMerge.Body.String(), "No open PR observations.") {
				t.Fatal("an observed merge must remove the open PR row on refresh")
			}
		})
	}
}

func TestFormBodyLimit(t *testing.T) {
	appFake := &fakeApplication{}
	b := newBrowser(t, appFake)
	large := "csrf=" + url.QueryEscape(b.csrf) + "&x=" + strings.Repeat("x", maxFormBytes+1)
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://127.0.0.1:8080/action", strings.NewReader(large))
	r.Host = "127.0.0.1:8080"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://127.0.0.1:8080")
	r.Header.Set("Cookie", b.cookie)
	w := httptest.NewRecorder()
	b.handler.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		body, _ := io.ReadAll(w.Result().Body)
		t.Fatalf("oversized body status=%d body=%s", w.Code, body)
	}
}

func TestIncompleteInitialConfigKeepsSetupOpenAndVersionInTitle(t *testing.T) {
	appFake := &fakeApplication{snapshot: app.Snapshot{Config: release.Config{Version: "5.0.1"}}}
	b := newBrowser(t, appFake)
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:8080/", nil)
	r.Host = "127.0.0.1:8080"
	w := httptest.NewRecorder()
	b.handler.ServeHTTP(w, r)
	body := w.Body.String()
	if !strings.Contains(body, "<title>ACS 5.0.1 Release Assistant</title>") || !strings.Contains(body, `<h3 id="setup-heading">`) {
		t.Fatal("incomplete CLI configuration did not keep setup visible")
	}
	if n := strings.Count(body, `role="tab" aria-disabled="true"`); n != 4 {
		t.Fatalf("incomplete configuration shows %d disabled phases, want 4", n)
	}
	if !strings.Contains(body, "Setup required</span>") || strings.Contains(body, `data-workflow-tab="component-prs"`) {
		t.Fatal("initial setup must flag Settings and disable phase navigation")
	}
	if strings.Contains(body, "Confirm PR selection") {
		t.Fatal("incomplete configuration rendered session controls")
	}
}

func TestParallelPortsUseDistinctCSRFCookies(t *testing.T) {
	handler, err := New(&fakeApplication{})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, host := range []string{"127.0.0.1:8080", "127.0.0.1:8081"} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+host+"/", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		cookies := w.Result().Cookies()
		if len(cookies) != 1 || names[cookies[0].Name] {
			t.Fatal("parallel instances overwrite CSRF cookie")
		}
		names[cookies[0].Name] = true
	}
}

func TestConfigurationErrorsPreserveSubmittedInputs(t *testing.T) {
	for name, tc := range map[string]struct {
		version, pinning, branch string
		applyError               error
		status, actions          int
	}{
		"invalid PR URL on initial setup":                          {"4.10.2", "https://github.com/stackrox/stackrox/pull/12345?x=1&y=2", "release-4.10", nil, http.StatusBadRequest, 0},
		"invalid version before other fields parse":                {"not-a-version", "12345", "release-4.10", nil, http.StatusBadRequest, 0},
		"application validation while editing saved configuration": {"4.10.2", "12345", "", errors.New("an explicit ACS branch is required"), http.StatusConflict, 1},
		"escaped rejected input":                                   {"4.10.2", `"><script>alert(1)</script>`, "release-4.10", nil, http.StatusBadRequest, 0},
	} {
		t.Run(name, func(t *testing.T) {
			snapshot := app.Snapshot{}
			if tc.applyError != nil {
				snapshot.Config = validACS5Config()
			}
			application := &fakeApplication{snapshot: snapshot, err: tc.applyError}
			b := newBrowser(t, application)
			form := url.Values{"kind": {"configure"}, "version": {tc.version}, "branch": {tc.branch}, "pinning_pr": {tc.pinning}, "fact_repo": {"stackrox/fact"}, "fact_branch": {"release-0.3"}, "fact_stream": {"0.3"}, "collector_repo": {"stackrox/collector"}, "collector_branch": {"release-3.24"}, "collector_stream": {"3.24"}, "scanner_repo": {"stackrox/scanner"}, "scanner_branch": {"release-2.39"}, "scanner_stream": {"2.39"}}
			w := b.post(form)
			if w.Code != tc.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			for field, values := range form {
				if field == "kind" || field == "csrf" {
					continue
				}
				if got := inputValue(t, body, field); got != values[0] {
					t.Errorf("%s = %q, want submitted %q", field, got, values[0])
				}
			}
			if !strings.Contains(body, `<h3 id="setup-heading">`) || !strings.Contains(body, `aria-labelledby="setup-heading" data-live-preserve`) {
				t.Error("rejected configuration must remain open and protected from automatic refresh")
			}
			if strings.Contains(body, "<script>alert(1)</script>") {
				t.Error("submitted input was not escaped")
			}
			if len(application.actions) != tc.actions || !reflect.DeepEqual(application.snapshot, snapshot) {
				t.Fatal("rejected draft changed the active session")
			}
			if name == "invalid PR URL on initial setup" {
				form.Set("pinning_pr", "12345")
				if corrected := b.post(form); corrected.Code != http.StatusSeeOther {
					t.Fatal("corrected draft could not be submitted")
				}
				if len(application.actions) != 1 || application.actions[0].Config.PinningPR != 12345 {
					t.Fatal("corrected PR number was not applied")
				}
				if err := application.actions[0].Config.Validate(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestPinningPRFieldExplainsAndConstrainsNumberInput(t *testing.T) {
	b := newBrowser(t, &fakeApplication{})
	w := httptest.NewRecorder()
	b.handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:8080/", nil))
	body := w.Body.String()
	for _, want := range []string{"Pinning PR number (optional)", `pattern="[0-9]+"`, `placeholder="12345"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing number guidance %q", want)
		}
	}
}

func TestVersionDefaultsPrefillStreamsBranchesAndScannerRepository(t *testing.T) {
	for name, tc := range map[string]struct {
		version     string
		streams     map[string]string
		scannerRepo string
	}{
		"ACS 4.10 includes scanner": {"4.10.12", map[string]string{"fact": "0.2", "collector": "3.24", "scanner": "2.39"}, "stackrox/scanner"},
		"ACS 4.11 includes scanner": {"4.11.3", map[string]string{"fact": "0.3", "collector": "3.25", "scanner": "2.40"}, "stackrox/scanner"},
		"ACS 5.1 omits scanner":     {"5.1.0", map[string]string{"fact": "0.5", "collector": "3.27", "scanner": ""}, ""},
		"no CLI version":            {"", map[string]string{"fact": "", "collector": "", "scanner": ""}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			config := prefilledConfig(tc.version)
			b := newBrowser(t, &fakeApplication{snapshot: app.Snapshot{Config: config}})
			w := httptest.NewRecorder()
			b.handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:8080/", nil))
			body := w.Body.String()
			for component, stream := range tc.streams {
				if got := inputValue(t, body, component+"_stream"); got != stream {
					t.Errorf("%s stream = %q, want prefilled %q", component, got, stream)
				}
				wantBranch := ""
				if stream != "" {
					wantBranch = "release-" + stream
				}
				if got := inputValue(t, body, component+"_branch"); got != wantBranch {
					t.Errorf("%s branch = %q, want %q", component, got, wantBranch)
				}
				branch := regexp.MustCompile(`<input\b[^>]*\bname="` + component + `_branch"[^>]*>`).FindString(body)
				if !strings.Contains(branch, " readonly ") {
					t.Errorf("%s branch must be read-only", component)
				}
				if strings.Index(body, `name="`+component+`_repo"`) > strings.Index(body, `name="`+component+`_stream"`) ||
					strings.Index(body, `name="`+component+`_stream"`) > strings.Index(body, `name="`+component+`_branch"`) {
					t.Errorf("%s fields must appear in repository, stream, branch order", component)
				}
			}
			if got := inputValue(t, body, "scanner_repo"); got != tc.scannerRepo {
				t.Errorf("scanner repository = %q, want %q", got, tc.scannerRepo)
			}
		})
	}
}

func TestBrowserVersionDoesNotReapplyCLIPrefills(t *testing.T) {
	config := prefilledConfig("4.10.12")
	config.Version = "5.1.0" // A later browser edit must not re-run the prefill table.
	b := newBrowser(t, &fakeApplication{snapshot: app.Snapshot{Config: config}})
	w := httptest.NewRecorder()
	b.handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:8080/", nil))
	body := w.Body.String()
	if got := inputValue(t, body, "fact_stream"); got != "0.2" {
		t.Errorf("fact stream = %q, want the CLI prefill 0.2", got)
	}
	if got := inputValue(t, body, "collector_stream"); got != "3.24" {
		t.Errorf("collector stream = %q, want the CLI prefill 3.24", got)
	}
}

func TestConfigureDerivesBranchesAndPreservesUnchangedCLIOverrides(t *testing.T) {
	for name, tc := range map[string]struct {
		repo, stream, branch string
	}{
		"unchanged CLI override": {"acme/fact", "3.25", "test-release"},
		"changed release stream": {"acme/fact", "0.5", "release-0.5"},
		"changed repository":     {"other/fact", "3.25", "release-3.25"},
	} {
		t.Run(name, func(t *testing.T) {
			config := validACS5Config()
			config.Components[0].Branch = "test-release"
			application := &fakeApplication{snapshot: app.Snapshot{Config: config}}
			b := newBrowser(t, application)
			w := httptest.NewRecorder()
			b.handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:8080/", nil))
			if inputValue(t, w.Body.String(), "fact_stream") != "3.25" || inputValue(t, w.Body.String(), "fact_branch") != "test-release" {
				t.Fatal("saved configuration was overwritten by defaults")
			}
			form := url.Values{
				"kind": {"configure"}, "version": {"5.0.1"}, "branch": {"release-5.0"},
				"fact_repo": {tc.repo}, "fact_stream": {tc.stream}, "fact_branch": {"forged-branch"},
				"collector_repo": {"acme/collector"}, "collector_stream": {"3.26"},
			}
			if result := b.post(form); result.Code != http.StatusSeeOther {
				t.Fatalf("configure status=%d body=%s", result.Code, result.Body.String())
			}
			got := application.actions[0].Config
			if got.Components[0].Branch != tc.branch || got.Components[1].Branch != "release-3.26" {
				t.Fatalf("submitted branches were not resolved from component scope: %+v", got.Components)
			}
		})
	}
}

func TestInvalidDraftKeepsEmptyStreamAndRecomputesReadOnlyBranches(t *testing.T) {
	config := validACS5Config()
	config.Components[0].Stream = "0.4"
	config.Components[0].Branch = "test-release"
	b := newBrowser(t, &fakeApplication{snapshot: app.Snapshot{Config: config}})
	w := b.post(url.Values{
		"kind": {"configure"}, "version": {"4.10.2"}, "pinning_pr": {"not-a-number"},
		"fact_repo": {"acme/fact"}, "fact_stream": {""}, "fact_branch": {"forged-branch"},
		"collector_stream": {"3.27"}, "collector_branch": {"stale-branch"},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", w.Code)
	}
	body := w.Body.String()
	if inputValue(t, body, "fact_stream") != "" || inputValue(t, body, "fact_branch") != "" {
		t.Fatal("rejected empty stream must remain empty without a branch")
	}
	if got := inputValue(t, body, "collector_branch"); got != "release-3.27" {
		t.Fatalf("read-only branch=%q, want release-3.27", got)
	}
	if !strings.Contains(body, `data-configured-repo="acme/fact" data-configured-stream="0.4" data-configured-branch="test-release"`) {
		t.Fatal("rejected draft lost the CLI override needed when restoring the configured stream")
	}
}

func inputValue(t *testing.T, body, name string) string {
	t.Helper()
	input := regexp.MustCompile(`<input\b[^>]*\bname="` + regexp.QuoteMeta(name) + `"[^>]*>`).FindString(body)
	if input == "" {
		t.Fatalf("input %q missing", name)
	}
	value := regexp.MustCompile(`\bvalue="([^"]*)"`).FindStringSubmatch(input)
	if len(value) != 2 {
		t.Fatalf("input %q value missing", name)
	}
	return html.UnescapeString(value[1])
}

func TestInitialLoadingStateIsVisibleUntilRefreshCompletes(t *testing.T) {
	application := &fakeApplication{snapshot: app.Snapshot{Config: validACS5Config(), Loading: true}}
	b := newBrowser(t, application)
	body := b.getBody(t)
	if !strings.Contains(body, `data-loading="true"`) || !strings.Contains(body, `role="status">Loading release data…`) {
		t.Fatal("initial load is not visible to the browser")
	}
	application.snapshot.Loading = false
	body = b.getBody(t)
	if !strings.Contains(body, `data-loading="false"`) || strings.Contains(body, "Loading release data…") {
		t.Fatal("completed load still requests rapid refresh")
	}
}

func TestCompactReleaseSummaryAndIndividualImageResults(t *testing.T) {
	observed := release.Evidence{Known: true, At: time.Now()}
	image := func(name, component, family, ref string, status release.Truth) release.ImageStatus {
		evidence := observed
		repo, tag, _ := strings.Cut(strings.TrimPrefix(ref, "quay.io/"), ":")
		evidence.URL = "https://quay.io/repository/" + repo + "?tag=" + tag
		return release.ImageStatus{
			Image:  release.Image{Name: name, Component: component, Family: family, Reference: ref, Present: status, Evidence: evidence},
			Status: status, Required: component != "acs",
		}
	}
	rows := []release.ImageStatus{image("fact", "fact", "ci", "quay.io/fixture/fact:3.25.1", release.Yes), image("collector", "collector", "konflux", "quay.io/fixture/collector:3.25.4-fast", release.No), image("main", "acs", "ci", "quay.io/rhacs-eng/main:5.0.1-rc.0", release.No), image("scanner-v4", "acs", "konflux", "quay.io/fixture/release-scanner-v4:5.0.1-rc.0", "")}
	rows[3].Image.Evidence.Known = false
	rows[3].Image.Evidence.Error = "Registry access denied"
	rows[3].Reason = "The registry request could not complete."
	factKonflux := image("fact", "fact", "konflux", "quay.io/fixture/release-fact:3.25.1-fast", release.Yes)
	factKonflux.SelectedTag = "3.25.1"
	collectorCI := image("collector", "collector", "ci", "quay.io/fixture/collector:3.25.4", release.Yes)
	collectorCI.SelectedTag = "3.25.4"
	collectorCI.Status = ""
	collectorCI.Image.Evidence.Known = false
	collectorCI.Image.Evidence.Error = "Registry timed out"
	rows = append(rows, factKonflux, collectorCI)
	rows = append(rows, image("main", "acs", "konflux", "quay.io/fixture/release-main:5.0.1-rc.0", release.Yes))
	application := &fakeApplication{snapshot: app.Snapshot{Config: validACS5Config(), Choices: release.Choices{Tags: map[string]string{"fact": "3.25.1", "collector": "3.25.4"}}, Assessment: release.Assessment{Images: rows, Pins: release.PinSummary{BranchStatus: release.Yes, PRNumber: 99, PRURL: "https://github.com/stackrox/stackrox/pull/99"}, Conditions: []release.Condition{{Name: "configuration", Status: release.Yes, Reason: "Release configuration is valid"}}}, Repositories: []release.Repository{{Source: release.Source{Name: "acs"}, PinningPR: &release.PR{Target: release.Target{Repo: "stackrox/stackrox", PR: 99}, State: "open", Evidence: observed}}}}}
	body := newBrowser(t, application).getBody(t)
	for _, want := range []string{
		"Fact: 3.25.1", "Collector: 3.25.4", "ACS pins: match selected tags", "Pin PR #99: Open",
		`href="https://github.com/stackrox/stackrox/pull/99"`, "Available", "Not found", "Unable to check",
		`href="https://quay.io/repository/fixture/release-fact?tag=3.25.1-fast"`,
		`href="https://quay.io/repository/fixture/release-main?tag=5.0.1-rc.0"`,
		"Registry access denied",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"Release status", "Release configuration is valid", "Confirm PR selection",
		"confirm-selection", "image inventory evidence is not fresh", "Saved pattern source",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("unexpected obsolete status/control %q", forbidden)
		}
	}
	if !strings.Contains(body, `class="pill unknown" href="https://github.com/stackrox/stackrox/pull/99"`) {
		t.Fatal("an open pin PR must not imply its proposed versions have merged")
	}
	components := between(t, body, `id="component-images-panel"`, `</section>`)
	releaseImages := between(t, body, `id="release-images-panel"`, `</section>`)
	if strings.Count(components, `scope="row"`) != 2 || strings.Contains(components, "5.0.1-rc.0") ||
		!strings.Contains(releaseImages, "<code>operator-index</code></th>") || strings.Contains(releaseImages, "3.25.4-fast") {
		t.Fatal("component prerequisites and later ACS RC.0 images are mixed")
	}
	rcHeading := between(t, releaseImages, `id="release-images-heading"`, `</h3>`)
	if !strings.Contains(rcHeading, "1 of 3 available · 1 not found · 1 unable to check") {
		t.Error("RC image header must report all current statuses")
	}
	rcMain := between(t, releaseImages, `<th scope="row"><code>main</code></th>`, "</tr>")
	if !strings.Contains(rcMain, "quay.io/rhacs-eng/main:5.0.1-rc.0") || !strings.Contains(rcMain, "quay.io/fixture/release-main:5.0.1-rc.0") {
		t.Error("both RC image families must appear in the same row")
	}
	heading := between(t, components, `id="component-images-heading"`, `</summary>`)
	if !strings.Contains(heading, "2 of 4 available · 1 not found · 1 unable to check") {
		t.Error("collapsed component image heading must report all current statuses")
	}
	factRow := between(t, components, `<th scope="row"><code>fact</code></th>`, `</tr>`)
	if !strings.Contains(factRow, "component-image:fact:ci:fact") || !strings.Contains(factRow, "component-image:fact:konflux:fact") {
		t.Error("both Fact image families must appear in the same row")
	}
	for _, want := range []string{"Last result unverified", "Registry timed out", "Last confirmed available", "Current availability is unverified"} {
		if !strings.Contains(components, want) {
			t.Errorf("missing stale component evidence %q", want)
		}
	}
	if strings.Contains(body, `<details id="pr-selection"`) || !strings.Contains(body, `id="pr-selection"`) {
		t.Error("PR selection must stay visible and directly linkable")
	}
	for _, control := range []string{`<details id="component-images" open>`, `<details id="acs-prs-retry-details" open>`, `<details id="component-releases-retry-details" open>`} {
		if !strings.Contains(body, control) {
			t.Errorf("missing section fold control %s", control)
		}
	}
	if !strings.Contains(body, `href="https://quay.io/repository/fixture/fact?tag=3.25.1"`) {
		t.Fatal("image evidence must link to the exact tag")
	}
}

func TestActivityConsoleEscapesErrorsAndReadEndpointDoesNotMutate(t *testing.T) {
	message := `GET /repos/acme/fact/branches/release rejected (HTTP 401) <script>alert(1)</script>`
	application := &fakeApplication{snapshot: app.Snapshot{LogEntries: []app.Record{{At: time.Now(), Target: release.Target{Repo: "acme/fact"}, Decision: "github-error", Result: message, Payload: "not-for-console"}}}}
	b := newBrowser(t, application)
	body := b.getBody(t)
	if !strings.Contains(body, `id="activity-console"`) || !strings.Contains(body, html.EscapeString(message)) || strings.Contains(body, "not-for-console") || strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatal("console omitted or failed to escape diagnostic text")
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:8080/activity", nil)
	response := httptest.NewRecorder()
	b.handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/plain; charset=utf-8" || response.Header().Get("Cache-Control") != "no-store" || !strings.Contains(response.Body.String(), message) || len(application.actions) != 0 {
		t.Fatalf("activity read returned an unsafe response or mutated state: %d %s", response.Code, response.Body.String())
	}
	request = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://evil.example/activity", nil)
	response = httptest.NewRecorder()
	b.handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatal("activity endpoint accepted a foreign host")
	}
}

func TestFoldControlsHaveUniqueStableIdentities(t *testing.T) {
	snapshot := app.Snapshot{
		Config:     validACS5Config(),
		Choices:    release.Choices{Tags: map[string]string{"fact": "3.25.1"}},
		Assessment: release.Assessment{Conditions: []release.Condition{{Name: "Selected changes", Reason: "Waiting"}}},
		History:    []app.Cycle{{Component: "fact", Tag: "3.25.0"}},
	}
	for _, source := range snapshot.Config.Components {
		repo := release.Repository{Source: source}
		for i := range 2 {
			repo.Tags = append(repo.Tags, release.Tag{Name: fmt.Sprintf("3.25.%d", i+1), SHA: "tag", Belongs: release.Yes, Evidence: release.Evidence{Known: true, At: time.Now()}})
			repo.PRs = append(repo.PRs, release.PR{Target: release.Target{Repo: source.Repo, PR: i + 1}, State: "open"})
		}
		snapshot.Repositories = append(snapshot.Repositories, repo)
	}
	body := newBrowser(t, &fakeApplication{snapshot: snapshot}).getBody(t)
	if !strings.Contains(body, `href="https://github.com/acme/collector/tree/refs/tags/3.25.1"`) ||
		strings.Contains(body, `data-fold-key="tag-evidence:collector:acme/collector@3.25.1"`) {
		t.Fatal("candidate tag should link directly to GitHub without an evidence disclosure")
	}
	seen := map[string]bool{}
	identity := regexp.MustCompile(`(?:id|data-fold-key)="([^"]+)"`)
	for _, section := range regexp.MustCompile(`<details\b[^>]*>`).FindAllString(body, -1) {
		match := identity.FindStringSubmatch(section)
		if len(match) != 2 {
			t.Fatalf("fold control has no stable identity: %s", section)
		}
		key := html.UnescapeString(match[1])
		if strings.HasPrefix(match[0], `id="`) && !strings.HasSuffix(section, " open>") {
			t.Errorf("topic panel %q should start open", key)
		}
		if seen[key] {
			t.Errorf("duplicate fold identity %q", key)
		}
		seen[key] = true
	}
	for _, key := range []string{
		"previous-cycles",
		"tag-evidence:fact:acme/fact@3.25.1",
	} {
		if !seen[key] {
			t.Errorf("missing stable fold identity %q", key)
		}
	}
}

func TestActivityErrorsKeepSummaryAndPerCheckDetailsTogether(t *testing.T) {
	message := "PipelineRun identity verification failed for 2 checks (2 API/archive failures):\ncheck build-a (run 17): API: HTTP 403 Forbidden; archive: HTTP 404 Not Found\ncheck build-b (run 18): dial tcp: lookup cluster.example: no such host"
	snapshot := app.Snapshot{LogEntries: []app.Record{
		{Decision: "checks-error", Target: release.Target{Repo: "stackrox/stackrox", PR: 24065}, Result: message},
		{Decision: "github-error", Result: "Error: request timed out"},
		{Decision: "checks-recovered", Result: "Reads recovered."},
	}}
	text := activityText(snapshot)
	for _, want := range []string{
		"Error: Could not read build results in stackrox#24065. PipelineRun identity verification failed for 2 checks",
		"\n  check build-a (run 17): API: HTTP 403 Forbidden; archive: HTTP 404 Not Found\n  check build-b (run 18): dial tcp: lookup cluster.example: no such host\n",
		"Error: Could not read pull requests and release tags in the release. Error: request timed out",
		"Info: Reading build results in the release is working again.",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in activity:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Error: Error:") {
		t.Fatalf("duplicated severity prefix: %s", text)
	}
}

func TestPartialTagInventoryShowsVerifiedChoicesImmediately(t *testing.T) {
	config := validACS5Config()
	source := config.Components[0]
	tag := release.Tag{Name: source.Stream + ".1", SHA: "tag-head", Belongs: release.Yes, Evidence: release.Evidence{At: time.Now(), Known: true}}
	snapshot := app.Snapshot{Config: config, Repositories: []release.Repository{{Source: source, Head: "branch-head", Tags: []release.Tag{tag}, TagsPending: true, Evidence: release.Evidence{At: time.Now()}}}}
	model := makePage(snapshot, "csrf", "")
	if len(model.TagRepos) != len(config.Components) || len(model.TagRepos[0].Tags) != 1 || len(model.TagRepos[0].OtherTags) != 0 || model.TagRepos[0].Tags[0].Proposed {
		t.Fatal("partial tag choices are hidden or presented as a complete-inventory recommendation")
	}
	b := newBrowser(t, &fakeApplication{snapshot: snapshot})
	body := b.getBody(t)
	if !strings.Contains(body, "Confirm "+source.Name+" "+tag.Name) {
		t.Fatal("early verified tag cannot be selected")
	}
}

func TestSidebarReadBudget(t *testing.T) {
	now := time.Now()
	nextSlot := now.Add(time.Minute)
	reset := now.Add(50*time.Minute + 30*time.Second)
	application := &fakeApplication{snapshot: app.Snapshot{GitHubReadBudget: githubread.Budget{
		Limit: 3000, NextSlot: nextSlot,
		GitHub: githubread.Quota{Remaining: 1812, Limit: 5000, ObservedAt: now, ResetAt: reset},
	}}}
	body := newBrowser(t, application).getBody(t)
	_, after, found := strings.Cut(body, "<aside ")
	sidebar, _, closed := strings.Cut(after, "</aside>")
	if !found || !closed {
		t.Fatal("missing sidebar")
	}
	for _, want := range []string{
		"0 of 3000 reads left", "Next slot at " + formatTime(nextSlot),
		"1812 of 5000 requests left", "Resets in 51 minutes",
	} {
		if !strings.Contains(sidebar, want) {
			t.Errorf("sidebar missing %q", want)
		}
	}
	if strings.Contains(sidebar, "Observed at") || strings.Contains(sidebar, "Resets at") {
		t.Error("sidebar contains an absolute GitHub quota timestamp")
	}
	if got := quotaReset(time.Now().Add(30 * time.Second)); got != "Resets in 1 minute" {
		t.Errorf("final minute = %q", got)
	}
}

func TestGitHubReadLimitSettings(t *testing.T) {
	reads := githubread.New(nil)
	cache := github.NewCommitWindowCache(nil)
	session := app.New(nil, nil, nil, dispatch.New(nil, nil, nil, dispatch.Options{}), nil, app.Options{GitHubReads: reads, Cache: cache})
	b := newBrowser(t, session)
	if body := b.getBody(t); !strings.Contains(body, "3000 of 3000 reads left") || !strings.Contains(body, "<dd>Unknown</dd>") {
		t.Fatal("sidebar should show the local allowance and an unknown GitHub quota before the first read")
	}
	checkLimits := func(startup, ongoing int) {
		t.Helper()
		if gotStartup, gotOngoing := reads.ReadLimits(); gotStartup != startup || gotOngoing != ongoing {
			t.Fatalf("transport limits = %d, %d, want %d, %d", gotStartup, gotOngoing, startup, ongoing)
		}
		body := b.getBody(t)
		for _, field := range []string{
			fmt.Sprintf(`name="startup_read_limit" min="1" max="5" required value="%d"`, startup),
			fmt.Sprintf(`name="ongoing_read_limit" min="1" max="3" required value="%d"`, ongoing),
		} {
			if !strings.Contains(body, field) {
				t.Errorf("settings form missing %s", field)
			}
		}
	}
	checkLimits(3, 1)
	values := url.Values{"kind": {"application-settings"}, "tab": {"settings"}, "generation": {fmt.Sprint(session.Snapshot().Generation)}}
	save := func() {
		t.Helper()
		if response := b.post(values); response.Code != http.StatusSeeOther {
			t.Fatalf("save settings: HTTP %d", response.Code)
		}
	}
	values.Set("startup_read_limit", "5")
	save()
	checkLimits(5, 1)
	values.Del("startup_read_limit")
	values.Set("ongoing_read_limit", "3")
	save()
	checkLimits(5, 3)

	values.Set("cache", "false")
	for _, tc := range []struct{ startup, ongoing []string }{
		{[]string{"0"}, []string{"1"}}, {[]string{"6"}, []string{"1"}},
		{[]string{"1"}, []string{"0"}}, {[]string{"1"}, []string{"4"}},
		{[]string{"1"}, []string{""}}, {[]string{"2.5"}, []string{"2"}},
		{[]string{"1"}, []string{"1", "2"}},
	} {
		values["startup_read_limit"], values["ongoing_read_limit"] = tc.startup, tc.ongoing
		if response := b.post(values); response.Code < 400 || !cache.Enabled() {
			t.Fatalf("invalid limits %+v applied settings", tc)
		}
		checkLimits(5, 3)
	}
	values.Set("startup_read_limit", "1")
	values.Set("ongoing_read_limit", "2")
	save()
	checkLimits(1, 2)
}

func TestReleaseHomeAndDraftNavigation(t *testing.T) {
	state, err := app.OpenReleaseState(filepath.Join(t.TempDir(), "state.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	session := app.New(nil, nil, nil, dispatch.New(nil, nil, nil, dispatch.Options{}), nil, app.Options{Profiles: true, ReleaseState: state})
	handler, err := New(session)
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://localhost"+path, nil))
		if response.Code != http.StatusOK || !strings.HasSuffix(response.Body.String(), "</html>\n") {
			t.Fatalf("GET %s: %d %s", path, response.Code, response.Body.String())
		}
		return response
	}
	home := get("/")
	cookie := home.Result().Cookies()[0]
	if !strings.Contains(home.Body.String(), "Tracked releases") || strings.Contains(home.Body.String(), "Remove release") {
		t.Fatal("empty Home is missing its release manager")
	}
	post := func(values url.Values) *httptest.ResponseRecorder {
		t.Helper()
		values.Set("csrf", cookie.Value)
		request := httptest.NewRequest(http.MethodPost, "http://localhost/action", strings.NewReader(values.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Origin", "http://localhost")
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	if response := post(url.Values{"kind": {"add-release"}}); response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/" {
		t.Fatalf("add release = %d %s", response.Code, response.Body.String())
	}
	saved, active, _ := session.ReleaseProfiles()
	if len(saved) != 1 || active != "" {
		t.Fatalf("saved = %+v, active = %s", saved, active)
	}
	id := saved[0].ID
	if response := post(url.Values{"kind": {"open-release"}, "profile": {id}}); response.Code != http.StatusSeeOther {
		t.Fatalf("open release = %d %s", response.Code, response.Body.String())
	}
	draft := get(profilePageURL(id, "acs-rcs"))
	if !strings.Contains(draft.Body.String(), "Unconfigured release</h1>") ||
		!strings.Contains(draft.Body.String(), "Configure this release</a> to start monitoring.") {
		t.Fatal("draft does not expose normal navigation and setup")
	}
}
