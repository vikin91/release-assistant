package apilog_test

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"testing"

	"release-assistant/internal/apilog"
	"release-assistant/internal/app"
	"release-assistant/internal/githubread"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestOutboundRequestLog(t *testing.T) {
	for name, tc := range map[string]struct{ method, url, want string }{
		"GitHub read":         {"GET", "https://api.github.com/repos/stackrox/stackrox/pulls/23956?page=2", "GitHub GET /repos/stackrox/stackrox/pulls/23956"},
		"GitHub write":        {"POST", "https://api.github.com/repos/test/project/issues/7/comments", "GitHub POST /repos/test/project/issues/7/comments"},
		"Quay manifest":       {"HEAD", "https://quay.io/v2/test/project/manifests/build", "Quay HEAD /v2/test/project/manifests/build"},
		"Quay authentication": {"GET", "https://quay.io/v2/auth?scope=private", "Quay GET /v2/auth"},
		"Konflux PipelineRun": {"GET", "https://api.cluster.openshiftapps.com/apis/tekton.dev/v1/namespaces/test/pipelineruns/build", "Konflux GET /apis/tekton.dev/v1/namespaces/test/pipelineruns/build"},
		"Konflux Results":     {"GET", "https://tekton-results-tekton-results.apps.cluster.openshiftapps.com/apis/results.tekton.dev/v1alpha2/parents/test/results/-/records?filter=private", "Konflux GET /apis/results.tekton.dev/v1alpha2/parents/test/results/-/records"},
		"escaped resource":    {"GET", "https://user@api.github.com/repos/test/project/commits/refs%2Ftags%2Fv1", "GitHub GET /repos/test/project/commits/refs%2Ftags%2Fv1"},
		"credential in path":  {"GET", "https://api.github.com/repos/test/project/commits/private", "GitHub GET /repos/test/project/commits/[redacted]"},
	} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			log := app.NewLog(&output)
			req, err := http.NewRequestWithContext(t.Context(), tc.method, tc.url, strings.NewReader("private"))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer private")
			failure := errors.New("network failure")
			base := roundTripFunc(func(got *http.Request) (*http.Response, error) {
				line := " Debug: Calling API: " + tc.want + "\n"
				if !strings.HasSuffix(output.String(), line) || strings.Count(output.String(), "\n") != 1 {
					t.Errorf("output before send = %q, want one line ending in %q", output.String(), line)
				}
				if got != req || got.Header.Get("Authorization") != "Bearer private" {
					t.Error("logging changed the request")
				}
				return nil, failure
			})
			transport := apilog.Transport{Base: base, Calling: log.CallingAPI}
			if _, err := transport.RoundTrip(req); !errors.Is(err, failure) {
				t.Fatalf("request error = %v, want %v", err, failure)
			}
			entries := log.Recent()
			if len(entries) != 1 || entries[0].Level() != "Debug" || entries[0].Message() != "Calling API: "+tc.want {
				t.Fatalf("activity = %+v", entries)
			}
		})
	}
}

func TestQuayRedirectLogsOriginalResource(t *testing.T) {
	var output bytes.Buffer
	log := app.NewLog(&output)
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp := &http.Response{Request: req, StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody}
		if req.URL.Host == "quay.io" {
			resp.StatusCode = http.StatusTemporaryRedirect
			resp.Header.Set("Location", "https://storage.example/private-path?signature=private")
		}
		return resp, nil
	})
	client := &http.Client{Transport: apilog.Transport{Base: base, Calling: log.CallingAPI}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://quay.io/v2/test/blobs/sha256:abcd", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	const want = " Debug: Calling API: Quay GET /v2/test/blobs/sha256:abcd"
	if len(lines) != 2 || !strings.HasSuffix(lines[0], want) || !strings.HasSuffix(lines[1], want+" (redirect)") {
		t.Fatalf("redirect log = %q", lines)
	}
}

func TestDeferredGitHubReadProducesNoCallRecord(t *testing.T) {
	var output bytes.Buffer
	log := app.NewLog(&output)
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})
	client := &http.Client{Transport: githubread.New(apilog.Transport{Base: base, Calling: log.CallingAPI})}
	ctx, _ := githubread.Track(t.Context(), 1)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/test/project", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(req)
	if _, deferred := errors.AsType[*githubread.DeferredError](err); !deferred || len(log.Recent()) != 1 {
		t.Fatalf("second read = %v, activity = %q", err, output.String())
	}
}
