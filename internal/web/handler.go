// Package web renders the local release session and converts forms to typed actions.
package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"release-assistant/internal/app"
	"release-assistant/internal/release"
)

//go:embed templates/*.html static/*
var assets embed.FS

const (
	retryCommentPermissionLabel = "Allow automatic retry comments"
	maxFormBytes                = 64 << 10
	csrfCookie                  = "release_assistant_csrf"
)

type (
	// Application is the only session boundary used by the browser.
	Application interface {
		Snapshot() app.Snapshot
		Apply(context.Context, app.Action) error
	}
	profileApplication interface {
		ProfilesEnabled() bool
		ReleaseProfiles() ([]app.ReleaseProfile, string, []string)
		ProfileSnapshot(string) (app.Snapshot, bool)
	}
	server struct {
		app      Application
		template *template.Template
		csrf     [32]byte
	}
)

// New constructs the local HTML handler and its per-process CSRF secret.
func New(application Application) (http.Handler, error) {
	if application == nil {
		return nil, errors.New("web application is required")
	}
	page, err := template.New("index.html").Funcs(template.FuncMap{
		"prCommentLabel":              func() string { return release.PRCommentLabel },
		"manualRetryWarning":          func() string { return release.ManualRetryWarning },
		"ciRetryDiagnostics":          ciRetryDiagnostics,
		"konfluxRetryDiagnostics":     konfluxRetryDiagnostics,
		"retryCommentPermissionLabel": func() string { return retryCommentPermissionLabel },
		"pinVersion":                  pinVersion, "pinDiffers": pinDiffers,
		"tabPage": tabPage, "tabTitle": tabTitle, "tabHelp": tabHelp, "tabStep": tabStep,
		"truth": truthLabel, "when": formatTime, "quotaReset": quotaReset, "url": safeURL,
		"activity": activityText, "commentURL": commentURL, "profilePage": profilePageURL,
		"profileAction": func(id string) string {
			if id == "" {
				return "/action"
			}
			return "/action?release=" + url.QueryEscape(id)
		},
		"releaseLink": func(p pageModel, profile app.ReleaseProfile) any {
			return struct {
				Page    pageModel
				Profile app.ReleaseProfile
			}{p, profile}
		},
		"refreshSections": refreshSections, "wholeMinutes": wholeMinutes,
		"slice": func(values ...string) []string { return values },
		"title": func(value string) string {
			if value == "acs" {
				return "ACS"
			}
			if value == "" {
				return value
			}
			return strings.ToUpper(value[:1]) + value[1:]
		},
	}).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse embedded page template: %w", err)
	}
	s := &server{app: application, template: page}
	if _, err := rand.Read(s.csrf[:]); err != nil {
		return nil, fmt.Errorf("initialize form protection")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.get)
	mux.HandleFunc("GET /activity", s.getActivity)
	mux.HandleFunc("POST /action", s.post)
	mux.HandleFunc("GET /static/app.css", serveAsset("static/app.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /static/external-link.svg", serveAsset("static/external-link.svg", "image/svg+xml"))
	mux.HandleFunc("GET /static/app.js", serveAsset("static/app.js", "text/javascript; charset=utf-8"))
	for _, family := range []string{"redhattext", "redhatdisplay", "inconsolata"} {
		name := "static/fonts/" + family + ".ttf"
		mux.HandleFunc("GET /"+name, serveAsset(name, "font/ttf"))
	}
	return s.localOnly(mux), nil
}

func serveAsset(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		content, err := fs.ReadFile(assets, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(content)
	}
}

