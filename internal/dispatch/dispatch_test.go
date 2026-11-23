package dispatch

import (
	"context"
	"errors"
	"release-assistant/internal/release"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fake struct {
	identity    Identity
	plan        release.Plan
	readHook    func()
	planErr     error
	readErr     error
	writes      atomic.Int32
	writeStatus string
	writeErr    error
}

func (f *fake) ReadTarget(context.Context, release.Target) (Identity, error) {
	if f.readHook != nil {
		f.readHook()
	}
	return f.identity, f.readErr
}
func (f *fake) Plan(context.Context, release.Target) (release.Plan, error) { return f.plan, f.planErr }
func (f *fake) Write(context.Context, release.Proposal) (Delivery, error) {
	f.writes.Add(1)
	status := f.writeStatus
	if status == "" {
		status = "sent"
	}
	return Delivery{Status: status}, f.writeErr
}
func setup(t *testing.T) (*Dispatcher, *fake, *time.Time) {
	t.Helper()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	target := release.Target{Repo: "stackrox/fact", PR: 1}
	proposal := release.Proposal{Target: target, SHA: "abc", Job: "fact-on-push", RunID: "1", Payload: "/konflux-retest fact-on-push", At: now}
	f := &fake{identity: Identity{Target: target, SHA: "abc", Open: true, Evidence: release.Evidence{At: now, Known: true}}, plan: release.Plan{Target: target, SHA: "abc", Proposals: []release.Proposal{proposal}, Runs: []release.Run{{Job: proposal.Job, ID: "1", Eligible: true, Status: "completed", Outcome: "failure"}}}}
	d := New(f, f, f, Options{EnableWrites: true, Threshold: 2, Now: func() time.Time { return now }})
	d.ObserveIdentity(f.identity)
	d.ObservePlan(f.plan)
	if err := d.Grant(target, "abc", true); err != nil {
		t.Fatal(err)
	}
	return d, f, &now
}
func TestDispatchWithholdsUnsafeWrites(t *testing.T) {
	for name, mutate := range map[string]func(*Dispatcher, *fake){
		"revocation":            func(d *Dispatcher, f *fake) { f.readHook = func() { _ = d.Grant(f.identity.Target, "abc", false) } },
		"ignore":                func(d *Dispatcher, f *fake) { f.readHook = func() { d.Ignore(f.identity.Target, true) } },
		"base change":           func(_ *Dispatcher, f *fake) { f.identity.Base = "another-branch" },
		"head change":           func(_ *Dispatcher, f *fake) { f.identity.SHA = "new" },
		"opt out":               func(_ *Dispatcher, f *fake) { f.identity.OptOut = true },
		"unknown authority":     func(_ *Dispatcher, f *fake) { f.identity.Evidence.Known = false },
		"closed":                func(_ *Dispatcher, f *fake) { f.identity.Open = false },
		"withdrawn":             func(_ *Dispatcher, f *fake) { f.plan.Proposals = nil },
		"new run":               func(_ *Dispatcher, f *fake) { f.plan.Proposals[0].RunID = "2" },
		"payload changed":       func(_ *Dispatcher, f *fake) { f.plan.Proposals[0].Payload = "different" },
		"configuration changed": func(d *Dispatcher, f *fake) { f.readHook = d.Reset },
		"write mode changed":    func(d *Dispatcher, f *fake) { f.readHook = func() { d.Interactive(false) } },
		"server stop":           func(d *Dispatcher, f *fake) { f.readHook = d.Stop },
	} {
		t.Run(name, func(t *testing.T) {
			d, f, _ := setup(t)
			p := f.plan.Proposals[0]
			mutate(d, f)
			if _, err := d.Send(t.Context(), p.Target, ID(p), true); err == nil {
				t.Fatal("unsafe write accepted")
			}
			if f.writes.Load() != 0 {
				t.Fatal("writer called")
			}
		})
	}
}
func TestPRCommentLabelGate(t *testing.T) {
	for name, tc := range map[string]struct {
		interactive, required, tag, observed, current, readError, optOut, wantWrite bool
	}{
		"label requirement off":       {wantWrite: true},
		"automatic eligible PR":       {required: true, observed: true, current: true, wantWrite: true},
		"interactive eligible PR":     {interactive: true, required: true, observed: true, current: true, wantWrite: true},
		"unlabeled PR":                {required: true},
		"label removed before write":  {required: true, observed: true},
		"confirmed but label removed": {interactive: true, required: true, observed: true},
		"label read failed":           {required: true, observed: true, current: true, readError: true},
		"opt-out takes precedence":    {required: true, observed: true, current: true, optOut: true},
		"tag needs no label":          {required: true, tag: true, wantWrite: true},
	} {
		t.Run(name, func(t *testing.T) {
			d, f, _ := setup(t)
			if tc.tag {
				target := release.Target{Repo: f.identity.Target.Repo, Tag: "3.0.0"}
				f.identity.Target, f.plan.Target, f.plan.Proposals[0].Target = target, target, target
			}
			d.SetWriteMode(&tc.interactive, &tc.required)
			f.identity.CommentLabel = tc.observed
			d.ObserveIdentity(f.identity)
			d.ObservePlan(f.plan)
			if err := d.Grant(f.identity.Target, f.identity.SHA, !tc.interactive); err != nil {
				t.Fatal(err)
			}
			f.identity.CommentLabel = tc.current
			f.identity.OptOut = tc.optOut
			if tc.readError {
				f.readErr = errors.New("GitHub unavailable")
			}
			p := f.plan.Proposals[0]
			_, err := d.Send(t.Context(), p.Target, ID(p), tc.interactive)
			if tc.wantWrite {
				if err != nil || f.writes.Load() != 1 {
					t.Fatalf("writes = %d, error = %v", f.writes.Load(), err)
				}
			} else if err == nil || f.writes.Load() != 0 {
				t.Fatalf("expected denial, got writes = %d, error = %v", f.writes.Load(), err)
			}
		})
	}
}

func TestSingleUseCooldownAndDuplicateSubmission(t *testing.T) {
	d, f, now := setup(t)
	p := f.plan.Proposals[0]
	if _, err := d.Send(t.Context(), p.Target, ID(p), false); err == nil {
		t.Fatal("confirmation not required")
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() { _, _ = d.Send(t.Context(), p.Target, ID(p), true) })
	}
	wg.Wait()
	if f.writes.Load() != 1 {
		t.Fatal(f.writes.Load())
	}
	if !d.Views()[0].Offers[0].CommentSent {
		t.Fatal("successful delivery missing from the offer")
	}
	if d.Views()[0].Offers[0].WaitingUntil.IsZero() {
		t.Fatal("cooldown not visible")
	}
	*now = now.Add(time.Minute)
	if _, err := d.Send(t.Context(), p.Target, ID(p), false); err == nil {
		t.Fatal("approval replayed")
	}
	if _, err := d.Send(t.Context(), p.Target, ID(p), true); err == nil || f.writes.Load() != 1 {
		t.Fatal("a completed submission was posted again")
	}
}
func TestFailureCountsUseRunsAndReset(t *testing.T) {
	d, f, now := setup(t)
	p := f.plan.Proposals[0]
	for range 20 {
		d.ObservePlan(f.plan)
	}
	if d.Views()[0].Offers[0].Failures != 1 {
		t.Fatal("polls counted")
	}
	_, _ = d.Send(t.Context(), p.Target, ID(p), true)
	*now = now.Add(time.Minute)
	f.plan.Runs[0].ID = "2"
	f.plan.Proposals[0].RunID = "2"
	d.ObservePlan(f.plan)
	o := d.Views()[0].Offers[0]
	if o.Failures != 2 || o.CanApprove || o.WaitingUntil.Sub(*now) != 59*time.Minute {
		t.Fatal(o)
	}
	f.plan.Runs[0].ID = "3"
	f.plan.Runs[0].Outcome = "success"
	f.plan.Runs[0].Eligible = false
	d.ObservePlan(f.plan)
	if d.Views()[0].Offers[0].Failures != 0 {
		t.Fatal("success did not reset")
	}
	f.identity.SHA = "new"
	d.ObserveIdentity(f.identity)
	if d.Views()[0].Granted || len(d.Views()[0].Offers) != 0 {
		t.Fatal("head did not reset")
	}
}
func TestVerifiedProposalSurvivesPartialResults(t *testing.T) {
	d, f, _ := setup(t)
	p := f.plan.Proposals[0]
	f.planErr = errors.New("unrelated job unavailable")
	if _, err := d.Send(t.Context(), p.Target, ID(p), true); err != nil {
		t.Fatal(err)
	}
}
func TestOptOutRemovalNeverRestoresGrant(t *testing.T) {
	d, f, _ := setup(t)
	f.identity.OptOut = true
	d.ObserveIdentity(f.identity)
	f.identity.OptOut = false
	d.ObserveIdentity(f.identity)
	if d.Views()[0].Granted {
		t.Fatal("grant restored")
	}
}

