package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"release-assistant/internal/github"
	"release-assistant/internal/konflux"
	"release-assistant/internal/release"
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
			got, err := targets(tc.args, new(bytes.Buffer), make(konflux.Lookups))
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

func TestKonfluxCommentsPrecedeManualRetries(t *testing.T) {
	const repo = "stackrox/stackrox"
	const sha = "abcd"
	for name, tc := range map[string]struct {
		args                    []string
		url, comment, manualSHA string
	}{
		"PR comment":         {[]string{"--prs", "7"}, "https://github.com/" + repo + "/pull/7", "/konflux-retest central-db-on-push", sha},
		"tag commit comment": {[]string{"--tags", "5.0.1-rc.0"}, "https://github.com/" + repo + "/commit/" + sha, "/retest central-db-on-push tag:5.0.1-rc.0", sha},
		"moved PR":           {[]string{"--prs", "7"}, "https://github.com/" + repo + "/pull/7", "/konflux-retest central-db-on-push", "new-head"},
	} {
		t.Run(name, func(t *testing.T) {
			var out, diagnostics bytes.Buffer
			var planned bool
			read := func(context.Context, konflux.Lookups) (readers, error) {
				return readers{
					konfluxPlan: func(_ context.Context, target release.Target) (release.Plan, error) {
						planned = true
						return release.Plan{Target: target, SHA: sha, Evidence: release.Evidence{URL: tc.url},
							Proposals: []release.Proposal{{Target: target, SHA: sha, Payload: tc.comment}}}, nil
					},
					manualRetries: func(_ context.Context, target release.Target) (release.ManualRetryReport, error) {
						if !planned {
							t.Fatal("CI checked the target before Konflux")
						}
						return release.ManualRetryReport{Target: target, SHA: tc.manualSHA, URL: tc.url, Jobs: []release.ManualJob{{ID: 17, Name: "scan"}}}, nil
					},
				}, nil
			}
			code := run(t.Context(), tc.args, &out, &diagnostics, read)
			beforeCI, _, found := strings.Cut(out.String(), "gh api --method POST")
			if !found {
				t.Fatalf("missing CI job: %s", &out)
			}
			if tc.manualSHA != sha {
				if code != 1 || strings.Contains(out.String(), tc.comment) {
					t.Fatalf("stale proposal: %s", &out)
				}
				return
			}
			if code != 0 || !strings.Contains(beforeCI, tc.url+" (commit "+sha+")\n"+tc.comment+"\n") || strings.Count(out.String(), tc.comment) != 1 {
				t.Fatalf("code=%d, output=%s, diagnostics=%s", code, &out, &diagnostics)
			}
		})
	}
}

