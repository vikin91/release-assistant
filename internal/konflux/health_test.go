package konflux

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"release-assistant/internal/release"
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
			config := startupConfig(t.Context(), lookups, func(context.Context, string, ...string) string {
				return "https://" + host + ":6443\ncluster-token"
			})
			config.HTTPClient = client
			adapter := New(config)
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

func TestHealthRecoversAfterOCLogin(t *testing.T) {
	const (
		host    = "api.example.openshiftapps.com"
		other   = "api.other.openshiftapps.com"
		startup = "https://" + host + ":6443\ninitial-token"
		renewed = "https://" + host + ":6443\nrenewed-token"
		newPort = "https://" + host + ":8443\ninitial-token"
		newHost = "https://" + other + ":8443\nrenewed-token"
	)
	for name, tc := range map[string]struct {
		startup, next, destination string
		initialStatus              int
		rejectUpdate               bool
	}{
		"should recover after startup without a login":                    {next: renewed, destination: host + ":6443"},
		"should recover when a token expires":                             {startup: startup, next: renewed, destination: host + ":6443", initialStatus: http.StatusUnauthorized},
		"should recover after access is forbidden":                        {startup: startup, next: renewed, destination: host + ":6443", initialStatus: http.StatusForbidden},
		"should recover when only the port changes":                       {startup: startup, next: newPort, destination: host + ":8443", initialStatus: http.StatusUnauthorized},
		"should recover on a different approved cluster":                  {startup: startup, next: newHost, destination: other + ":8443", initialStatus: http.StatusUnauthorized},
		"should stay unavailable when oc fails":                           {startup: startup, initialStatus: http.StatusUnauthorized},
		"should stay unavailable when oc returns no token":                {startup: startup, next: "https://" + other, initialStatus: http.StatusUnauthorized},
		"should stay unavailable when the replacement fails verification": {startup: startup, next: renewed, destination: host + ":6443", initialStatus: http.StatusUnauthorized, rejectUpdate: true},
		"should stay unavailable on an unapproved cluster":                {startup: startup, next: "https://api.unapproved.openshiftapps.com\nrenewed-token", initialStatus: http.StatusUnauthorized},
	} {
		t.Run(name, func(t *testing.T) {
			lookups := Lookups{
				{appID: 42, host: host, namespace: "builds"}:        {},
				{appID: 43, host: other, namespace: "other-builds"}: {},
			}
			output, reads := tc.startup, 0
			config := startupConfig(t.Context(), lookups, func(context.Context, string, ...string) string {
				reads++
				return output
			})
			initial := config.oc.current.Load()
			expectedUpdate := tc.destination != ""
			initialStatus := http.StatusOK
			updates := 0
			config.HTTPClient = &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
				token := req.Header.Get("Authorization")
				namespace := "builds"
				if req.URL.Hostname() == other {
					namespace = "other-builds"
				}
				if req.URL.Path != "/apis/tekton.dev/v1/namespaces/"+namespace+"/pipelineruns" {
					t.Fatalf("unexpected namespace in %s", req.URL)
				}
				if req.URL.Host == host+":6443" && token == "Bearer initial-token" && tc.startup != "" {
					return healthFixtureResponse(initialStatus, namespace), nil
				}
				_, nextToken, _ := strings.Cut(tc.next, "\n")
				if !expectedUpdate || req.URL.Host != tc.destination || token != "Bearer "+nextToken {
					t.Fatalf("unexpected host or credentials on request to %s", req.URL)
				}
				updates++
				if tc.rejectUpdate {
					return healthFixtureResponse(http.StatusUnauthorized, namespace), nil
				}
				return healthFixtureResponse(http.StatusOK, namespace), nil
			})}
			adapter := New(config)
			check := func(wantHealthy bool, wantReads int) {
				t.Helper()
				health := adapter.ProbeHealth(t.Context())
				if health.Healthy() != wantHealthy || health.APIReadable != wantHealthy || reads != wantReads {
					t.Fatalf("health = %+v, oc reads = %d, want healthy = %t and %d reads", health, reads, wantHealthy, wantReads)
				}
			}
			if initial != nil {
				check(true, 1)
				check(true, 1)
			} else {
				check(false, 2)
			}

			initialStatus, output = tc.initialStatus, tc.next
			recovered := expectedUpdate && !tc.rejectUpdate
			probeReads := reads + 1
			check(recovered, probeReads)
			if recovered {
				check(true, probeReads)
				if updates != 2 {
					t.Fatalf("updated login was used for %d health reads, want 2", updates)
				}
			} else if adapter.oc.current.Load() != initial {
				t.Fatal("failed verification replaced the cached login")
			}
		})
	}
}

