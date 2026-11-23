package konflux_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	retry "github.com/vikin91/konflux-retest"

	"release-assistant/internal/dispatch"
	"release-assistant/internal/github"
	"release-assistant/internal/konflux"
	"release-assistant/internal/release"
)

const (
	admissionPR     = 23854
	admissionHead   = "c7b9d50a891118421d64c6e5e41e15ae50265305"
	admissionBranch = "alkmim/rox-37740-release-4.11-v1.6.2-bundle-v4"
	admissionMainID = 135671709681
	admissionFooter = "*posted from release-assistant v1.2.3*"
	admissionDenial = "ValidatingAdmissionPolicy 'deny-pipelinerun-create' with binding 'deny-pipelinerun-create' denied request: PipelineRun creation is temporarily disabled"
)

type (
	prAdmissionFixture struct {
		head                     string
		mainCheck                int64
		associatedPR, returnedPR int
		label                    string
		optOut                   bool
		newer                    bool
		prReads                  int
	}
	admissionWriter struct{ writes []release.Proposal }
)

func (w *admissionWriter) Write(_ context.Context, proposal release.Proposal) (dispatch.Delivery, error) {
	w.writes = append(w.writes, proposal)
	return dispatch.Delivery{Status: "sent"}, nil
}

func (f *prAdmissionFixture) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet {
		return nil, fmt.Errorf("unexpected fixture request %s %s", req.Method, req.URL)
	}
	var body string
	switch req.URL.Hostname() + req.URL.Path {
	case "api.github.com/repos/stackrox/stackrox/pulls/23854":
		f.prReads++
		labels := fmt.Sprintf(`{"name":%q}`, f.label)
		if f.optOut {
			labels += `,{"name":"no-release-assistant"}`
		}
		body = fmt.Sprintf(`{"id":5147254021,"number":%d,"state":"open","head":{"sha":%q,"ref":%q},"base":{"ref":"release-4.11"},"labels":[%s]}`,
			f.returnedPR, f.head, admissionBranch, labels)
	case "api.github.com/repos/stackrox/stackrox/commits/" + f.head + "/check-runs":
		check := func(id int64, job string, pr int) string {
			return fmt.Sprintf(`{"id":%d,"app":{"id":296509},"name":"Red Hat Konflux / %s",
				"head_sha":%q,"status":"completed","conclusion":"failure",
				"pull_requests":[{"id":5147254021,"number":%d,"head":{"sha":%q,"ref":%q}}],
				"output":{"title":"pipelinerun start failure","text":%q}}`, id, job, f.head, pr, f.head, admissionBranch, admissionDenial)
		}
		checks := []string{
			check(135671710608, "checks", admissionPR),
			check(f.mainCheck, "main-on-push", f.associatedPR),
			check(135671708354, "operator-bundle-on-push", admissionPR),
		}
		if f.newer {
			checks = append(checks, fmt.Sprintf(`{"id":%d,"app":{"id":296509},"name":"Red Hat Konflux / main-on-push","status":"queued",
				"details_url":"https://konflux-ui.apps.example.openshiftapps.com/ns/builds/pipelinerun/main-retry"}`, f.mainCheck+10))
		}
		body = `{"check_runs":[` + strings.Join(checks, ",") + `]}`
	case "api.example.openshiftapps.com/apis/tekton.dev/v1/namespaces/builds/pipelineruns/main-retry":
		body = fmt.Sprintf(`{"metadata":{"name":"main-retry","namespace":"builds","annotations":{
			"pipelinesascode.tekton.dev/check-run-id":%q,"pipelinesascode.tekton.dev/sha":%q,
			"pipelinesascode.tekton.dev/repo-url":"https://github.com/stackrox/stackrox",
			"pipelinesascode.tekton.dev/event-type":"pull_request","pipelinesascode.tekton.dev/pull-request":"23854",
			"pipelinesascode.tekton.dev/branch":%q}}}`,
			strconv.FormatInt(f.mainCheck+10, 10), f.head, admissionBranch)
	default:
		return nil, fmt.Errorf("unexpected fixture request %s %s", req.Method, req.URL)
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func TestTemporaryAdmissionDenialUsesCLIJobsAndRevalidatesDispatch(t *testing.T) {
	for name, tc := range map[string]struct {
		change      func(*prAdmissionFixture)
		readOnly    bool
		automatic   bool
		unconfirmed bool
		wantWrite   bool
	}{
		"should post after approval":                      {wantWrite: true},
		"should withhold when the PR association changes": {change: func(f *prAdmissionFixture) { f.associatedPR++ }},
		"should withhold when GitHub returns another PR":  {change: func(f *prAdmissionFixture) { f.returnedPR++ }},
		"should withhold when the check ID changes":       {change: func(f *prAdmissionFixture) { f.mainCheck++ }},
		"should withhold when the head changes":           {change: func(f *prAdmissionFixture) { f.head = "new-head" }},
		"should withhold when a newer attempt is queued":  {change: func(f *prAdmissionFixture) { f.newer = true }},
		"should withhold when the PR label is removed":    {change: func(f *prAdmissionFixture) { f.label = "" }},
		"should withhold when the PR opts out":            {change: func(f *prAdmissionFixture) { f.optOut = true }},
		"should withhold in read-only mode":               {readOnly: true},
		"should withhold without an automatic grant":      {automatic: true},
		"should withhold without confirmation":            {unconfirmed: true},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			target := release.Target{Repo: "stackrox/stackrox", PR: admissionPR}
			fixture := &prAdmissionFixture{head: admissionHead, mainCheck: admissionMainID, associatedPR: admissionPR, returnedPR: admissionPR, label: release.PRCommentLabel}
			client := &http.Client{Transport: fixture}
			a := konflux.New(konflux.Config{HTTPClient: client, CommentFooter: admissionFooter, KonfluxToken: func(context.Context, string) (string, error) {
				return "fixture", nil
			}})
			reader := github.New("", "", client)
			identity, err := reader.ReadTarget(ctx, target)
			if err != nil {
				t.Fatal(err)
			}
			cli, err := retry.CheckPR(ctx, retry.Config{HTTPClient: client, VerifyRunIdentity: true}, target.Repo, target.PR)
			if err != nil || !slices.Equal(cli.Jobs, []string{"checks", "main-on-push"}) {
				t.Fatalf("CLI planner jobs = %v, error = %v", cli.Jobs, err)
			}
			plan, err := a.Plan(ctx, target)
			if err != nil || len(plan.Proposals) != len(cli.Jobs) {
				t.Fatalf("adapter proposals = %+v, error = %v", plan.Proposals, err)
			}
			for i, proposal := range plan.Proposals {
				if proposal.Job != cli.Jobs[i] || proposal.Target != target || proposal.SHA != admissionHead ||
					proposal.CommentBody() != "/konflux-retest "+cli.Jobs[i]+"\n\n"+admissionFooter {
					t.Fatalf("incorrect adapter proposal: %+v", proposal)
				}
			}
			proposal := plan.Proposals[1]
			if proposal.RunID != "check:135671709681" {
				t.Fatalf("unexpected proposal evidence: %+v", proposal)
			}
			for _, run := range plan.Runs {
				if run.ID != "" || run.Image != (release.Image{}) {
					t.Fatalf("check evidence was promoted to PipelineRun or image evidence: %+v", run)
				}
			}

			now := time.Now()
			writer := &admissionWriter{}
			d := dispatch.New(reader, a, writer, dispatch.Options{EnableWrites: !tc.readOnly, Threshold: 2, Now: func() time.Time { return now }})
			d.SetWriteMode(new(!tc.automatic), new(true))
			d.ObserveIdentity(identity)
			d.ObservePlan(plan)
			if tc.change != nil {
				tc.change(fixture)
			}
			if fixture.newer {
				fresh, err := a.Plan(ctx, target)
				if err != nil || len(fresh.Proposals) != 1 || fresh.Proposals[0].Job != "checks" ||
					!slices.ContainsFunc(fresh.Runs, func(run release.Run) bool {
						return run.Job == proposal.Job && run.ID == strconv.FormatInt(fixture.mainCheck+10, 10) && run.Status == "queued"
					}) {
					t.Fatalf("newer verified attempt did not withdraw the job: %+v, error = %v", fresh, err)
				}
			}
			reads := fixture.prReads
			_, err = d.Send(ctx, target, dispatch.ID(proposal), !tc.automatic && !tc.unconfirmed)
			if !tc.wantWrite {
				if err == nil || len(writer.writes) != 0 {
					t.Fatalf("unsafe dispatch: writes = %v, error = %v", writer.writes, err)
				}
				return
			}
			if err != nil || fixture.prReads != reads+2 || !slices.Equal(writer.writes, []release.Proposal{proposal}) {
				t.Fatalf("dispatch did not revalidate and post the exact proposal: reads = %d, writes = %v, error = %v", fixture.prReads-reads, writer.writes, err)
			}
			if _, err := d.Send(ctx, target, dispatch.ID(plan.Proposals[0]), true); err != nil {
				t.Fatalf("another job on the same target should be eligible: %v", err)
			}
			now = now.Add(time.Minute)
			if _, err := d.Send(ctx, target, dispatch.ID(proposal), true); err == nil {
				t.Fatal("duplicate check comment was allowed after cooldown")
			}

			fixture.mainCheck++
			fresh, err := a.Plan(ctx, target)
			if err != nil || len(fresh.Proposals) != 2 {
				t.Fatalf("new planner result: %+v, error = %v", fresh, err)
			}
			d.ObservePlan(fresh)
			d.ObservePlan(fresh)
			offer := d.Views()[0].Offers[1]
			if offer.Failures != 2 || offer.CanApprove || !offer.WaitingUntil.Equal(now.Add(59*time.Minute)) {
				t.Fatalf("expected slow cooldown for two distinct failed checks: %+v", offer)
			}
			if _, err := d.Send(ctx, target, offer.ID, true); err == nil || len(writer.writes) != 2 {
				t.Fatalf("cooldown or duplicate called the writer: writes = %v, error = %v", writer.writes, err)
			}
		})
	}
}
