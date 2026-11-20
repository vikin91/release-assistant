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

	"release-assistant/internal/app"
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
			got, err := targets(tc.args, new(bytes.Buffer), &cliOptions{lookups: make(konflux.Lookups)})
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
		"tag commit comment": {[]string{"-v", "--tags", "5.0.1-rc.0"}, "https://github.com/" + repo + "/commit/" + sha, "/retest central-db-on-push tag:5.0.1-rc.0", sha},
		"moved PR":           {[]string{"--prs", "7"}, "https://github.com/" + repo + "/pull/7", "/konflux-retest central-db-on-push", "new-head"},
	} {
		t.Run(name, func(t *testing.T) {
			var out, diagnostics bytes.Buffer
			var planned bool
			read := func(_ context.Context, _ konflux.Lookups, log *app.Log) (readers, error) {
				log.CallingAPI("GitHub", "GET", "/repos/"+repo)
				return readers{
					konfluxPlan: func(_ context.Context, target release.Target) (release.Plan, error) {
						planned = true
						return release.Plan{Target: target, SHA: sha, Evidence: release.Evidence{URL: tc.url},
							Proposals: []release.Proposal{{Target: target, SHA: sha, Payload: tc.comment}}}, nil
					},
					manualRetries: func(_ context.Context, target release.Target, _ bool) (release.ManualRetryReport, error) {
						if !planned {
							t.Fatal("CI checked the target before Konflux")
						}
						return release.ManualRetryReport{Target: target, SHA: tc.manualSHA, URL: tc.url, Jobs: []release.ManualJob{{ID: 17, Name: "scan"}}}, nil
					},
				}, nil
			}
			code := run(t.Context(), tc.args, &out, &diagnostics, read)
			if got := strings.Contains(diagnostics.String(), "Debug: Calling API:"); got != (tc.args[0] == "-v") {
				t.Fatalf("unexpected debug output for %v: %q", tc.args, diagnostics.String())
			}
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

func TestKonfluxIdentityWarningWithoutPlannerError(t *testing.T) {
	const warning = "identity unknown: PipelineRun event identity unavailable"
	var out, diagnostics bytes.Buffer
	code := run(t.Context(), []string{"--prs", "7"}, &out, &diagnostics, func(context.Context, konflux.Lookups, *app.Log) (readers, error) {
		return readers{
			konfluxPlan: func(_ context.Context, target release.Target) (release.Plan, error) {
				return release.Plan{Target: target, SHA: "head", Action: "Wait", Warnings: []string{warning}}, nil
			},
			manualRetries: func(_ context.Context, target release.Target, _ bool) (release.ManualRetryReport, error) {
				return release.ManualRetryReport{Target: target, SHA: "head", URL: "https://github.com/stackrox/stackrox/pull/7"}, nil
			},
		}, nil
	})
	if code != 0 || diagnostics.Len() != 0 || !strings.Contains(out.String(), "Next action: Investigate") ||
		!strings.Contains(out.String(), "Konflux: "+warning) {
		t.Fatalf("code=%d, output=%s, diagnostics=%s", code, &out, &diagnostics)
	}
}

func TestCLIReports(t *testing.T) {
	const sha = "1111111111111111111111111111111111111111"
	const pr = `{"state":"open","number":7,"merge_commit_sha":"merge-revision","base":{"sha":"base","ref":"master","repo":{"id":1}},"head":{"sha":"` + sha + `","repo":{"id":2}}}`
	const statuses = `[{"context":"ci/prow/Green","state":"success"},{"context":"ci/prow/green","state":"failure","creator":{"id":75433959},"target_url":"https://prow.ci.openshift.org/view/gs/test-platform-results-public/pr-logs/pull/stackrox_stackrox/7/green/111"},{"context":"ci/prow/gke-qa-e2e-tests","state":"failure","creator":{"id":75433959},"target_url":"https://prow.ci.openshift.org/view/gs/test-platform-results-public/pr-logs/pull/stackrox_stackrox/7/build/123"},{"context":"ci/prow/test","state":"pending"},{"context":"ci/prow/test","state":"failure","creator":{"id":75433959},"target_url":"https://prow.ci.openshift.org/view/gs/test-platform-results-public/pr-logs/pull/stackrox_stackrox/7/test/345"}]`
	failedCheck := func(id int, name, app string, appID int) string {
		return fmt.Sprintf(`{"id":%d,"name":%q,"status":"completed","conclusion":"failure","head_sha":%q,"app":{"slug":%q,"id":%d},"check_suite":{"id":11}}`, id, name, sha, app, appID)
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

	const mergeWorkflow = `jobs:
  gate:
    name: merge gate
    needs: [intermediate]
  intermediate:
    needs: compile
  compile:
    runs-on: ubuntu-latest
  test:
    name: "integration-tests / amd64-integration-tests (rhel) / Testing rhel"
`
	for name, tc := range map[string]struct {
		args                             []string
		workflow, ref                    string
		repo, workflowName, workflowPath string
		mixed, optional, mergeScope      bool
		greenGate, rulesUnavailable      bool
		moved, otherCheck, missingConfig bool
		newerRunPR                       int
		rerunConclusion                  string
		attemptMoved                     bool
		imageWait                        bool
		buildConclusion                  string
		want, absent                     []string
		code                             int
	}{
		"PR reports both actions and continues past missing target": {args: []string{"--prs", "8,7", "7"}, want: []string{"HTTP 404", jobOutput + "\n\nFailed Prow checks:", "Failed Prow checks: post this comment on https://github.com/stackrox/stackrox/pull/7", "\n/test gke-qa-e2e-tests\n"}, absent: []string{"ci/prow/test", "/test green", "/test pending", "\n/retest\n"}, code: 1},
		"Prow config unavailable preserves Actions offers":          {args: []string{"--prs", "7"}, missingConfig: true, want: []string{jobOutput, "reading OpenShift CI presubmits"}, absent: []string{"Failed Prow checks:", "/test gke-qa-e2e-tests", "\n/retest\n"}, code: 1},
		"PR head moved after observation":                           {args: []string{"--prs", "7"}, moved: true, want: []string{"target identity could not be reconfirmed"}, absent: []string{"/job/99", "\n/retest\n", "/test gke-qa-e2e-tests"}, code: 1},
		"job for different check":                                   {args: []string{"--prs", "7"}, otherCheck: true, want: []string{"no unique failed job", "\n/test gke-qa-e2e-tests\n"}, absent: []string{"/job/99"}},
		"tag only push":                                             {optional: true, args: []string{"--tags", "rc"}, workflow: "on:\n  push:\n    tags: ['*']\n", want: []string{"Tag: rc", "/commit/" + sha, jobOutput + "\n\nWarnings:\nThis tool cannot tell flakes from true failures. Inspect each failure before retrying.\n"}, absent: []string{"\n/retest\n"}},
		"tag proven by local workflow":                              {optional: true, args: []string{"--tags", "rc"}, workflow: "jobs:\n  build:\n    uses: ./.github/workflows/build.yml\n", ref: "refs/tags/rc", want: []string{"/job/99"}},
		"same name branch push":                                     {optional: true, args: []string{"--tags", "rc"}, workflow: "jobs:\n  build:\n    uses: ./.github/workflows/build.yml\n", ref: "refs/heads/rc", want: []string{"tag trigger unproven"}, absent: []string{"/job/99", "\n/retest\n", "/test gke-qa-e2e-tests"}},
		"separately pinned reusable workflow":                       {optional: true, args: []string{"--tags", "rc"}, workflow: "jobs:\n  build:\n    uses: stackrox/stackrox/.github/workflows/build.yml@rc\n", ref: "refs/tags/rc", want: []string{"tag trigger unproven"}, absent: []string{"/job/99"}},
		"Konflux image wait on a tag":                               {repo: "stackrox/collector", optional: true, args: []string{"--tags", "rc"}, workflowName: "Test Konflux builds", workflowPath: ".github/workflows/konflux.yml", workflow: "on:\n  push:\n    tags: ['*']\n", absent: []string{"gh api --method POST", "wait-for-images", "run-konflux-tests", "Test Konflux builds", "Unknown check"}},
		"Konflux tests with a renamed workflow on a PR":             {repo: "stackrox/fact", args: []string{"--prs", "7"}, workflowName: "Image checks", workflowPath: ".github/workflows/konflux-tests.yml", absent: []string{"gh api --method POST", "run-konflux-tests", "Image checks", "Unknown check"}},
		"Konflux tests with a renamed workflow file":                {args: []string{"--prs", "7"}, repo: "stackrox/collector", workflowName: "Test Konflux builds", workflowPath: ".github/workflows/new-path.yml", want: []string{"wait-for-images · Unknown check — patch ci-retest to classify it"}, absent: []string{"gh api --method POST", "Handled by konflux-retest"}},
		"only configured optional jobs on a tag": {
			optional: true, args: []string{"--tags", "rc"}, repo: "stackrox/fact", workflowName: "Main CI", workflowPath: ".github/workflows/ci.yml", workflow: "on:\n  push:\n    tags: ['*']\n", mixed: true,
			want:   []string{"gh api --method POST repos/stackrox/fact/actions/jobs/99/rerun"},
			absent: []string{"/jobs/101/rerun", "Red Hat Konflux", "Handled by konflux-retest", "Unknown check", "vendor/gate"},
		},
		"tag without optional has no manual CI": {args: []string{"--tags", "rc"}, absent: []string{"gh api --method", "Unknown check", "Warnings:"}},
		"PR defaults to the required job's transitive prerequisite": {
			repo: "stackrox/collector", args: []string{"--prs", "7"}, workflowPath: ".github/workflows/main.yml", workflow: mergeWorkflow, mergeScope: true,
			want: []string{"actions/jobs/99/rerun"}, absent: []string{"actions/jobs/101/rerun", "Unknown check"},
		},
		"optional adds a supported unrelated job to the same PR": {
			repo: "stackrox/collector", args: []string{"--prs", "7"}, workflowPath: ".github/workflows/main.yml", workflow: mergeWorkflow, mergeScope: true, optional: true,
			want: []string{"actions/jobs/99/rerun", "actions/jobs/101/rerun"}, absent: []string{"Unknown check"},
		},
		"Prow presubmit outside merge scope is silent": {
			args: []string{"--prs", "7"}, mergeScope: true, absent: []string{"/test gke-qa-e2e-tests", "Unknown check", "gh api --method"},
		},
		"optional includes configured Prow presubmit": {
			args: []string{"--prs", "7"}, mergeScope: true, optional: true, want: []string{"/test gke-qa-e2e-tests"}, absent: []string{"Unknown check"},
		},
		"passed gate needs no upstream retries": {
			repo: "stackrox/collector", args: []string{"--prs", "7"}, workflowPath: ".github/workflows/main.yml", workflow: mergeWorkflow, mergeScope: true, greenGate: true,
			absent: []string{"gh api --method POST", "Unknown check", "Warnings:"},
		},
		"should omit a cancelled prerequisite when a newer run for this PR passed it": {
			repo: "stackrox/collector", args: []string{"--prs", "7"}, workflowPath: ".github/workflows/main.yml", workflow: mergeWorkflow, mergeScope: true, newerRunPR: 7,
			absent: []string{"gh api --method POST", "Unknown check", "Warnings:"},
		},
		"should keep a cancelled prerequisite when the newer run belongs to another PR": {
			repo: "stackrox/collector", args: []string{"--prs", "7"}, workflowPath: ".github/workflows/main.yml", workflow: mergeWorkflow, mergeScope: true, newerRunPR: 8,
			want: []string{"compile: ineligible Actions conclusion cancelled"}, absent: []string{"gh api --method POST"},
		},
		"should omit a cancelled check when the latest attempt passed it": {
			repo: "stackrox/collector", args: []string{"--prs", "7"}, workflowPath: ".github/workflows/main.yml", workflow: mergeWorkflow, mergeScope: true, rerunConclusion: "success",
			absent: []string{"gh api --method POST", "Unknown check", "Warnings:"},
		},
		"should offer the failed job from the latest attempt": {
			repo: "stackrox/collector", args: []string{"--prs", "7"}, workflowPath: ".github/workflows/main.yml", workflow: mergeWorkflow, mergeScope: true, rerunConclusion: "failure",
			want: []string{"actions/runs/22/job/103", "actions/jobs/103/rerun"}, absent: []string{"cancelled", "/jobs/99/rerun"},
		},
		"should omit prerequisite retries when the latest required check passed": {
			repo: "stackrox/collector", args: []string{"--prs", "7"}, workflowPath: ".github/workflows/main.yml", workflow: mergeWorkflow, mergeScope: true, rerunConclusion: "failure", greenGate: true,
			absent: []string{"gh api --method POST", "Unknown check", "Warnings:"},
		},
		"should withhold retry results when the attempt changes during collection": {
			repo: "stackrox/collector", args: []string{"--prs", "7"}, workflowPath: ".github/workflows/main.yml", workflow: mergeWorkflow, mergeScope: true, rerunConclusion: "failure", attemptMoved: true,
			want: []string{"run changed during collection"}, absent: []string{"cancelled", "/rerun"},
		},
		"should point to the latest failed Build instead of retrying the image wait": {
			args: []string{"--prs", "7"}, workflowPath: ".github/workflows/e2e-dispatch.yaml", imageWait: true, buildConclusion: "failure",
			want: []string{"wait-for-images: inspect the Build run for this commit (failure)", "https://github.com/stackrox/stackrox/actions/runs/25"}, absent: []string{"Unknown check", "/jobs/99/rerun"},
		},
		"should retry the failed image wait after Build succeeds": {
			args: []string{"--prs", "7"}, workflowPath: ".github/workflows/e2e-dispatch.yaml", imageWait: true, buildConclusion: "success",
			want: []string{"wait-for-images\nhttps://github.com/stackrox/stackrox/actions/runs/22/job/99", "/jobs/99/rerun"}, absent: []string{"Unknown check"},
		},
		"should withhold an image wait retry when Build is unavailable": {
			args: []string{"--prs", "7"}, workflowPath: ".github/workflows/e2e-dispatch.yaml", imageWait: true,
			want: []string{"wait-for-images: cannot verify the Build run for this commit"}, absent: []string{"Unknown check", "/jobs/99/rerun"},
		},
		"unavailable rules do not promote optional failures": {
			args: []string{"--prs", "7"}, rulesUnavailable: true, want: []string{"required checks for master"}, absent: []string{"gh api --method POST", "/test gke-qa-e2e-tests", "Unknown check"}, code: 1,
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
			created := time.Now().UTC().Format(time.RFC3339)
			attempt := 1
			if tc.rerunConclusion != "" {
				attempt = 2
			}
			workflowRun := func(id, suite, number int, pullRequest string) string {
				return fmt.Sprintf(`{"id":%d,"name":%q,"path":%q,"workflow_id":33,"check_suite_id":%d,"head_sha":%q,"head_branch":%q,"event":%q,"run_attempt":%d,"run_number":%d,"status":"completed","created_at":%q,"pull_requests":[%s],"referenced_workflows":[{"path":"stackrox/stackrox/.github/workflows/build.yml@%s","ref":%q,"sha":%q}]}`, id, workflowName, workflowPath, suite, sha, ref, event, attempt, number, created, pullRequest, sha, tc.ref, sha)
			}
			runData := workflowRun(22, 11, 2, pr)
			runs := []string{runData}
			if tc.buildConclusion != "" {
				buildRun := func(id, number int, conclusion string) string {
					return fmt.Sprintf(`{"id":%d,"name":"Build","path":".github/workflows/build.yaml","workflow_id":34,"check_suite_id":%d,"head_sha":%q,"head_branch":"feature","event":"pull_request","run_attempt":1,"run_number":%d,"status":"completed","conclusion":%q,"pull_requests":[%s]}`, id, id+10, sha, number, conclusion, pr)
				}
				runs = append(runs, buildRun(25, 2, tc.buildConclusion), buildRun(24, 1, "success"))
			}
			first, second := "Konflux: release-central-db:rc", "Upstream: main:rc"
			if tc.workflowName != "" {
				first, second = "wait-for-images", "run-konflux-tests / integration-tests (fedora-coreos)"
			}
			if tc.mixed {
				first, second = "unit-tests / unit-tests (rhcos)", "wait-for-images"
			}
			if tc.imageWait {
				first, second = "wait-for-images", "unrelated job"
			}
			if tc.mergeScope {
				first, second = "compile", "integration-tests / amd64-integration-tests (rhel) / Testing rhel"
			}
			checks := []string{failedCheck(41, first, "github-actions", 15368), failedCheck(42, second, "github-actions", 15368)}
			if tc.newerRunPR > 0 {
				checks[0] = strings.Replace(checks[0], `"conclusion":"failure"`, `"conclusion":"cancelled"`, 1)
				checks = append(checks, fmt.Sprintf(`{"id":44,"name":%q,"status":"completed","conclusion":"success","head_sha":%q,"app":{"slug":"github-actions","id":15368},"check_suite":{"id":12}}`, first, sha))
				newerPR := strings.Replace(pr, `"number":7`, fmt.Sprintf(`"number":%d`, tc.newerRunPR), 1)
				runs = append(runs, workflowRun(23, 12, 3, newerPR))
			}
			if tc.rerunConclusion != "" {
				checks[0] = strings.Replace(checks[0], `"conclusion":"failure"`, `"conclusion":"cancelled"`, 1)
				checks = append(checks, strings.Replace(failedCheck(44, first, "github-actions", 15368), `"conclusion":"failure"`, `"conclusion":"`+tc.rerunConclusion+`"`, 1))
			}
			if tc.greenGate {
				checks = append(checks, strings.Replace(failedCheck(45, "merge gate", "github-actions", 15368), `"conclusion":"failure"`, `"conclusion":"success"`, 1))
			}
			if tc.mixed {
				checks = append(checks, failedCheck(43, "Red Hat Konflux / fact-on-push", "red-hat-konflux", 296509))
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Errorf("unexpected method %s", r.Method)
				}
				if tc.workflowName != "" && !tc.mixed && strings.HasPrefix(r.URL.Path, root+"/actions/") && r.URL.Path != root+"/actions/runs" {
					t.Errorf("inspected an unsupported or deny-listed Actions run: %s", r.URL.Path)
				}
				switch strings.TrimPrefix(r.URL.Path, root) {
				case "/branches/master":
					fmt.Fprint(w, `{"protection":{"required_status_checks":{"enforcement_level":"off"}}}`)
				case "/branches/master/protection/required_status_checks":
					http.NotFound(w, r)
				case "/rules/branches/master":
					if tc.rulesUnavailable {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					if tc.mergeScope {
						fmt.Fprint(w, `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"merge gate","integration_id":15368}]}}]`)
						return
					}
					fmt.Fprint(w, `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"Konflux: release-central-db:rc"},{"context":"Upstream: main:rc"},{"context":"ci/prow/gke-qa-e2e-tests"},{"context":"wait-for-images"},{"context":"run-konflux-tests / integration-tests (fedora-coreos)"}]}}]`)
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
					fmt.Fprintf(w, `{"total_count":%d,"workflow_runs":[%s]}`, len(runs), strings.Join(runs, ","))
				case "/actions/runs/22":
					if tc.attemptMoved {
						fmt.Fprint(w, strings.Replace(runData, `"run_attempt":2`, `"run_attempt":3`, 1))
						return
					}
					fmt.Fprint(w, runData)
				case "/actions/runs/22/jobs", "/actions/runs/22/attempts/2/jobs":
					if r.URL.Path == root+"/actions/runs/22/jobs" && r.URL.Query().Get("filter") != "latest" {
						t.Errorf("unexpected workflow jobs filter in %s", r.URL)
					}
					latestJobs := []string{fmt.Sprintf(`{"id":103,"status":"completed","conclusion":%q,"check_run_url":"https://api.github.com/repos/%s/check-runs/44"}`, tc.rerunConclusion, repo)}
					if tc.greenGate {
						latestJobs = append(latestJobs, fmt.Sprintf(`{"id":104,"status":"completed","conclusion":"success","check_run_url":"https://api.github.com/repos/%s/check-runs/45"}`, repo))
					}
					fmt.Fprintf(w, `{"total_count":%d,"jobs":[%s]}`, len(latestJobs), strings.Join(latestJobs, ","))
				case "/contents/" + workflowPath:
					if tc.mergeScope && r.URL.Query().Get("ref") != "merge-revision" {
						t.Errorf("workflow read at %s", r.URL.RawQuery)
					}
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
			args := []string{"--repo", repo}
			if tc.optional {
				args = append(args, "--optional")
			}
			args = append(args, tc.args...)
			code := run(t.Context(), args, &out, &diag, func(context.Context, konflux.Lookups, *app.Log) (readers, error) {
				return readers{
					konfluxPlan:   func(context.Context, release.Target) (release.Plan, error) { return release.Plan{}, nil },
					manualRetries: github.New(server.URL, "", server.Client()).ManualRetriesWithOptional,
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
