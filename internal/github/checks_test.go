package github

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"release-assistant/internal/release"
)

func TestReadRCChecksJoinsWorkflowTriggersAndKeepsLatestAttempts(t *testing.T) {
	target := release.Target{Repo: "stackrox/stackrox", Tag: "5.0.1-rc.0"}
	server := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, "/repos/"+target.Repo) {
		case "/commits/refs/tags/5.0.1-rc.0":
			fmt.Fprint(w, `{"sha":"candidate-sha"}`)
		case "/commits/candidate-sha/check-runs":
			fmt.Fprint(w, `{"total_count":4,"check_runs":[
				{"id":1,"name":"Upgrade","status":"completed","conclusion":"failure","app":{"slug":"github-actions"},"check_suite":{"id":101}},
				{"id":2,"name":"Upgrade","status":"completed","conclusion":"success","app":{"slug":"github-actions"},"check_suite":{"id":102}},
				{"id":3,"name":"Upgrade","status":"completed","conclusion":"cancelled","app":{"slug":"github-actions"},"check_suite":{"id":103}},
				{"id":4,"name":"Red Hat Konflux / acs-conforma-prod / main","status":"in_progress","app":{"slug":"red-hat-konflux"}}
			]}`)
		case "/commits/candidate-sha/statuses":
			fmt.Fprint(w, `[{"context":"external CI","state":"failure"},{"context":"external CI","state":"pending"}]`)
		case "/actions/runs":
			if r.URL.Query().Get("head_sha") != "candidate-sha" {
				t.Errorf("workflow lookup was not scoped to the candidate SHA: %s", r.URL)
			}
			fmt.Fprint(w, `{"total_count":4,"workflow_runs":[
				{"id":301,"check_suite_id":101,"workflow_id":200,"head_sha":"candidate-sha","event":"push","head_branch":"5.0.1-rc.0"},
				{"id":302,"check_suite_id":102,"workflow_id":200,"head_sha":"candidate-sha","event":"push","head_branch":"5.0.1-rc.0"},
				{"id":303,"check_suite_id":103,"workflow_id":200,"head_sha":"candidate-sha","event":"push","head_branch":"release-5.0"},
				{"id":404,"check_suite_id":103,"workflow_id":200,"head_sha":"other-sha","event":"pull_request","head_branch":"wrong"}
			]}`)
		default:
			t.Errorf("unexpected request %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	c := New(server.URL, "", server.Client())
	result := c.ReadRCChecks(t.Context(), target)
	if result.SHA != "candidate-sha" || !result.Evidence.Fresh() || len(result.Checks) != 4 {
		t.Fatalf("unexpected validation snapshot %+v", result)
	}
	if slices.ContainsFunc(result.Checks, func(check release.ValidationCheck) bool { return check.ID == 1 }) {
		t.Fatal("prior failed attempt survived a successful rerun")
	}
	index := slices.IndexFunc(result.Checks, func(check release.ValidationCheck) bool { return check.ID == 3 })
	if index < 0 {
		t.Fatal("missing branch run on candidate SHA")
	}
	branch := result.Checks[index]
	if branch.Event != "push" || branch.Ref != "release-5.0" ||
		branch.WorkflowURL != "https://github.com/stackrox/stackrox/actions/runs/303" ||
		!strings.HasSuffix(branch.URL, "/commit/candidate-sha/checks?check_run_id=3") {
		t.Fatalf("incorrect workflow provenance %+v", branch)
	}
}

func TestReadRCChecksPreservesPartialEvidenceOnActionsFailure(t *testing.T) {
	server := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/refs/tags/"):
			fmt.Fprint(w, `{"sha":"candidate-sha"}`)
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			fmt.Fprint(w, `{"total_count":1,"check_runs":[{"id":1,"name":"Upgrade","status":"completed","conclusion":"failure","app":{"slug":"github-actions"},"check_suite":{"id":101}}]}`)
		case strings.HasSuffix(r.URL.Path, "/statuses"):
			fmt.Fprint(w, "[]")
		case strings.HasSuffix(r.URL.Path, "/actions/runs"):
			w.WriteHeader(http.StatusForbidden)
		default:
			t.Errorf("unexpected request %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	c := New(server.URL, "", server.Client())
	result := c.ReadRCChecks(t.Context(), release.Target{Repo: "stackrox/stackrox", Tag: "5.0.1-rc.0"})
	if result.Evidence.Fresh() || !strings.Contains(result.Evidence.Error, "Workflow triggers:") ||
		len(result.Checks) != 1 || result.Checks[0].Event != "" || result.Checks[0].Conclusion != "failure" {
		t.Fatalf("partial checks were lost or mislabeled: %+v", result)
	}
}

func TestPRCheckResultsStayCurrentWhenRequiredSettingsAreUnavailable(t *testing.T) {
	server := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			fmt.Fprint(w, `{"total_count":1,"check_runs":[{"id":1,"name":"GitHub Actions tests","status":"completed","conclusion":"failure","app":{"slug":"github-actions"}}]}`)
		case strings.HasSuffix(r.URL.Path, "/statuses"):
			fmt.Fprint(w, `[{"context":"external CI","state":"failure"}]`)
		case strings.HasSuffix(r.URL.Path, "/required_status_checks"):
			w.WriteHeader(http.StatusForbidden)
		default:
			t.Errorf("unexpected request %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	c := New(server.URL, "", server.Client())
	pr := release.PR{SHA: "head"}
	c.readChecks(t.Context(), release.Source{Repo: "stackrox/stackrox", Branch: "release-5.0"}, &pr)
	if !pr.CheckResultsEvidence.Fresh() || pr.ChecksEvidence.Fresh() ||
		!strings.Contains(pr.ChecksEvidence.Error, "Required-check settings") || len(pr.Checks) != 2 {
		t.Fatalf("check availability or required settings were mislabeled: %+v", pr)
	}
	for _, check := range pr.Checks {
		if check.Result() != release.No || check.Required != "" {
			t.Errorf("failed check was hidden or classified without branch settings: %+v", check)
		}
	}
}
