package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"release-assistant/internal/app"
	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
	"release-assistant/internal/web"
)

type retestQueueFixture struct {
	pr     release.PR
	posted []string
}

func (f *retestQueueFixture) Discover(_ context.Context, request app.DiscoveryRequest) (release.Repository, error) {
	return release.Repository{Source: request.Source, Head: "branch-head", Evidence: observation(), PRs: []release.PR{f.pr}}, nil
}

func (f *retestQueueFixture) Inspect(context.Context, release.Config, map[string]string) (release.Inventory, error) {
	return release.Inventory{}, nil
}

func (f *retestQueueFixture) InspectReference(context.Context, string) (release.Image, error) {
	return release.Image{}, nil
}

func (f *retestQueueFixture) Plan(_ context.Context, target release.Target) (release.Plan, error) {
	plan := release.Plan{Target: target, SHA: f.pr.SHA, Evidence: observation(), Action: "Retest"}
	for _, job := range []string{"central-db-on-push", "retag-scanner"} {
		plan.Proposals = append(plan.Proposals, release.Proposal{
			Target: target, SHA: plan.SHA, Job: job, RunID: "run-" + job, Payload: "/konflux-retest " + job,
		})
	}
	return plan, nil
}

func (f *retestQueueFixture) ReadTarget(_ context.Context, target release.Target) (dispatch.Identity, error) {
	return dispatch.Identity{
		Target: target, SHA: f.pr.SHA, Base: f.pr.Base, Open: true,
		CommentLabel: f.pr.CommentLabel, OptOut: f.pr.OptOut, Evidence: observation(),
	}, nil
}

func (f *retestQueueFixture) Write(_ context.Context, proposal release.Proposal) (dispatch.Delivery, error) {
	f.posted = append(f.posted, proposal.Payload)
	return dispatch.Delivery{Status: "sent"}, nil
}

type retestQueueBrowser struct {
	handler http.Handler
	cookie  *http.Cookie
	session *app.Session
}

func openRetestQueue(t *testing.T, session *app.Session) retestQueueBrowser {
	t.Helper()
	handler, err := web.New(session)
	if err != nil {
		t.Fatal(err)
	}
	browser := retestQueueBrowser{handler: handler, session: session}
	cookies := browser.get(t).Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("Retest Queue did not set the form security cookie")
	}
	browser.cookie = cookies[0]
	return browser
}

func (b retestQueueBrowser) get(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://localhost/?tab=retest-queue", nil)
	w := httptest.NewRecorder()
	b.handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET Retest Queue returned %d", w.Code)
	}
	return w
}

func (b retestQueueBrowser) send(t *testing.T, id uint64) int {
	t.Helper()
	const origin = "http://localhost"
	values := url.Values{
		"kind": {"queue-send"}, "tab": {"retest-queue"}, "queue_id": {strconv.FormatUint(id, 10)},
		"csrf": {b.cookie.Value}, "generation": {strconv.FormatUint(b.session.Snapshot().Generation, 10)},
	}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, origin+"/action", strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", origin)
	r.AddCookie(b.cookie)
	w := httptest.NewRecorder()
	b.handler.ServeHTTP(w, r)
	return w.Code
}

func newRetestQueueSession(t *testing.T, options dispatch.Options) (*app.Session, *retestQueueFixture, release.Source) {
	t.Helper()
	config := release.Config{Version: "5.0.1", Branch: "release-5.0", Components: []release.Source{
		{Name: "fact", Repo: "stackrox/fact", Branch: "release-0.4", Stream: "0.4"},
		{Name: "collector", Repo: "stackrox/collector", Branch: "release-3.26", Stream: "3.26"},
	}}
	source := config.ACS()
	fixture := &retestQueueFixture{pr: release.PR{
		Target: release.Target{Repo: source.Repo, PR: 23986}, SHA: "pr-head", State: "open", Base: source.Branch,
		Title: "Verify queued retry comments", Evidence: observation(),
	}}
	dispatcher := dispatch.New(fixture, fixture, fixture, options)
	session := app.New(fixture, fixture, fixture, dispatcher, nil, app.Options{Initial: config})
	refreshRetestQueue(t, session, source)
	applyRetestAction(t, session, app.Action{Kind: "choice", Target: fixture.pr.Target, SHA: fixture.pr.SHA, Choice: release.Included})
	return session, fixture, source
}

func applyRetestAction(t *testing.T, session *app.Session, action app.Action) {
	t.Helper()
	action.Generation = session.Snapshot().Generation
	if err := session.Apply(t.Context(), action); err != nil {
		t.Fatal(err)
	}
}

func refreshRetestQueue(t *testing.T, session *app.Session, source release.Source) {
	t.Helper()
	if err := session.Refresh(t.Context(), session.Snapshot().Generation, source, true); err != nil {
		t.Fatal(err)
	}
}