func (s *server) localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			http.Error(w, "local requests only", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost && (!sameOrigin(r) || strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site")) {
			http.Error(w, "same-origin form submission required", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func loopbackHost(hostport string) bool {
	host := hostport
	if strings.Contains(hostport, ":") {
		if h, _, err := net.SplitHostPort(hostport); err == nil {
			host = h
		} else if strings.HasPrefix(hostport, "[") && strings.HasSuffix(hostport, "]") {
			host = strings.Trim(hostport, "[]")
		} else if strings.Count(hostport, ":") == 1 {
			host = strings.SplitN(hostport, ":", 2)[0]
		}
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" || origin == "null" {
		return strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "same-origin")
	}
	u, err := url.Parse(origin)
	return err == nil && u.Scheme == "http" && u.Host == r.Host && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == "" && loopbackHost(u.Host)
}

func (s *server) get(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	token := fmt.Sprintf("%x", s.csrf[:])
	http.SetCookie(w, &http.Cookie{Name: csrfName(r.Host), Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 3600})
	page := s.makePage(token, "", r.URL.Query().Get("release"), r.URL.Query().Get("tab"))
	page.ConfigSaved = page.Configured && page.Tab == "settings" && r.URL.Query().Get("saved") == "configuration"
	if page.Tab == "settings" {
		saved := r.URL.Query().Get("saved")
		if saved == "application" {
			page.SettingsSaved = saved
		} else {
			page.SettingsSaved = settingsSection(saved)
		}
	}
	page.CacheCleared = page.Snapshot.CacheAvailable && page.Tab == "settings" && r.URL.Query().Get("saved") == "cache-clear"
	if err := s.template.Execute(w, page); err != nil {
		http.Error(w, "page rendering failed", http.StatusInternalServerError)
	}
}

func (s *server) getActivity(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(activityText(s.app.Snapshot())))
}

func activityText(snapshot app.Snapshot) string {
	var text strings.Builder
	for _, entry := range snapshot.LogEntries {
		fmt.Fprintf(&text, "%s %s: %s\n", formatTime(entry.At), entry.Level(), strings.ReplaceAll(entry.DisplayMessage(), "\n", "\n  "))
	}
	if snapshot.LogError != "" {
		fmt.Fprintf(&text, "[log-error] Error: %s\n", strings.ReplaceAll(snapshot.LogError, "\n", "\n  "))
	}
	if text.Len() == 0 {
		if snapshot.Config.Validate() != nil {
			return "Waiting for release configuration."
		}
		return "Waiting for the first monitoring results."
	}
	return text.String()
}

func (s *server) post(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if !s.validCSRF(r) {
		http.Error(w, "form token is invalid", http.StatusForbidden)
		return
	}
	if r.PostForm.Get("kind") == "queue-send" && r.Header.Get("Accept") == "application/json" {
		s.sendQueue(w, r)
		return
	}
	profileID := r.PostForm.Get("profile")
	if profileID == "" {
		profileID = r.URL.Query().Get("release")
	}
	page := s.makePage("", "", profileID, r.PostForm.Get("tab"))
	snapshot := page.Snapshot
	current := snapshot.Config
	if snapshot.PendingPinningPR > 0 {
		current.PinningPR = snapshot.PendingPinningPR
	}
	action, err := parseAction(r, current)
	if err != nil {
		s.renderError(w, r, err, http.StatusBadRequest)
		return
	}
	action.ProfileID = page.ProfileID
	if action.Kind == "add-release" || action.Kind == "open-release" || action.Kind == "remove-release" {
		action.ProfileID = profileID
	}
	if err := s.app.Apply(r.Context(), action); err != nil {
		s.renderError(w, r, err, http.StatusConflict)
		return
	}
	destination := "/"
	if tab := r.PostForm.Get("tab"); tab != "" {
		destination = "/?tab=" + validTab(tab)
	}
	switch action.Kind {
	case "add-release", "remove-release":
		destination = "/"
	case "open-release":
		destination = profilePageURL(profileID, "")
	case "configure":
		destination = "/?tab=settings&saved=configuration#configuration-panel"
	case "application-settings":
		if section := settingsSection(r.PostForm.Get("settings_section")); section != "" {
			destination = "/?tab=settings&saved=" + section + "#" + section
		} else {
			destination = "/?tab=settings&saved=application#retry-settings"
		}
	case "clear-cache":
		destination = "/?tab=settings&saved=cache-clear#cache-settings"
	case "check-pr-retries":
		destination += "#" + prRetryAnchor(action.Target)
	}
	if page.ProfileID != "" && action.Kind != "open-release" && action.Kind != "remove-release" && action.Kind != "add-release" {
		location, err := url.Parse(destination)
		if err != nil {
			http.Error(w, "invalid redirect", http.StatusInternalServerError)
			return
		}
		query := location.Query()
		query.Set("release", page.ProfileID)
		location.RawQuery = query.Encode()
		destination = location.String()
	}
	http.Redirect(w, r, destination, http.StatusSeeOther)
}

// sendQueue dispatches only the IDs captured by the click, with a result for each row.
func (s *server) sendQueue(w http.ResponseWriter, r *http.Request) {
	action, err := parseAction(r, release.Config{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ids := make(map[uint64]struct{}, len(r.PostForm["queue_id"]))
	for _, value := range r.PostForm["queue_id"] {
		id, err := strconv.ParseUint(value, 10, 64)
		if err != nil || id == 0 {
			http.Error(w, "a valid queue item ID is required", http.StatusBadRequest)
			return
		}
		ids[id] = struct{}{}
	}
	results := make(map[uint64]string, len(ids))
	var sends sync.WaitGroup
	var resultsMu sync.Mutex
	for id := range ids {
		sends.Go(func() {
			a := action
			a.QueueID = id
			var message string
			if err := s.app.Apply(r.Context(), a); err != nil {
				message = err.Error()
			}
			resultsMu.Lock()
			results[id] = message
			resultsMu.Unlock()
		})
	}
	sends.Wait()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(results)
}

func (s *server) validCSRF(r *http.Request) bool {
	cookie, err := r.Cookie(csrfName(r.Host))
	if err != nil {
		return false
	}
	form := r.FormValue("csrf")
	secret := fmt.Sprintf("%x", s.csrf[:])
	return len(form) == len(secret) && subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(secret)) == 1 && subtle.ConstantTimeCompare([]byte(form), []byte(secret)) == 1
}

func (s *server) renderError(w http.ResponseWriter, r *http.Request, cause error, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	profileID := r.PostForm.Get("profile")
	if profileID == "" {
		profileID = r.URL.Query().Get("release")
	}
	if r.PostForm.Get("kind") == "add-release" || r.PostForm.Get("kind") == "remove-release" || r.PostForm.Get("kind") == "open-release" {
		profileID = ""
	}
	page := s.makePage(fmt.Sprintf("%x", s.csrf[:]), cause.Error(), profileID, r.PostForm.Get("tab"))
	if r.PostForm.Get("kind") == "configure" {
		// Rejected text remains a page-local draft, separate from the active configuration.
		page.ConfigRejected = true
		page.ConfigForm = r.PostForm
		setComponentBranches(page.ConfigForm, page.Snapshot.Config)
		page.Tab = "settings"
		page.ACS4 = strings.HasPrefix(r.PostForm.Get("version"), "4.")
	}
	_ = s.template.Execute(w, page)
}

func profilePageURL(id, tab string) string {
	q := make(url.Values)
	if id != "" {
		q.Set("release", id)
	}
	if tab != "" {
		q.Set("tab", tab)
	}
	if len(q) == 0 {
		return "/"
	}
	return "/?" + q.Encode()
}

func (s *server) makePage(token, message, profileID, tab string) pageModel {
	if profiles, ok := s.app.(profileApplication); ok && profiles.ProfilesEnabled() {
		saved, active, warnings := profiles.ReleaseProfiles()
		if profileID == "" && tab != "" {
			profileID = active
		}
		var snapshot app.Snapshot
		var found bool
		if profileID != "" {
			snapshot, found = profiles.ProfileSnapshot(profileID)
		}
		home := profileID == "" && tab == ""
		paused := profileID != "" && !found
		if home || paused || profileID == "" {
			snapshot = s.app.Snapshot()
		}
		page := makePage(snapshot, token, message)
		page.Profiles, page.Home, page.Paused = true, home, paused
		page.ProfileID, page.ActiveProfile = profileID, active
		page.ReleaseProfiles, page.ProfileWarnings = saved, warnings
		page.Tab = validTab(tab)
		if tab == "" && !page.Configured {
			page.Tab = "settings"
		}
		return page
	}
	page := makePage(s.app.Snapshot(), token, message)
	page.Tab = validTab(tab)
	if !page.Configured && page.Tab != "activity" && page.Tab != "retest-queue" {
		page.Tab = "settings"
	}
	return page
}

func parseAction(r *http.Request, current release.Config) (app.Action, error) {
	form := r.Form
	a := app.Action{Kind: form.Get("kind"), Target: release.Target{Repo: form.Get("repo")}, SHA: form.Get("sha"), ID: form.Get("id")}
	switch a.Kind {
	case "add-release", "open-release", "remove-release":
		return a, nil
	case "configure":
		if form.Has("generation") {
			generation, err := strconv.ParseUint(form.Get("generation"), 10, 64)
			if err != nil {
				return a, fmt.Errorf("page generation is invalid")
			}
			a.Generation = generation
		}
		version := form.Get("version")
		major, _, _, ok := release.Version(version)
		if !ok {
			return a, fmt.Errorf("ACS version must be a final semantic patch version")
		}
		config := release.Config{Version: version, Branch: form.Get("branch")}
		if form.Get("pinning_pr") != "" {
			n, err := strconv.Atoi(form.Get("pinning_pr"))
			if err != nil {
				return a, fmt.Errorf("pinning PR must be a number, for example 12345 (not a URL)")
			}
			if n < 0 {
				return a, fmt.Errorf("pinning PR must be nonnegative")
			}
			config.PinningPR = n
		}
		if form.Get("detect_pinning_pr") == "true" {
			config.DetectPinningPR = true
			config.ManualPinningPR = config.PinningPR
			config.PinningPR = 0
		}
		// An unchanged field still describes the release shown when the form was opened.
		if current.Validate() == nil &&
			(strings.TrimPrefix(version, "v") != strings.TrimPrefix(current.Version, "v") || config.Branch != current.Branch) &&
			config.PinningPR == current.PinningPR && config.DetectPinningPR == current.DetectPinningPR && config.ManualPinningPR == current.ManualPinningPR {
			config.PinningPR, config.ManualPinningPR, config.DetectPinningPR = 0, 0, false
		}
		for _, name := range []string{"fact", "collector", "scanner"} {
			if name == "scanner" && major == 5 {
				continue
			}
			repo, stream := form.Get(name+"_repo"), form.Get(name+"_stream")
			config.Components = append(config.Components, release.Source{
				Name: name, Repo: repo, Stream: stream, Branch: componentBranch(current, name, repo, stream),
			})
		}
		a.Config = config
		return a, nil
	}
	gen, err := strconv.ParseUint(form.Get("generation"), 10, 64)
	if err != nil {
		return a, fmt.Errorf("page generation is missing or invalid")
	}
	a.Generation = gen
	if form.Get("pr") != "" {
		a.Target.PR, err = strconv.Atoi(form.Get("pr"))
		if err != nil || a.Target.PR < 1 {
			return a, fmt.Errorf("pull request number is invalid")
		}
	}
	a.Target.Tag = form.Get("tag")
	switch a.Kind {
	case "use-pinning-pr":
		if a.Target.PR < 1 || !a.Target.Valid() || a.SHA == "" {
			return a, fmt.Errorf("pinning PR selection requires the reviewed PR and SHA")
		}
	case "check-pr-retries":
		if a.Target.PR < 1 || !a.Target.Valid() || a.SHA == "" {
			return a, fmt.Errorf("retry check requires the displayed PR and SHA")
		}
	case "choice":
		a.Choice = release.Choice(lastFormValue(form, "choice"))
		if a.Target.PR < 1 || a.Target.Tag != "" || !release.ValidRepo(a.Target.Repo) {
			return a, fmt.Errorf("choice target is invalid")
		}
	case "component-pr-choices", "acs-pr-choices":
		a.Choice = release.Choice(lastFormValue(form, "choice"))
		tab := form.Get("tab")
		if a.Target != (release.Target{}) ||
			(a.Kind == "component-pr-choices" && tab != "component-prs") ||
			(a.Kind == "acs-pr-choices" && tab != "acs-prs") {
			return a, fmt.Errorf("bulk selection requires the matching PR list")
		}
	case "grant":
		if !a.Target.Valid() {
			return a, fmt.Errorf("grant target is invalid")
		}
		a.Enabled = formHasValue(form, "enabled", "true")
		if a.SHA == "" {
			return a, fmt.Errorf("grant requires the displayed SHA")
		}
	case "application-settings":
		a.RefreshIntervals, err = submittedIntervals(r.PostForm)
		if err != nil {
			return a, err
		}
		a.StartupReadLimit, err = readLimitSetting(r.PostForm, "startup_read_limit")
		if err != nil {
			return a, err
		}
		a.OngoingReadLimit, err = readLimitSetting(r.PostForm, "ongoing_read_limit")
		if err != nil {
			return a, err
		}
		a.Interactive, err = checkboxSetting(r.PostForm, "interactive")
		if err != nil {
			return a, err
		}
		a.RequirePRLabel, err = checkboxSetting(r.PostForm, "require_pr_label")
		if err != nil {
			return a, err
		}
		a.CacheEnabled, err = checkboxSetting(r.PostForm, "cache")
		if err != nil {
			return a, err
		}
	case "interactive", "cache-enabled":
		a.Enabled = formHasValue(form, "enabled", "true")
	case "clear-cache":
	case "clear-tag":
		a.Component = form.Get("component")
		if a.Component == "" {
			return a, fmt.Errorf("component is required to prepare a new tag")
		}
	case "tag":
		a.Component = form.Get("component")
		a.Tag = form.Get("tag")
		if a.Component == "" || a.Tag == "" || a.SHA == "" {
			return a, fmt.Errorf("tag confirmation requires component, tag, and SHA")
		}
	case "acknowledge":
		a.Enabled = formHasValue(form, "enabled", "true")
		if a.ID == "" {
			return a, fmt.Errorf("advisory identity is missing")
		}
	case "queue-send", "queue-skip":
		a.QueueID, err = strconv.ParseUint(form.Get("queue_id"), 10, 64)
		if err != nil || a.QueueID == 0 {
			return a, fmt.Errorf("a valid queue item ID is required")
		}
	default:
		return a, fmt.Errorf("unknown form action")
	}
	return a, nil
}

func truthLabel(value release.Truth) string {
	if value == "" {
		return "unknown"
	}
	return string(value)
}

func readLimitSetting(form url.Values, field string) (*int, error) {
	values, ok := form[field]
	if !ok {
		return nil, nil
	}
	if len(values) != 1 {
		return nil, fmt.Errorf("submit one value for each GitHub read limit")
	}
	limit, err := strconv.Atoi(values[0])
	if err != nil {
		return nil, fmt.Errorf("GitHub read limits must be whole numbers")
	}
	return &limit, nil
}

func checkboxSetting(form url.Values, field string) (*bool, error) {
	if !form.Has(field) {
		return nil, nil
	}
	switch lastFormValue(form, field) {
	case "true":
		return new(true), nil
	case "false":
		return new(false), nil
	default:
		return nil, fmt.Errorf("invalid %s setting", field)
	}
}

func lastFormValue(values url.Values, name string) string {
	items := values[name]
	if len(items) == 0 {
		return ""
	}
	return items[len(items)-1]
}

func formHasValue(values url.Values, name, want string) bool {
	for _, value := range values[name] {
		if value == want {
			return true
		}
	}
	return false
}
func formatTime(t time.Time) string {
	if t.IsZero() {
		return "time unavailable"
	}
	return t.Local().Format("2006-01-02 15:04:05 MST")
}

func quotaReset(at time.Time) string {
	minutes := int(math.Ceil(time.Until(at).Minutes()))
	switch {
	case minutes <= 0:
		return "Reset due"
	case minutes == 1:
		return "Resets in 1 minute"
	default:
		return fmt.Sprintf("Resets in %d minutes", minutes)
	}
}

func safeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return u.String()
}

// csrfName keeps parallel loopback instances from overwriting each other's cookies.
func csrfName(host string) string {
	sum := sha256.Sum256([]byte(host))
	return fmt.Sprintf("%s_%x", csrfCookie, sum[:6])
}
