// Package web renders the local release session and converts forms to typed actions.
package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"release-assistant/internal/app"
	"release-assistant/internal/release"
)

//go:embed templates/*.html static/*
var assets embed.FS

const (
	maxFormBytes = 64 << 10
	csrfCookie   = "release_assistant_csrf"
)

type (
	// Application is the only session boundary used by the browser.
	Application interface {
		Snapshot() app.Snapshot
		Apply(context.Context, app.Action) error
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
		"pinVersion": pinVersion, "pinDiffers": pinDiffers,
		"tabPage": tabPage, "tabTitle": tabTitle, "tabHelp": tabHelp, "tabStep": tabStep,
		"truth": truthLabel, "when": formatTime, "url": safeURL,
		"activity": activityText, "commentURL": commentURL,
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
	mux.HandleFunc("GET /static/app.js", serveAsset("static/app.js", "text/javascript; charset=utf-8"))
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
	page := makePage(s.app.Snapshot(), token, "")
	page.Tab = validTab(r.URL.Query().Get("tab"))
	if !page.Configured && page.Tab != "activity" {
		page.Tab = "settings"
	}
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
		fmt.Fprintf(&text, "%s %s: %s\n", formatTime(entry.At), entry.Level(), strings.ReplaceAll(entry.Message(), "\n", "\n  "))
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
	action, err := parseAction(r, s.app.Snapshot().Config)
	if err != nil {
		s.renderError(w, r, err, http.StatusBadRequest)
		return
	}
	if err := s.app.Apply(r.Context(), action); err != nil {
		s.renderError(w, r, err, http.StatusConflict)
		return
	}
	destination := "/"
	if tab := r.PostForm.Get("tab"); tab != "" {
		destination = "/?tab=" + validTab(tab)

	}
	http.Redirect(w, r, destination, http.StatusSeeOther)
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
	page := makePage(s.app.Snapshot(), fmt.Sprintf("%x", s.csrf[:]), cause.Error())
	page.Tab = validTab(r.PostForm.Get("tab"))
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

func parseAction(r *http.Request, current release.Config) (app.Action, error) {
	form := r.Form
	a := app.Action{Kind: form.Get("kind"), Target: release.Target{Repo: form.Get("repo")}, SHA: form.Get("sha"), ID: form.Get("id")}
	if a.Kind == "configure" {
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
			config.PinningPR = n
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
	case "choice":
		a.Choice = release.Choice(lastFormValue(form, "choice"))
		if a.Target.PR < 1 || a.Target.Tag != "" || !release.ValidRepo(a.Target.Repo) {
			return a, fmt.Errorf("choice target is invalid")
		}
	case "ignore-component-prs":
		if form.Get("tab") != "component-prs" || a.Target != (release.Target{}) {
			return a, fmt.Errorf("ignore all requires the Component PRs list")
		}
	case "grant":
		if !a.Target.Valid() {
			return a, fmt.Errorf("grant target is invalid")
		}
		a.Enabled = formHasValue(form, "enabled", "true")
		if a.SHA == "" {
			return a, fmt.Errorf("grant requires the displayed SHA")
		}
	case "interactive":
		a.Enabled = formHasValue(form, "enabled", "true")
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
	case "send", "reject":
		if !a.Target.Valid() || a.ID == "" || a.SHA == "" {
			return a, fmt.Errorf("proposal action requires target, SHA, and offer identity")
		}
		a.Enabled = a.Kind == "reject" && formHasValue(form, "enabled", "true")
		if a.Kind == "send" && form.Get("confirm") != "yes" {
			return a, fmt.Errorf("exact comment confirmation is required")
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