func TestRetestQueueShowsBlockedCommentsAcrossWriteModes(t *testing.T) {
	for name, test := range map[string]struct {
		writes, interactive, requireLabel, grant, optOut bool
		blocker                                          string
	}{
		"read-only":             {interactive: true, blocker: "writes disabled"},
		"interactive and label": {writes: true, interactive: true, requireLabel: true, blocker: "allow-release-assistant"},
		"automatic":             {writes: true, blocker: "Automatic posting permission required"},
		"automatic and label":   {writes: true, requireLabel: true, grant: true, blocker: "allow-release-assistant"},
		"PR opts out":           {writes: true, interactive: true, optOut: true, blocker: "no-release-assistant disables writes"},
	} {
		t.Run(name, func(t *testing.T) {
			session, fixture, source := newRetestQueueSession(t, dispatch.Options{EnableWrites: test.writes})
			fixture.pr.OptOut = test.optOut
			if test.writes {
				applyRetestAction(t, session, app.Action{
					Kind: "application-settings", Interactive: &test.interactive, RequirePRLabel: &test.requireLabel,
				})
			}
			if test.grant {
				applyRetestAction(t, session, app.Action{Kind: "grant", Target: fixture.pr.Target, SHA: fixture.pr.SHA, Enabled: true})
			}
			refreshRetestQueue(t, session, source)
			browser := openRetestQueue(t, session)
			items := session.Snapshot().RetestQueue
			body := browser.get(t).Body.String()
			if len(items) != 2 || strings.Count(body, "data-retest-row=") != 2 {
				t.Fatalf("want two visible retry comments, got %+v", items)
			}
			wantDisabled := 0
			if test.writes && test.interactive {
				wantDisabled = len(items)
			}
			if count := strings.Count(body, `<button type="submit" disabled>Send</button>`); count != wantDisabled {
				t.Errorf("disabled Send controls = %d, want %d", count, wantDisabled)
			}
			if !test.writes && strings.Contains(body, `name="kind" value="queue-send"`) {
				t.Error("Read-only queue rendered a Send form")
			}
			for _, item := range items {
				if !strings.Contains(item.Blocked, test.blocker) || !strings.Contains(body, item.Blocked) {
					t.Fatalf("queue did not show blocker %q for %+v", test.blocker, item)
				}
				if !test.writes {
					blockerID := "retest-blocker-" + strconv.FormatUint(item.ID, 10)
					if !strings.Contains(body, `<button type="button" disabled aria-describedby="`+blockerID+`">Send</button>`) ||
						!strings.Contains(body, `id="`+blockerID+`" class="retest-job muted">`+item.Blocked+`</small>`) {
						t.Errorf("Read-only queue item %d is missing its disabled Send button or reason", item.ID)
					}
				}
				if status := browser.send(t, item.ID); status != http.StatusConflict {
					t.Errorf("blocked Send returned %d", status)
				}
			}
			if len(fixture.posted) != 0 {
				t.Fatalf("blocked comments reached the writer: %v", fixture.posted)
			}
		})
	}
}

func TestInteractiveRetestQueueRequiresASeparateSendForEachComment(t *testing.T) {
	now := time.Now()
	session, fixture, source := newRetestQueueSession(t, dispatch.Options{EnableWrites: true, Now: func() time.Time { return now }})
	refreshRetestQueue(t, session, source)
	browser := openRetestQueue(t, session)
	initial := session.Snapshot().RetestQueue
	if len(initial) != 2 || len(fixture.posted) != 0 ||
		strings.Count(browser.get(t).Body.String(), `<button type="submit">Send</button>`) != 2 {
		t.Fatalf("expected two unconfirmed rows and no writes: %+v, %v", initial, fixture.posted)
	}
	first, second := initial[0], initial[1]
	if status := browser.send(t, first.ID); status != http.StatusSeeOther || !slices.Equal(fixture.posted, []string{first.Payload}) {
		t.Fatalf("first Send returned %d and posted %v", status, fixture.posted)
	}
	remaining := session.Snapshot().RetestQueue
	if len(remaining) != 1 || remaining[0].ID != second.ID || !strings.Contains(remaining[0].Blocked, "rate limit") {
		t.Fatalf("second retry should remain visible during cooldown: %+v", remaining)
	}
	if status := browser.send(t, first.ID); status != http.StatusConflict {
		t.Fatalf("replayed approval returned %d", status)
	}
	now = now.Add(2 * time.Minute)
	refreshRetestQueue(t, session, source)
	if len(fixture.posted) != 1 {
		t.Fatalf("the second retry posted without confirmation: %v", fixture.posted)
	}
	if status := browser.send(t, second.ID); status != http.StatusSeeOther ||
		!slices.Equal(fixture.posted, []string{first.Payload, second.Payload}) {
		t.Fatalf("second Send returned %d and posted %v", status, fixture.posted)
	}
	if remaining := session.Snapshot().RetestQueue; len(remaining) != 0 {
		t.Fatalf("confirmed retries remain in the queue: %+v", remaining)
	}
}

func TestAutomaticRetestQueueShowsPendingCommentsAndUsesBackgroundDispatch(t *testing.T) {
	session, fixture, source := newRetestQueueSession(t, dispatch.Options{EnableWrites: true})
	applyRetestAction(t, session, app.Action{Kind: "interactive", Enabled: false})
	refreshRetestQueue(t, session, source)
	applyRetestAction(t, session, app.Action{Kind: "grant", Target: fixture.pr.Target, SHA: fixture.pr.SHA, Enabled: true})
	browser := openRetestQueue(t, session)
	items := session.Snapshot().RetestQueue
	body := browser.get(t).Body.String()
	if len(items) != 2 || strings.Count(body, "Waiting for automatic posting.") != 2 ||
		strings.Contains(body, `name="kind" value="queue-send"`) {
		t.Fatalf("expected two pending automatic retries and no Send controls: %+v", items)
	}
	if status := browser.send(t, items[0].ID); status != http.StatusConflict || len(fixture.posted) != 0 {
		t.Fatalf("manual approval in automatic mode returned %d and posted %v", status, fixture.posted)
	}
	refreshRetestQueue(t, session, source)
	remaining := session.Snapshot().RetestQueue
	if len(fixture.posted) != 1 || len(remaining) != 1 || !strings.Contains(remaining[0].Blocked, "rate limit") {
		t.Fatalf("automatic polling should post one comment and show the remaining blocked retry: %v, %+v", fixture.posted, remaining)
	}
}
