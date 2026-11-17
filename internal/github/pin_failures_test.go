package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"

	"release-assistant/internal/app"
	"release-assistant/internal/release"
)

func TestPinReadsKeepSpecificRequestFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		metadata, fact int
		content, want  string
	}{
		"metadata authentication failed": {metadata: 401, want: "Authentication failed"},
		"metadata not found":             {metadata: 404, want: "Not found or inaccessible"},
		"metadata rate limited":          {metadata: 429, want: "rate limit reached"},
		"version file access denied":     {fact: 403, want: "Access denied"},
		"missing version file":           {fact: 404, want: "Not found or inaccessible"},
		"empty version file":             {content: " ", want: "Version file is empty"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/branches/release"):
					_, _ = w.Write([]byte(`{"commit":{"sha":"branch"}}`))
				case strings.HasSuffix(r.URL.Path, "/pulls/123"):
					if tc.metadata != 0 {
						w.WriteHeader(tc.metadata)
						_, _ = w.Write([]byte("private-response-secret"))
						return
					}
					_, _ = w.Write([]byte(`{"number":123,"state":"closed","base":{"ref":"release"},"head":{"sha":"pin-head"}}`))
				case strings.Contains(r.URL.Path, "/contents/"):
					content := "3.26.5"
					if strings.HasSuffix(r.URL.Path, "/FACT_VERSION") {
						content = "0.4.7"
						if r.URL.Query().Get("ref") == "pin-head" {
							if tc.fact != 0 {
								w.WriteHeader(tc.fact)
								_, _ = w.Write([]byte("private-response-secret"))
								return
							}
							if tc.content != "" {
								content = tc.content
							}
						}
					}
					_ = json.NewEncoder(w).Encode(map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(content))})
				default:
					_, _ = w.Write([]byte(`[]`))
				}
			}))
			defer srv.Close()
			repo, _ := New(srv.URL, "private-token", srv.Client()).Discover(t.Context(), app.DiscoveryRequest{Source: release.Source{Name: "acs", Repo: "stackrox/stackrox", Branch: "release"}, Version: "5.0.1", PinningPR: 123})
			var problem string
			if tc.metadata != 0 {
				problem = repo.PinningEvidence.Error
				if repo.PinningPR != nil || repo.PinningEvidence.Fresh() || repo.PinningPins.Evidence.Error == "" {
					t.Fatal("failed metadata read looks pending or successful")
				}
			} else {
				problem = repo.PinningPins.Failures["fact"]
				if repo.PinningPins.Versions["collector"] != "3.26.5" || repo.PinningPins.Paths["fact"] != "FACT_VERSION" || repo.PinningPins.Evidence.Fresh() {
					t.Fatal("partial pin values or error location lost")
				}
			}
			if !strings.Contains(problem, tc.want) || strings.Contains(problem, "private-") {
				t.Fatalf("unsafe or unhelpful problem: %q", problem)
			}
			if !repo.Pins.Evidence.Fresh() {
				t.Fatal("PR failure invalidated independently read branch pins")
			}
		})
	}
}

func TestRequestProblemPreservesSpecificSafeCauses(t *testing.T) {
	for name, tc := range map[string]struct {
		cause error
		want  string
	}{
		"timeout": {context.DeadlineExceeded, "Request timed out"},
		"DNS":     {&net.DNSError{Err: "private-error", Name: "private-host"}, "DNS lookup failed"},
	} {
		t.Run(name, func(t *testing.T) {
			c := New("https://api.github.test", "private-token", &http.Client{Transport: errorTransport{err: tc.cause}})
			err := c.get(t.Context(), "/repos/acme/repo?token=private-token", new(map[string]any))
			problem := requestProblem(err)
			if !strings.Contains(problem, tc.want) || strings.Contains(problem, "private-") {
				t.Fatalf("problem=%q", problem)
			}
		})
	}
}