func TestSuccessOnPreviouslyFailedCheckResetsCounter(t *testing.T) {
	d, f, _ := setup(t)
	f.plan.Runs[0].Outcome = "success"
	f.plan.Runs[0].Eligible = false
	d.ObservePlan(f.plan)
	if d.Views()[0].Offers[0].Failures != 0 {
		t.Fatal("success on same check ID did not reset failures")
	}
}
func TestWarningsSurviveWithdrawalOfProposal(t *testing.T) {
	d, f, _ := setup(t)
	f.plan.Runs[0].ID = "2"
	d.ObservePlan(f.plan)
	f.plan.Proposals = nil
	f.plan.Runs[0].Eligible = false
	d.ObservePlan(f.plan)
	if len(d.Views()[0].Warnings) != 1 {
		t.Fatal("threshold warning disappeared")
	}
}

func TestUncertainDeliveryStillReservesGap(t *testing.T) {
	d, f, _ := setup(t)
	f.writeStatus = "uncertain"
	f.writeErr = errors.New("transport timeout")
	p := f.plan.Proposals[0]
	result, err := d.Send(t.Context(), p.Target, ID(p), true)
	if err == nil || result.Status != "uncertain" {
		t.Fatal(result, err)
	}
	if _, err := d.Send(t.Context(), p.Target, ID(p), true); err == nil || f.writes.Load() != 1 {
		t.Fatal("uncertain attempt immediately replayed")
	}
}
func TestTargetGapAppliesAcrossJobs(t *testing.T) {
	d, f, _ := setup(t)
	p := f.plan.Proposals[0]
	second := p
	second.Job = "another-library-job"
	second.RunID = "second"
	second.Payload = "/konflux-retest another-library-job"
	f.plan.Proposals = append(f.plan.Proposals, second)
	d.ObservePlan(f.plan)
	if _, err := d.Send(t.Context(), p.Target, ID(p), true); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Send(t.Context(), second.Target, ID(second), true); err == nil {
		t.Fatal("target cooldown bypassed through another job")
	}
}