func TestCLIReports(t *testing.T) {
	const sha = "1111111111111111111111111111111111111111"
	const pr = `{"state":"open","number":7,"base":{"ref":"master","repo":{"id":1}},"head":{"sha":"` + sha + `","repo":{"id":2}}}`
	const statuses = `[{"context":"ci/prow/Green","state":"success"},{"context":"ci/prow/green","state":"failure","creator":{"id":75433959},"target_url":"https://prow.ci.openshift.org/view/gs/test-platform-results-public/pr-logs/pull/stackrox_stackrox/7/green/111"},{"context":"ci/prow/gke-qa-e2e-tests","state":"failure","creator":{"id":75433959},"target_url":"https://prow.ci.openshift.org/view/gs/test-platform-results-public/pr-logs/pull/stackrox_stackrox/7/build/123"},{"context":"ci/prow/test","state":"pending"},{"context":"ci/prow/test","state":"failure","creator":{"id":75433959},"target_url":"https://prow.ci.openshift.org/view/gs/test-platform-results-public/pr-logs/pull/stackrox_stackrox/7/test/345"}]`
	failedCheck := func(id int, name, app string) string {
		return fmt.Sprintf(`{"id":%d,"name":%q,"status":"completed","conclusion":"failure","head_sha":%q,"app":{"slug":%q},"check_suite":{"id":11}}`, id, name, sha, app)
	}
	const jobs = `{"total_count":2,"jobs":[
		{"id":99,"status":"completed","conclusion":"failure","check_run_url":"https://api.github.com/repos/stackrox/stackrox/check-runs/41"},
		{"id":101,"status":"completed","conclusion":"failure","check_run_url":"https://api.github.com/repos/stackrox/stackrox/check-runs/42"}
	]}`
	const jobOutput = `
Available retries
Scan images for vulnerabilities / Konflux: release-central-db:rc
https://github.com/stackrox/stackrox/actions/runs/22/job/99
gh api --method POST repos/stackrox/stackrox/actions/jobs/99/rerun

Scan images for vulnerabilities / Upstream: main:rc
https://github.com/stackrox/stackrox/actions/runs/22/job/101
gh api --method POST repos/stackrox/stackrox/actions/jobs/101/rerun`

	for name, tc := range map[string]struct {
		args                             []string
		workflow, ref                    string
		repo, workflowName, workflowPath string
		mixed                            bool
		moved, otherCheck, missingConfig bool
		want, absent                     []string
		code                             int
	}{
		"PR reports both actions and continues past missing target": {args: []string{"--prs", "8,7", "7"}, want: []string{"HTTP 404", jobOutput + "\n\nFailed Prow checks:", "Failed Prow checks: post this comment on https://github.com/stackrox/stackrox/pull/7", "\n/test gke-qa-e2e-tests\n"}, absent: []string{"ci/prow/test", "/test green", "/test pending", "\n/retest\n"}, code: 1},
		"Prow config unavailable preserves Actions offers":          {args: []string{"--prs", "7"}, missingConfig: true, want: []string{jobOutput, "reading OpenShift CI presubmits"}, absent: []string{"Failed Prow checks:", "/test gke-qa-e2e-tests", "\n/retest\n"}, code: 1},
		"PR head moved after observation":                           {args: []string{"--prs", "7"}, moved: true, want: []string{"target identity could not be reconfirmed"}, absent: []string{"/job/99", "\n/retest\n", "/test gke-qa-e2e-tests"}, code: 1},
		"job for different check":                                   {args: []string{"--prs", "7"}, otherCheck: true, want: []string{"no unique failed job", "\n/test gke-qa-e2e-tests\n"}, absent: []string{"/job/99"}},
		"tag only push":                                             {args: []string{"--tags", "rc"}, workflow: "on:\n  push:\n    tags: ['*']\n", want: []string{"Tag: rc", "/commit/" + sha, jobOutput + "\n\nWarnings:\nThis tool cannot tell flakes from true failures. Inspect each failure before retrying.\n"}, absent: []string{"\n/retest\n"}},
		"tag proven by local workflow":                              {args: []string{"--tags", "rc"}, workflow: "jobs:\n  build:\n    uses: ./.github/workflows/build.yml\n", ref: "refs/tags/rc", want: []string{"/job/99"}},
		"same name branch push":                                     {args: []string{"--tags", "rc"}, workflow: "jobs:\n  build:\n    uses: ./.github/workflows/build.yml\n", ref: "refs/heads/rc", want: []string{"tag trigger unproven"}, absent: []string{"/job/99", "\n/retest\n", "/test gke-qa-e2e-tests"}},
		"separately pinned reusable workflow":                       {args: []string{"--tags", "rc"}, workflow: "jobs:\n  build:\n    uses: stackrox/stackrox/.github/workflows/build.yml@rc\n", ref: "refs/tags/rc", want: []string{"tag trigger unproven"}, absent: []string{"/job/99"}},
		"Konflux image wait on a tag":                               {repo: "stackrox/collector", args: []string{"--tags", "rc"}, workflowName: "Test Konflux builds", workflowPath: ".github/workflows/konflux.yml", workflow: "on:\n  push:\n    tags: ['*']\n", absent: []string{"gh api --method POST", "wait-for-images", "run-konflux-tests", "Test Konflux builds", "Unknown check"}},
		"Konflux tests with a renamed workflow on a PR":             {repo: "stackrox/fact", args: []string{"--prs", "7"}, workflowName: "Image checks", workflowPath: ".github/workflows/konflux-tests.yml", absent: []string{"gh api --method POST", "run-konflux-tests", "Image checks", "Unknown check"}},
		"Konflux tests with a renamed workflow file":                {args: []string{"--prs", "7"}, repo: "stackrox/collector", workflowName: "Test Konflux builds", workflowPath: ".github/workflows/new-path.yml", want: []string{"wait-for-images · Unknown check — patch ci-retest to classify it"}, absent: []string{"gh api --method POST", "Handled by konflux-retest"}},
		"mixed ownership on one tag": {
			args: []string{"--tags", "rc"}, repo: "stackrox/fact", workflowName: "Main CI", workflowPath: ".github/workflows/ci.yml", workflow: "on:\n  push:\n    tags: ['*']\n", mixed: true,
			want:   []string{"gh api --method POST repos/stackrox/fact/actions/jobs/99/rerun", "stackrox/fact / github-actions / .github/workflows/ci.yml / wait-for-images · Unknown check — patch ci-retest to classify it", "commit status (vendor-bot) / vendor/gate · Unknown check"},
			absent: []string{"/jobs/101/rerun", "Red Hat Konflux", "Handled by konflux-retest"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			repo := cmp.Or(tc.repo, "stackrox/stackrox")
			root := "/repos/" + repo
			reads := 0
			event, ref := "pull_request", "feature"
			workflowName := cmp.Or(tc.workflowName, "Scan images for vulnerabilities")
			workflowPath := cmp.Or(tc.workflowPath, ".github/workflows/scan-image-vulnerabilities.yaml")
			if tc.args[0] == "--tags" {
				event, ref = "push", "rc"
				workflowName += " (rc)"
			}
			runData := fmt.Sprintf(`{"id":22,"name":%q,"path":%q,"workflow_id":33,"check_suite_id":11,"head_sha":%q,"head_branch":%q,"event":%q,"run_attempt":1,"run_number":2,"status":"completed","created_at":%q,"pull_requests":[%s],"referenced_workflows":[{"path":"stackrox/stackrox/.github/workflows/build.yml@%s","ref":%q,"sha":%q}]}`, workflowName, workflowPath, sha, ref, event, time.Now().UTC().Format(time.RFC3339), pr, sha, tc.ref, sha)
			first, second := "Konflux: release-central-db:rc", "Upstream: main:rc"
			if tc.workflowName != "" {
				first, second = "wait-for-images", "run-konflux-tests / integration-tests (fedora-coreos)"
			}
			if tc.mixed {
				first, second = "unit-tests / unit-tests (rhcos)", "wait-for-images"
			}
			checks := []string{failedCheck(41, first, "github-actions"), failedCheck(42, second, "github-actions")}
			if tc.mixed {
				checks = append(checks, failedCheck(43, "Red Hat Konflux / fact-on-push", "red-hat-konflux"))
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Errorf("unexpected method %s", r.Method)
				}
				if tc.workflowName != "" && !tc.mixed && strings.HasPrefix(r.URL.Path, root+"/actions/") && r.URL.Path != root+"/actions/runs" {
					t.Errorf("inspected an unsupported or deny-listed Actions run: %s", r.URL.Path)
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
    context: ci/prow/gke-qa-e2e-tests
    rerun_command: /test gke-qa-e2e-tests
    trigger: '(?m)^/test gke-qa-e2e-tests$'
  - name: release-build
    branches: [^release-5$]
    context: ci/prow/release
    rerun_command: /test gke-qa-e2e-tests
    trigger: '(?m)^/test gke-qa-e2e-tests$'
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
					switch {
					case tc.mixed:
						fmt.Fprint(w, `[{"context":"vendor/gate","state":"error","creator":{"login":"vendor-bot"}}]`)
					case tc.args[0] == "--prs" && repo == "stackrox/stackrox":
						fmt.Fprint(w, statuses)
					default:
						fmt.Fprint(w, "[]")
					}
				case "/commits/" + sha + "/check-runs":
					fmt.Fprintf(w, `{"total_count":%d,"check_runs":[%s]}`, len(checks), strings.Join(checks, ","))
				case "/actions/runs":
					fmt.Fprintf(w, `{"total_count":1,"workflow_runs":[%s]}`, runData)
				case "/actions/runs/22":
					fmt.Fprint(w, runData)
				case "/contents/" + workflowPath:
					fmt.Fprintf(w, `{"encoding":"base64","content":%q}`, base64.StdEncoding.EncodeToString([]byte(tc.workflow)))
				case "/actions/workflows/33":
					fmt.Fprint(w, `{"state":"active"}`)
				case "/actions/runs/22/attempts/1/jobs":
					if tc.otherCheck {
						fmt.Fprint(w, strings.ReplaceAll(jobs, "check-runs/4", "check-runs/9"))
						return
					}
					fmt.Fprint(w, strings.ReplaceAll(jobs, "repos/stackrox/stackrox", "repos/"+repo))
				default:
					t.Errorf("unexpected request %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			var out, diag bytes.Buffer
			args := append([]string{"--repo", repo}, tc.args...)
			code := run(t.Context(), args, &out, &diag, func(context.Context, konflux.Lookups) (readers, error) {
				return readers{
					konfluxPlan:   func(context.Context, release.Target) (release.Plan, error) { return release.Plan{}, nil },
					manualRetries: github.New(server.URL, "", server.Client()).ManualRetries,
				}, nil
			})
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
