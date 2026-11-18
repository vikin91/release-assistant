package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"release-assistant/internal/konflux"
	"release-assistant/internal/release"
)

func creationFailure(checkID int64) release.StartFailure {
	return release.StartFailure{CheckID: checkID, Job: "main-on-push", Cluster: "example.openshiftapps.com", Category: "admission_policy", Reason: "ValidatingAdmissionPolicy deny-pipelinerun-create denied request", Evidence: observed()}
}

func TestCreationFailureSnapshotsAndFailedReads(t *testing.T) {
	for name, withSHA := range map[string]bool{"failed target read": false, "partial read at known commit": true} {
		t.Run(name, func(t *testing.T) {
			s, f, config := newPinningSession(t)
			f.plan.StartFailures = []release.StartFailure{creationFailure(42)}
			f.plan.Proposals, f.plan.Runs, f.plan.Action = nil, nil, "Wait"
			original := clonePlan(f.plan)
			if err := s.Refresh(t.Context(), s.generation, config.ACS(), true); err != nil {
				t.Fatal(err)
			}
			x := s.Snapshot()
			x.Plans[0].StartFailures[0].Reason = "mutated snapshot"
			if s.Snapshot().Plans[0].StartFailures[0].Reason == "mutated snapshot" {
				t.Fatal("snapshot exposed mutable creation failure evidence")
			}
			f.plan = release.Plan{Target: config.PinningTarget()}
			if withSHA {
				f.plan.SHA = original.SHA
			}
			f.planErr = errors.New("GitHub unavailable")
			if err := s.Refresh(t.Context(), s.generation, config.ACS(), true); err == nil {
				t.Fatal("read failure was hidden")
			}
			retained := s.Snapshot().Plans[0].StartFailures
			if len(retained) != 1 || retained[0].Evidence.Fresh() || retained[0].Evidence.Error == "" || !retained[0].Evidence.At.Equal(original.StartFailures[0].Evidence.At) {
				t.Fatalf("earlier creation failure was lost or remained current: %+v", retained)
			}
			f.plan, f.planErr = original, nil
			if err := s.Refresh(t.Context(), s.generation, config.ACS(), true); err != nil {
				t.Fatal(err)
			}
			warnings := 0
			for _, entry := range s.Snapshot().LogEntries {
				if entry.Decision == "start-failures" {
					warnings++
				}
			}
			if warnings != 1 {
				t.Fatalf("same historical check was announced %d times", warnings)
			}
		})
	}
}

func TestCreationFailuresBecomeStaleWhenPRIdentityChanges(t *testing.T) {
	s, f, config := newPinningSession(t)
	f.plan.StartFailures = []release.StartFailure{creationFailure(42)}
	f.plan.Proposals, f.plan.Runs = nil, nil
	if err := s.Refresh(t.Context(), s.generation, config.ACS(), true); err != nil {
		t.Fatal(err)
	}
	acs := f.repos["acs"]
	acs.PRs[0].SHA = "new-head"
	acs.PinningPR.SHA = "new-head"
	f.repos["acs"] = acs
	if err := s.refresh(t.Context(), s.generation, config.ACS(), true, sourceWork{scope: ReadPins | ReadTargets, revision: s.workRevision}); err != nil {
		t.Fatal(err)
	}
	failure := s.Snapshot().Plans[0].StartFailures[0]
	if failure.Evidence.Fresh() || !strings.Contains(failure.Evidence.Error, "Current PR identity") {
		t.Fatalf("old commit check remained current after PR head change: %+v", failure)
	}
}

func TestCreationFailureLogsOnlyNewActiveCheckReports(t *testing.T) {
	s, _ := newFixture()
	var output bytes.Buffer
	s.log = NewLog(&output)
	plan := release.Plan{Target: release.Target{Repo: "stackrox/stackrox", PR: 3}, SHA: "commit", StartFailures: []release.StartFailure{creationFailure(42), creationFailure(43), creationFailure(42)}}
	s.recordStartFailures(release.Plan{}, plan)
	fresh := clonePlan(plan)
	for i := range fresh.StartFailures {
		fresh.StartFailures[i].Evidence.At = time.Now().Add(time.Hour)
	}
	s.recordStartFailures(plan, fresh)
	fresh.StartFailures = fresh.StartFailures[:1]
	s.recordStartFailures(plan, fresh)
	fresh.StartFailures[0].Superseded = true
	s.recordStartFailures(plan, fresh)
	if strings.Count(output.String(), `GitHub check 43 reports: "ValidatingAdmissionPolicy deny-pipelinerun-create denied request".`) != 1 {
		t.Fatalf("reported cause was missing or repeated: %s", output.String())
	}
	if strings.Count(output.String(), "Warn:") != 1 || !strings.Contains(output.String(), "Observed 2 PipelineRun creation failures") || !strings.Contains(output.String(), "cluster example.openshiftapps.com; category admission policy rejection") || !strings.Contains(output.String(), "Current cluster availability is unknown") {
		t.Fatalf("creation failure logs repeated polls or inferred health: %s", output.String())
	}
	fresh.StartFailures = append(fresh.StartFailures, creationFailure(44))
	s.recordStartFailures(plan, fresh)
	if strings.Count(output.String(), "Warn:") != 2 {
		t.Fatal("a new check report did not produce a grouped transition")
	}
}

type advisoryTransport func(*http.Request) (*http.Response, error)

func (f advisoryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestDeveloperConformaAdvisoryDoesNotBackOffMonitoring(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := newFixture()
		config := configuration()
		fact := f.repos["fact"]
		target := release.Target{Repo: fact.Source.Repo, PR: 3}
		fact.PRs = []release.PR{{Target: target, SHA: "head", State: "open", Base: fact.Source.Branch, Evidence: observed()}}
		f.repos["fact"] = fact
		apply(t, s, Action{Kind: "configure", Config: config})
		refreshAll(t, s)
		apply(t, s, Action{Kind: "choice", Target: target, Choice: release.Included})
		calls := 0
		s.planner = konflux.New(konflux.Config{HTTPClient: &http.Client{Transport: advisoryTransport(func(req *http.Request) (*http.Response, error) {
			calls++
			body := `{"state":"open","head":{"sha":"head"}}`
			if strings.HasSuffix(req.URL.Path, "/check-runs") {
				body = `{"check_runs":[{"id":44,"name":"Red Hat Konflux / acs-conforma-dev-4-10 / fact-4-10","status":"completed","conclusion":"failure","details_url":"https://konflux-ui.apps.example.openshiftapps.com/ns/builds/pipelinerun/conforma-dev"}]}`
			} else if !strings.HasSuffix(req.URL.Path, "/pulls/3") {
				t.Fatalf("advisory triggered unexpected identity lookup: %s", req.URL)
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
		})}})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { defer close(done); s.monitor(ctx, s.generation, fact.Source) }()
		synctest.Wait()
		time.Sleep(time.Minute)
		synctest.Wait()
		cancel()
		<-done
		x := s.Snapshot()
		if calls != 4 || len(x.Plans) != 1 || len(x.Plans[0].Advisories) != 1 || !x.Plans[0].Evidence.Fresh() {
			t.Fatalf("advisory delayed or invalidated normal polls: calls=%d plans=%+v", calls, x.Plans)
		}
		for _, entry := range x.LogEntries {
			if entry.Decision == "checks-error" || entry.Decision == "retry-wait" {
				t.Fatalf("normal advisory caused error/backoff: %+v", entry)
			}
		}
	})
}
