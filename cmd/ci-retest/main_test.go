package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"release-assistant/internal/github"
)

func TestTargetLists(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"PRs keep first occurrence":      {[]string{"--prs", "2,1", "02"}, "[stackrox/stackrox#2 stackrox/stackrox#1]"},
		"short repo and namespaced tags": {[]string{"--repo", "scanner", "--tags", "v1,build/v2+rc", "v1"}, "[stackrox/scanner@v1 stackrox/scanner@build/v2+rc]"},
		"full repo":                      {[]string{"--repo", "owner/repo", "--prs", "1"}, "[owner/repo#1]"},
		"mixed targets":                  {[]string{"--prs", "2", "--tags", "v1"}, ""},
		"missing targets":                {nil, ""},
		"invalid PR":                     {[]string{"--prs", "x"}, ""},
		"invalid ref":                    {[]string{"--tags", "v1/.hidden"}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := targets(tc.args, new(bytes.Buffer))
			if tc.want == "" {
				if err == nil {
					t.Fatal("expected usage error")
				}
				return
			}
			if err != nil || fmt.Sprint(got) != tc.want {
				t.Fatalf("targets = %v, %v", got, err)
			}
		})
	}
}

func TestCLIReports(t *testing.T) {
	const root = "/repos/stackrox/stackrox"
	const sha = "1111111111111111111111111111111111111111"
	const pr = `{"state":"open","number":7,"base":{"ref":"master","repo":{"id":1}},"head":{"sha":"` + sha + `","repo":{"id":2}}}`
	const statuses = `[{"context":"ci/prow/Green","state":"success"},{"context":"ci/prow/green","state":"failure","creator":{"id":75433959},"target_url":"https://prow.ci.openshift.org/view/gs/test-platform-results-public/pr-logs/pull/stackrox_stackrox/7/green/111"},{"context":"ci/prow/build","state":"failure","creator":{"id":75433959},"target_url":"https://prow.ci.openshift.org/view/gs/test-platform-results-public/pr-logs/pull/stackrox_stackrox/7/build/123"},{"context":"ci/prow/test","state":"pending"},{"context":"ci/prow/test","state":"failure","creator":{"id":75433959},"target_url":"https://prow.ci.openshift.org/view/gs/test-platform-results-public/pr-logs/pull/stackrox_stackrox/7/test/345"}]`
	const checks = `{"total_count":2,"check_runs":[
		{"id":41,"name":"Konflux: release-central-db:rc","status":"completed","conclusion":"failure","head_sha":"` + sha + `","app":{"slug":"github-actions"},"check_suite":{"id":11}},
		{"id":42,"name":"unit tests","status":"completed","conclusion":"failure","head_sha":"` + sha + `","app":{"slug":"github-actions"},"check_suite":{"id":11}}
	]}`
	const jobs = `{"total_count":2,"jobs":[
		{"id":99,"status":"completed","conclusion":"failure","check_run_url":"https://api.github.com/repos/stackrox/stackrox/check-runs/41"},
		{"id":101,"status":"completed","conclusion":"failure","check_run_url":"https://api.github.com/repos/stackrox/stackrox/check-runs/42"}
	]}`
	const jobOutput = `
Available retries
Scan images for vulnerabilities / Konflux: release-central-db:rc
https://github.com/stackrox/stackrox/actions/runs/22/job/99
gh api --method POST repos/stackrox/stackrox/actions/jobs/99/rerun

Scan images for vulnerabilities / unit tests
https://github.com/stackrox/stackrox/actions/runs/22/job/101
gh api --method POST repos/stackrox/stackrox/actions/jobs/101/rerun`

	for name, tc := range map[string]struct {
		args                             []string
		workflow, ref                    string
		moved, otherCheck, missingConfig bool
		want, absent                     []string
		code                             int
	}{
		"PR reports both actions and continues past missing target": {args: []string{"--prs", "8,7", "7"}, want: []string{"HTTP 404", jobOutput + "\n\nFailed Prow checks:", "Failed Prow checks: post this comment on https://github.com/stackrox/stackrox/pull/7", "\n/test build-job\n"}, absent: []string{"ci/prow/test", "/test green", "/test pending", "\n/retest\n"}, code: 1},
		"Prow config unavailable preserves Actions offers":          {args: []string{"--prs", "7"}, missingConfig: true, want: []string{jobOutput, "reading OpenShift CI presubmits"}, absent: []string{"Failed Prow checks:", "/test build-job", "\n/retest\n"}, code: 1},
		"PR head moved after observation":                           {args: []string{"--prs", "7"}, moved: true, want: []string{"target identity could not be reconfirmed"}, absent: []string{"/job/99", "\n/retest\n", "/test build-job"}, code: 1},
		"job for different check":                                   {args: []string{"--prs", "7"}, otherCheck: true, want: []string{"no unique failed job", "\n/test build-job\n"}, absent: []string{"/job/99"}},
		"tag only push":                                             {args: []string{"--tags", "rc"}, workflow: "on:\n  push:\n    tags: ['*']\n", want: []string{"Tag: rc", "/commit/" + sha, jobOutput + "\n\nWarnings:\nThis tool cannot tell flakes from true failures. Inspect each failure before retrying.\n"}, absent: []string{"\n/retest\n"}},
		"tag proven by local workflow":                              {args: []string{"--tags", "rc"}, workflow: "jobs:\n  build:\n    uses: ./.github/workflows/build.yml\n", ref: "refs/tags/rc", want: []string{"/job/99"}},
		"same name branch push":                                     {args: []string{"--tags", "rc"}, workflow: "jobs:\n  build:\n    uses: ./.github/workflows/build.yml\n", ref: "refs/heads/rc", want: []string{"tag trigger unproven"}, absent: []string{"/job/99", "\n/retest\n", "/test build-job"}},
		"separately pinned reusable workflow":                       {args: []string{"--tags", "rc"}, workflow: "jobs:\n  build:\n    uses: stackrox/stackrox/.github/workflows/build.yml@rc\n", ref: "refs/tags/rc", want: []string{"tag trigger unproven"}, absent: []string{"/job/99"}},
	} {
		t.Run(name, func(t *testing.T) {
			reads := 0
			event, ref := "pull_request", "feature"
			workflowName := "Scan images for vulnerabilities"
			if tc.args[0] == "--tags" {
				event, ref = "push", "rc"
				workflowName += " (rc)"
			}
			runData := fmt.Sprintf(`{"id":22,"name":%q,"path":".github/workflows/ci.yml","workflow_id":33,"check_suite_id":11,"head_sha":%q,"head_branch":%q,"event":%q,"run_attempt":1,"run_number":2,"status":"completed","created_at":%q,"pull_requests":[%s],"referenced_workflows":[{"path":"stackrox/stackrox/.github/workflows/build.yml@%s","ref":%q,"sha":%q}]}`, workflowName, sha, ref, event, time.Now().UTC().Format(time.RFC3339), pr, sha, tc.ref, sha)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Errorf("unexpected method %s", r.Method)
				}
				switch strings.TrimPrefix(r.URL.Path, root) {
				case "/repos/openshift/release/contents/ci-operator/jobs/stackrox/stackrox":
					fmt.Fprint(w, `[{"type":"file","name":"stackrox-stackrox-presubmits.yaml","sha":"config"}]`)
				case "/repos/openshift/release/git/blobs/config":
					if tc.missingConfig {
						http.NotFound(w, r)
						return
					}
					const config = `presubmits:
  stackrox/stackrox:
  - name: build
    branches: [^master$]
    context: ci/prow/build
    rerun_command: /test build-job
    trigger: '(?m)^/test build-job$'
  - name: release-build
    branches: [^release-5$]
    context: ci/prow/release
    rerun_command: /test build-job
    trigger: '(?m)^/test build-job$'
  - name: green
    context: ci/prow/green
    rerun_command: /test green
    trigger: '(?m)^/test green$'
  - name: test
    context: ci/prow/test
    rerun_command: /test pending
    trigger: '(?m)^/test pending$'
`
					fmt.Fprintf(w, `{"encoding":"base64","content":%q}`, base64.StdEncoding.EncodeToString([]byte(config)))

				case "/pulls/8":
					http.NotFound(w, r)
				case "/pulls/7":
					reads++
					if tc.moved && reads > 1 {
						fmt.Fprint(w, strings.ReplaceAll(pr, sha, "changed"))
						return
					}
					fmt.Fprint(w, pr)
				case "/commits/refs/tags/rc":
					fmt.Fprintf(w, `{"sha":%q}`, sha)
				case "/commits/" + sha + "/statuses":
					if tc.args[0] == "--tags" {
						t.Error("read PR statuses for tag")
					}
					fmt.Fprint(w, statuses)
				case "/commits/" + sha + "/check-runs":
					fmt.Fprint(w, checks)
				case "/actions/runs":
					fmt.Fprintf(w, `{"total_count":1,"workflow_runs":[%s]}`, runData)
				case "/actions/runs/22":
					fmt.Fprint(w, runData)
				case "/contents/.github/workflows/ci.yml":
					fmt.Fprintf(w, `{"encoding":"base64","content":%q}`, base64.StdEncoding.EncodeToString([]byte(tc.workflow)))
				case "/actions/workflows/33":
					fmt.Fprint(w, `{"state":"active"}`)
				case "/actions/runs/22/attempts/1/jobs":
					if tc.otherCheck {
						fmt.Fprint(w, strings.ReplaceAll(jobs, "check-runs/4", "check-runs/9"))
						return
					}
					fmt.Fprint(w, jobs)
				default:
					t.Errorf("unexpected request %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			var out, diag bytes.Buffer
			code := run(t.Context(), tc.args, &out, &diag, func(context.Context) (*github.Client, error) { return github.New(server.URL, "", server.Client()), nil })
			if code != tc.code || diag.Len() != 0 {
				t.Fatalf("code=%d, stderr=%s, stdout=%s", code, &diag, &out)
			}
			for _, want := range tc.want {
				if !strings.Contains(out.String(), want) {
					t.Errorf("missing %q in %s", want, &out)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(out.String(), absent) {
					t.Errorf("unexpected %q in %s", absent, &out)
				}
			}
		})
	}
}