func TestPlanKeepsItsLoginWhileHealthUpdatesIt(t *testing.T) {
	const host = "api.example.openshiftapps.com"
	const other = "api.other.openshiftapps.com"
	lookups := Lookups{
		{appID: 296509, host: host, namespace: "builds"}:  {},
		{appID: 296509, host: other, namespace: "builds"}: {},
	}
	login := "https://" + host + ":6443\ninitial-token"
	var ocReads, oldReads, newReads atomic.Int32
	config := startupConfig(t.Context(), lookups, func(context.Context, string, ...string) string {
		ocReads.Add(1)
		return login
	})
	firstPlanStarted, resumeFirstPlan := make(chan struct{}), make(chan struct{})
	config.HTTPClient = &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
		status, body := http.StatusOK, ""
		switch {
		case req.URL.Host == "api.github.com" && strings.HasSuffix(req.URL.Path, "/pulls/1"):
			close(firstPlanStarted)
			select {
			case <-resumeFirstPlan:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
			body = `{"state":"open","head":{"sha":"old"}}`
		case req.URL.Host == "api.github.com" && strings.HasSuffix(req.URL.Path, "/pulls/2"):
			body = `{"state":"open","head":{"sha":"new"}}`
		case req.URL.Host == "api.github.com" && strings.HasSuffix(req.URL.Path, "/check-runs"):
			cluster := "example.openshiftapps.com"
			if strings.Contains(req.URL.Path, "/old/") {
				body = checkRunsFixture(cluster, "old")
			} else {
				body = checkRunsFixture(cluster, "new") + "," + checkRunsFixture("other.openshiftapps.com", "wrong-host")
			}
			body = `{"check_runs":[` + body + `]}`
		case req.URL.Host == "api.github.com" && strings.HasPrefix(req.URL.Path, "/repos/stackrox/fact/contents/.tekton/"):
			status = http.StatusNotFound
		case req.URL.Query().Get("limit") == "1" && req.URL.Host == host+":6443":
			return healthFixtureResponse(http.StatusUnauthorized, "builds"), nil
		case req.URL.Query().Get("limit") == "1" && req.URL.Host == host+":8443":
			return healthFixtureResponse(http.StatusOK, "builds"), nil
		case strings.HasSuffix(req.URL.Path, "/old") && req.URL.Host == host+":6443" &&
			req.Header.Get("Authorization") == "Bearer initial-token":
			oldReads.Add(1)
			body = "{}"
		case strings.HasSuffix(req.URL.Path, "/new") && req.URL.Host == host+":8443" &&
			req.Header.Get("Authorization") == "Bearer renewed-token":
			newReads.Add(1)
			body = "{}"
		default:
			t.Errorf("unexpected planner endpoint or credentials for %s", req.URL)
			status = http.StatusNotFound
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	adapter := New(config)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	wg.Go(func() {
		_, _ = adapter.Plan(ctx, release.Target{Repo: "stackrox/fact", PR: 1})
	})
	select {
	case <-firstPlanStarted:
	case <-ctx.Done():
		t.Fatal("first plan did not start")
	}
	login = "https://" + host + ":8443\nrenewed-token"
	health := adapter.ProbeHealth(ctx)
	_, _ = adapter.Plan(ctx, release.Target{Repo: "stackrox/fact", PR: 2})
	close(resumeFirstPlan)
	wg.Wait()
	if !health.Healthy() {
		t.Fatalf("health did not recover: %+v", health)
	}
	if oldReads.Load() != 1 || newReads.Load() != 1 || ocReads.Load() != 2 {
		t.Fatalf("wrong read counts: old API %d, new API %d, oc %d", oldReads.Load(), newReads.Load(), ocReads.Load())
	}
}

func healthFixtureResponse(status int, namespace string) *http.Response {
	body := fmt.Sprintf(`{"kind":"PipelineRunList","items":[{"metadata":{"namespace":%q,"creationTimestamp":%q}}]}`,
		namespace, time.Now().Add(-time.Minute).Format(time.RFC3339))
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func checkRunsFixture(cluster, pipeline string) string {
	return fmt.Sprintf(`{"id":17,"name":%q,"app":{"id":296509},"status":"completed","conclusion":"failure","details_url":%q}`,
		"Red Hat Konflux / "+pipeline, "https://konflux-ui.apps."+cluster+"/ns/builds/pipelinerun/"+pipeline)
}
