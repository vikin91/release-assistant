package app

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	retry "github.com/vikin91/konflux-retest"

	"release-assistant/internal/dispatch"
	"release-assistant/internal/konflux"
)

type missingBuildFixture struct {
	mainTrigger    bool
	checkInventory string
	checkReads     int
}

func (f *missingBuildFixture) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet {
		return nil, fmt.Errorf("unexpected fixture request %s %s", req.Method, req.URL)
	}
	const contents = "api.github.com/repos/contributor/stackrox/contents/.tekton/"
	status := http.StatusOK
	var body string
	switch address := req.URL.Hostname() + req.URL.Path; {
	case address == "api.github.com/repos/stackrox/stackrox/pulls/123":
		body = `{"id":1234,"number":123,"state":"open","head":{"sha":"pin-head","ref":"topic","repo":{"full_name":"contributor/stackrox"}}}`
	case address == "api.github.com/repos/stackrox/stackrox/commits/pin-head/check-runs":
		f.checkReads++
		body = f.checkInventory
	case strings.HasPrefix(address, contents) && req.URL.Query().Get("ref") == "pin-head":
		file := strings.TrimPrefix(address, contents)
		job := strings.TrimSuffix(file, "-build.yaml") + "-on-push"
		if file != "central-db-build.yaml" && (file != "main-build.yaml" || !f.mainTrigger) {
			status = http.StatusNotFound
			break
		}
		manifest := fmt.Sprintf("apiVersion: tekton.dev/v1\nkind: PipelineRun\nmetadata:\n  name: %s\n  annotations:\n    pipelinesascode.tekton.dev/on-comment: '^/konflux-retest %s$'\n", job, job)
		body = fmt.Sprintf(`{"encoding":"base64","content":%q}`, base64.StdEncoding.EncodeToString([]byte(manifest)))
	case address == "api.example.openshiftapps.com/apis/tekton.dev/v1/namespaces/builds/pipelineruns/main":
		body = `{"metadata":{"name":"main","namespace":"builds","annotations":{
			"pipelinesascode.tekton.dev/check-run-id":"88","pipelinesascode.tekton.dev/sha":"pin-head",
			"pipelinesascode.tekton.dev/repo-url":"https://github.com/stackrox/stackrox",
			"pipelinesascode.tekton.dev/event-type":"pull_request","pipelinesascode.tekton.dev/pull-request":"123",
			"pipelinesascode.tekton.dev/branch":"topic"}}}`
	default:
		return nil, fmt.Errorf("unexpected fixture request %s %s", req.Method, req.URL)
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func TestVerifiedMissingBuildsUseRetestQueueApproval(t *testing.T) {
	const queued = `{"check_runs":[{"id":88,"app":{"id":296509},"name":"Red Hat Konflux / main-on-push","status":"queued",
		"details_url":"https://konflux-ui.apps.example.openshiftapps.com/ns/builds/pipelinerun/main"}]}`
	for name, tc := range map[string]struct {
		change    func(*missingBuildFixture, *pinningServices)
		readOnly  bool
		wantWrite bool
	}{
		"should send the approved start comment": {wantWrite: true},
		"should withhold after the build appears": {change: func(f *missingBuildFixture, _ *pinningServices) {
			f.checkInventory = queued
		}},
		"should withhold after trigger verification is lost": {change: func(f *missingBuildFixture, _ *pinningServices) {
			f.mainTrigger = false
		}},
		"should withhold without a current check inventory": {change: func(f *missingBuildFixture, _ *pinningServices) {
			f.checkInventory = `{}`
		}},
		"should withhold on a changed head": {change: func(_ *missingBuildFixture, services *pinningServices) {
			services.identity.SHA = "new-head"
		}},
		"should withhold after the required label is removed": {change: func(_ *missingBuildFixture, services *pinningServices) {
			services.identity.CommentLabel = false
		}},
		"should show the request in read-only mode": {readOnly: true},
	} {
		t.Run(name, func(t *testing.T) {
			const footer = "*posted from release-assistant v1.2.3*"
			s, services, config := newPinningSession(t)
			fixture := &missingBuildFixture{mainTrigger: true, checkInventory: `{"check_runs":[]}`}
			client := &http.Client{Transport: fixture}
			planner := konflux.New(konflux.Config{HTTPClient: client, CommentFooter: footer, KonfluxToken: func(context.Context, string) (string, error) {
				return "fixture", nil
			}})
			s.planner = planner
			services.identity.CommentLabel = true
			acs := services.repos["acs"]
			acs.PRs[0].CommentLabel = true
			acs.PinningPR.CommentLabel = true
			services.repos["acs"] = acs
			s.dispatcher = dispatch.New(services, planner, services, dispatch.Options{EnableWrites: !tc.readOnly})
			s.dispatcher.SetWriteMode(nil, new(true))
			target := config.PinningTarget()
			cliPlan, err := retry.CheckPR(t.Context(), retry.Config{
				HTTPClient: client, VerifyRunIdentity: true, VerifyMissingPRTriggers: true,
			}, target.Repo, target.PR)
			if err != nil || !slices.Equal(cliPlan.MissingPRJobs, []string{"central-db-on-push", "main-on-push"}) {
				t.Fatalf("CLI planner starts = %v, error = %v", cliPlan.MissingPRJobs, err)
			}
			if err := s.Refresh(t.Context(), s.Snapshot().Generation, config.ACS(), true); err != nil {
				t.Fatal(err)
			}
			before := s.Snapshot()
			if services.writes != 0 || len(before.RetestQueue) != len(cliPlan.MissingPRJobs) ||
				len(before.Plans) != 1 || len(before.Plans[0].Runs) != 0 || len(before.Plans[0].Proposals) != len(before.RetestQueue) ||
				len(before.Authority) != 1 || len(before.Authority[0].Offers) != len(before.RetestQueue) {
				t.Fatalf("expected two pending starts without PipelineRun evidence: %+v", before.RetestQueue)
			}
			for i, item := range before.RetestQueue {
				proposal := before.Plans[0].Proposals[i]
				if item.Job != cliPlan.MissingPRJobs[i] || !item.MissingBuild || item.Kind != "konflux" ||
					!item.MatchesProposal(proposal) || (item.Blocked != "") != tc.readOnly ||
					proposal.CommentBody() != "/konflux-retest "+item.Job+"\n\n"+footer || before.Authority[0].Offers[i].Failures != 0 {
					t.Fatalf("unexpected start request: %+v, proposal: %+v", item, proposal)
				}
			}
			if tc.change != nil {
				tc.change(fixture, services)
			}
			if fixture.checkInventory == queued {
				observed, err := planner.Plan(t.Context(), target)
				if err != nil || len(observed.Runs) != 1 || observed.Runs[0].ID != "88" || observed.Runs[0].Status != "queued" {
					t.Fatalf("new build identity was not verified: runs=%+v, error=%v", observed.Runs, err)
				}
			}
			queueID := before.RetestQueue[1].ID
			reads := fixture.checkReads
			send := Action{Kind: "queue-send", QueueID: queueID, Generation: before.Generation}
			err = s.Apply(t.Context(), send)
			if !tc.wantWrite {
				if err == nil || services.writes != 0 {
					t.Fatalf("unsafe start reached the writer: calls=%d, error=%v", services.writes, err)
				}
				if !tc.readOnly && fixture.checkReads != reads+1 {
					t.Fatal("denial did not recheck the planner")
				}
				return
			}
			if err != nil || fixture.checkReads != reads+1 || services.writes != 1 || len(s.Snapshot().RetestQueue) != 1 {
				t.Fatalf("approval did not revalidate and send one comment: calls=%d, error=%v", services.writes, err)
			}
			if err := s.Apply(t.Context(), send); err == nil || services.writes != 1 {
				t.Fatal("a confirmed start comment could be sent again")
			}
		})
	}
}
