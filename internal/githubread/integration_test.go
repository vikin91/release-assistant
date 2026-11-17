package githubread_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"release-assistant/internal/app"
	"release-assistant/internal/github"
	"release-assistant/internal/githubread"
	"release-assistant/internal/konflux"
	"release-assistant/internal/release"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDiscoveryAndPlannerUseOneSourceAllowance(t *testing.T) {
	calls := 0
	transport := githubread.New(transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		body := `[]`
		if strings.Contains(r.URL.Path, "/branches/") {
			body = `{"commit":{"sha":"head"}}`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	httpClient := &http.Client{Transport: transport}
	discovery := github.New("", "", httpClient)
	planner := konflux.New(konflux.Config{HTTPClient: httpClient})
	ctx, usage := githubread.Track(t.Context(), 2)
	_, err := discovery.Discover(ctx, app.DiscoveryRequest{Source: release.Source{Name: "fact", Repo: "acme/fact", Branch: "release"}, Scope: app.ReadPRs})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.Plan(ctx, release.Target{Repo: "acme/fact", PR: 1})
	if err == nil || plan.Evidence.Fresh() || !strings.Contains(err.Error(), "source-cycle GitHub read allowance exhausted") || usage.Calls() != 2 || calls != 2 {
		t.Fatalf("planner bypassed source allowance: calls=%d usage=%d plan=%+v error=%v", calls, usage.Calls(), plan, err)
	}
}