func TestStartupWritePermissionCannotBeBypassed(t *testing.T) {
	for name, target := range map[string]release.Target{
		"PR":  {Repo: "stackrox/fact", PR: 1},
		"tag": {Repo: "stackrox/fact", Tag: "0.4.7"},
	} {
		t.Run(name, func(t *testing.T) {
			_, f, _ := setup(t)
			f.identity.Target = target
			f.plan.Target = target
			f.plan.Proposals[0].Target = target
			p := f.plan.Proposals[0]
			d := New(f, f, f, Options{})
			if d.WritesEnabled() {
				t.Fatal("writes enabled by default")
			}
			for _, interactive := range []bool{true, false} {
				d.Reset()
				d.Interactive(interactive)
				d.ObserveIdentity(f.identity)
				d.ObservePlan(f.plan)
				if err := d.Grant(target, p.SHA, true); err == nil || !strings.Contains(err.Error(), "-enable-writes") {
					t.Fatalf("read-only grant: %v", err)
				}
				if err := d.Grant(target, p.SHA, false); err != nil {
					t.Fatalf("revocation denied: %v", err)
				}
				view := d.Views()[0]
				if view.Granted || len(view.Offers) != 1 || view.Offers[0].Allowed || view.Offers[0].CanApprove || !strings.Contains(view.Offers[0].Reason, "-enable-writes") {
					t.Fatalf("read-only offers: %+v", view)
				}
				for _, approve := range []bool{false, true} {
					if _, err := d.Send(t.Context(), target, ID(p), approve); err == nil || !strings.Contains(err.Error(), "-enable-writes") {
						t.Fatalf("read-only send (interactive %t, approve %t): %v", interactive, approve, err)
					}
				}
			}
			if f.writes.Load() != 0 {
				t.Fatal("read-only process called writer")
			}
		})
	}
}

func TestStartupWriteOptInDefaultsToSingleUseConfirmation(t *testing.T) {
	_, f, _ := setup(t)
	d := New(f, f, f, Options{EnableWrites: true})
	d.ObserveIdentity(f.identity)
	d.ObservePlan(f.plan)
	p := f.plan.Proposals[0]
	if !d.WritesEnabled() || !d.IsInteractive() {
		t.Fatal("expected Interactive mode with explicit startup permission")
	}
	if _, err := d.Send(t.Context(), p.Target, ID(p), false); err == nil || f.writes.Load() != 0 {
		t.Fatal("startup permission bypassed Interactive confirmation")
	}
	if _, err := d.Send(t.Context(), p.Target, ID(p), true); err != nil || f.writes.Load() != 1 {
		t.Fatalf("Interactive confirmation should work without an automatic posting grant: %v", err)
	}
	d.Reset()
	if !d.WritesEnabled() {
		t.Fatal("reset lost startup permission")
	}
	if len(d.Views()) != 0 {
		t.Fatal("reset retained target grants")
	}
}
