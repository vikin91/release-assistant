package konflux

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHealthProbeUsesApprovedNamespaceAndRecentStart(t *testing.T) {
	const host = "api.example.openshiftapps.com"
	lookups := make(Lookups)
	for _, lookup := range []string{"42," + host + ",builds", "43," + host + ",builds", "42,api.other.openshiftapps.com,other"} {
		if err := lookups.Add(lookup); err != nil {
			t.Fatal(err)
		}
	}
	started := time.Now().Add(-time.Minute).Truncate(time.Second)
	for name, tc := range map[string]struct {
		apiStatus int
		recent    bool
		redirect  bool
	}{
		"should verify a recent start":      {apiStatus: http.StatusOK, recent: true},
		"should report a readable idle API": {apiStatus: http.StatusOK},
		"should report missing sign-in":     {apiStatus: http.StatusUnauthorized},
		"should decline redirects":          {apiStatus: http.StatusFound, redirect: true},
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.Method != http.MethodGet || req.URL.Host != host+":6443" || req.Header.Get("Authorization") != "Bearer cluster-token" {
					t.Fatalf("unexpected health request %s %s", req.Method, req.URL)
				}
				response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header)}
				var body string
				switch calls {
				case 1:
					if req.URL.Path != "/apis/tekton.dev/v1/namespaces/builds/pipelineruns" || req.URL.Query().Get("limit") != "1" || req.Header.Get("Accept") != metadataList {
						t.Fatalf("unexpected PipelineRun query %s", req.URL)
					}
					response.StatusCode = tc.apiStatus
					if tc.redirect {
						response.Header.Set("Location", "https://elsewhere.example")
					}
					body = `{"kind":"PartialObjectMetadataList","items":[]}`
				case 2, 3:
					const path = "/api/v1/namespaces/builds/events"
					if req.URL.Path != path || !strings.Contains(req.URL.Query().Get("fieldSelector"), "reason=Started") {
						t.Fatalf("unexpected event query %s", req.URL)
					}
					if calls == 2 && tc.recent {
						body = `{"kind":"EventList","metadata":{"continue":"next"},"items":[]}`
					} else if tc.recent {
						if req.URL.Query().Get("continue") != "next" {
							t.Fatal("event continuation was omitted")
						}
						body = fmt.Sprintf(`{"kind":"EventList","items":[{"firstTimestamp":%q,"involvedObject":{"kind":"PipelineRun","apiVersion":"tekton.dev/v1","namespace":"builds"}}]}`, started.Format(time.RFC3339))
					} else {
						body = `{"kind":"EventList","items":[]}`
					}
				default:
					t.Fatalf("unexpected request %d", calls)
				}
				response.Body = io.NopCloser(strings.NewReader(body))
				return response, nil
			})}
			adapter := New(Config{HTTPClient: client, apiHost: host, apiPort: "6443", lookups: lookups,
				KonfluxToken: func(_ context.Context, requested string) (string, error) {
					if requested != host {
						t.Fatalf("requested credentials for %s", requested)
					}
					return "cluster-token", nil
				},
			})
			health := adapter.ProbeHealth(t.Context())
			if health.APIStatus != tc.apiStatus || health.APIReadable != (tc.apiStatus == http.StatusOK) || health.Healthy() != tc.recent {
				t.Fatalf("unexpected health observation: %+v", health)
			}
			if tc.recent && !health.AdmittedAt.Equal(started) {
				t.Fatalf("wrong admission time: %v", health.AdmittedAt)
			}
			if tc.apiStatus != http.StatusOK && calls != 1 {
				t.Fatalf("failed API read made %d requests", calls)
			}
		})
	}
}
