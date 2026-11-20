// Package apilog identifies outbound API requests for the activity log.
package apilog

import (
	"cmp"
	"net/http"
	"net/url"
	"strings"
)

// Transport records each request immediately before handing it to Base.
type Transport struct {
	Base    http.RoundTripper
	Calling func(system, method, resource string)
}

// RoundTrip logs the system, method, and resource without reading the request body.
func (t Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Quay redirects blob reads to signed storage URLs. Identify each hop by the
	// original registry resource so the signed destination stays private.
	origin := req
	for origin.Response != nil && origin.Response.Request != nil {
		origin = origin.Response.Request
	}
	host := strings.ToLower(origin.URL.Hostname())
	system := "Other"
	switch {
	case host == "api.github.com":
		system = "GitHub"
	case host == "quay.io":
		system = "Quay"
	case strings.HasSuffix(host, ".openshiftapps.com"):
		system = "Konflux"
	}
	_, credential, _ := strings.Cut(origin.Header.Get("Authorization"), " ")
	resource := Resource(origin.URL, credential)
	if req != origin {
		resource += " (redirect)"
	}
	t.Calling(system, cmp.Or(req.Method, http.MethodGet), resource)
	return t.Base.RoundTrip(req)
}

// Resource returns a single escaped path and redacts credentials from diagnostics.
func Resource(u *url.URL, credential string) string {
	path := cmp.Or(u.EscapedPath(), "/")
	if credential != "" {
		path = strings.ReplaceAll(path, url.PathEscape(credential), "[redacted]")
		path = strings.ReplaceAll(path, credential, "[redacted]")
	}
	lower := strings.ToLower(path)
	for _, marker := range []string{"token", "secret", "password", "credential", "authorization", "api_key", "apikey"} {
		if strings.Contains(lower, marker) {
			return "/[redacted]"
		}
	}
	return path
}
