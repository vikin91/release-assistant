package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

type pinningServices struct {
	*fixture
	plan     release.Plan
	planErr  error
	imageErr error
	identity dispatch.Identity
	refs     []string
	writes   int
}

func (f *pinningServices) Plan(_ context.Context, target release.Target) (release.Plan, error) {
	if target == f.plan.Target {
		return clonePlan(f.plan), f.planErr
	}
	return release.Plan{Target: target}, nil
}
func (f *pinningServices) ReadTarget(context.Context, release.Target) (dispatch.Identity, error) {
	return f.identity, nil
}
func (f *pinningServices) Write(context.Context, release.Proposal) (dispatch.Delivery, error) {
	f.writes++
	return dispatch.Delivery{Status: "sent"}, nil
}
func (f *pinningServices) InspectReference(_ context.Context, reference string) (release.Image, error) {
	f.refs = append(f.refs, reference)
	image := release.Image{Component: "acs", Family: "konflux", Name: "main", Reference: reference, Present: release.Yes, Evidence: observed()}
	if f.imageErr != nil {
		image.Present, image.Evidence.Known, image.Evidence.Error = "", false, f.imageErr.Error()
	}
	return image, f.imageErr
}
func newPinningSession(t *testing.T) (*Session, *pinningServices, release.Config) {
	t.Helper()
	s, fixture := newFixture()
	config := configuration()
	config.PinningPR = 123
	target := config.PinningTarget()
	pin := release.PR{Target: target, SHA: "pin-head", Base: config.Branch, State: "open", Evidence: observed(), ChecksEvidence: observed(), Checks: []release.Check{{Name: "build", Status: "completed", Conclusion: "failure"}}}
	acs := fixture.repos["acs"]
	acs.PinningPR, acs.PRs = &pin, []release.PR{pin}
	fixture.repos["acs"] = acs
	proposal := release.Proposal{Target: target, SHA: pin.SHA, Job: "main-on-push", RunID: "17", Payload: "/konflux-retest main-on-push"}
	f := &pinningServices{fixture: fixture, identity: dispatch.Identity{Target: target, SHA: pin.SHA, Base: pin.Base, Open: true, Evidence: observed()}, plan: release.Plan{
		Target: target, SHA: pin.SHA, Action: "Retest", Evidence: observed(), Proposals: []release.Proposal{proposal},
		Runs: []release.Run{{Job: proposal.Job, ID: proposal.RunID, Status: "completed", Outcome: "failure", Eligible: true, Image: release.Image{Reference: "quay.io/rhacs-eng/release-main@sha256:" + strings.Repeat("a", 64), Evidence: observed()}}},
	}}
	s.planner, s.images = f, f
	s.dispatcher = dispatch.New(f, f, f, dispatch.Options{EnableWrites: true})
	apply(t, s, Action{Kind: "configure", Config: config})
	return s, f, config
}

func TestConfiguredPinningPRIsObservedWithoutImplicitInclusionOrRW(t *testing.T) {
	s, f, config := newPinningSession(t)
	if err := s.Refresh(t.Context(), s.Snapshot().Generation, config.ACS(), true); err != nil {
		t.Fatal(err)
	}
	x := s.Snapshot()
	if len(x.Plans) != 1 || x.Plans[0].Target != config.PinningTarget() || x.Choices.PRs[config.PinningTarget()] != "" {
		t.Fatalf("pinning monitoring changed release inclusion: %+v", x.Choices)
	}
	if len(f.refs) != 1 || f.refs[0] != f.plan.Runs[0].Image.Reference || x.Plans[0].Runs[0].Image.Present != release.Yes {
		t.Fatal("reported pinning image was not inspected")
	}
	if len(x.Authority) != 1 || x.Authority[0].Granted || f.writes != 0 {
		t.Fatal("pinning monitoring enabled writes")
	}
	apply(t, s, Action{Kind: "grant", Target: config.PinningTarget(), SHA: f.plan.SHA, Enabled: true})
	apply(t, s, Action{Kind: "send", Target: config.PinningTarget(), ID: dispatch.ID(f.plan.Proposals[0])})
	if f.writes != 1 {
		t.Fatal("explicitly approved fake retry was not delivered")
	}
}

func TestChangingConfiguredPinningPRRevokesItsGrant(t *testing.T) {
	for name, number := range map[string]int{"replacement": 456, "removed": 0} {
		t.Run(name, func(t *testing.T) {
			s, f, config := newPinningSession(t)
			if err := s.Refresh(t.Context(), s.Snapshot().Generation, config.ACS(), true); err != nil {
				t.Fatal(err)
			}
			target := config.PinningTarget()
			apply(t, s, Action{Kind: "grant", Target: target, SHA: f.plan.SHA, Enabled: true})
			config.PinningPR = number
			apply(t, s, Action{Kind: "configure", Config: config})
			if _, err := s.dispatcher.Send(t.Context(), target, dispatch.ID(f.plan.Proposals[0]), true); err == nil || f.writes != 0 {
				t.Fatal("earlier pinning PR retained write permission")
			}
		})
	}
}

func TestPinningImageFailureDoesNotInventOrSuppressLibraryRetries(t *testing.T) {
	s, f, config := newPinningSession(t)
	f.imageErr = errors.New("registry returned HTTP 403")
	if err := s.Refresh(t.Context(), s.Snapshot().Generation, config.ACS(), true); err == nil {
		t.Fatal("image error lost")
	}
	x := s.Snapshot()
	if len(x.Plans) != 1 || len(x.Plans[0].Proposals) != 1 || dispatch.ID(x.Plans[0].Proposals[0]) != dispatch.ID(f.plan.Proposals[0]) {
		t.Fatal("registry result changed retry decisions")
	}
	if x.Plans[0].Runs[0].Image.Present != "" || x.Plans[0].Runs[0].Image.Evidence.Fresh() {
		t.Fatal("registry access failure was treated as image availability")
	}
	apply(t, s, Action{Kind: "grant", Target: config.PinningTarget(), SHA: f.plan.SHA, Enabled: true})
	apply(t, s, Action{Kind: "send", Target: config.PinningTarget(), ID: dispatch.ID(f.plan.Proposals[0])})
	if f.writes != 1 {
		t.Fatal("fresh library-approved retry was suppressed by output image inspection")
	}
}

func TestPinningOutputsRemainUnknownWhenHeadChangesOrReadsFail(t *testing.T) {
	for name, changeHead := range map[string]bool{"head changed during reads": true, "latest observation failed": false} {
		t.Run(name, func(t *testing.T) {
			s, f, config := newPinningSession(t)
			if err := s.Refresh(t.Context(), s.Snapshot().Generation, config.ACS(), true); err != nil {
				t.Fatal(err)
			}
			if changeHead {
				f.plan.SHA = "different-head"
			} else {
				f.plan = release.Plan{Target: config.PinningTarget()}
				f.planErr = errors.New("checks unavailable")
			}
			_ = s.Refresh(t.Context(), s.Snapshot().Generation, config.ACS(), true)
			x := s.Snapshot()
			if changeHead {
				for _, repo := range x.Repositories {
					if repo.Source.Name == "acs" && (repo.PinningPR.SHA != "different-head" || repo.PinningPR.Evidence.Fresh()) {
						t.Fatal("pinning metadata retained stale identity")
					}
				}
			} else if len(x.Plans) != 1 || x.Plans[0].Runs[0].Image.Present != release.Yes || x.Plans[0].Runs[0].Image.Evidence.Fresh() {
				t.Fatal("failed image observation lost prior result or kept it current")
			}
		})
	}
}

func TestPinningReadFailureKeepsConcreteCause(t *testing.T) {
	for name, keepPrevious := range map[string]bool{"first read": false, "after successful read": true} {
		t.Run(name, func(t *testing.T) {
			s, f, config := newPinningSession(t)
			if keepPrevious {
				if err := s.Refresh(t.Context(), s.Snapshot().Generation, config.ACS(), true); err != nil {
					t.Fatal(err)
				}
			}
			cause := "GitHub authentication failed (HTTP 401)"
			f.plan = release.Plan{Target: config.PinningTarget(), Evidence: release.Evidence{Error: cause}}
			f.planErr = errors.New(cause)
			if err := s.Refresh(t.Context(), s.Snapshot().Generation, config.ACS(), true); err == nil {
				t.Fatal("read failure lost")
			}
			plans := s.Snapshot().Plans
			if len(plans) != 1 || plans[0].Target != config.PinningTarget() || plans[0].Evidence.Error != cause || plans[0].Evidence.Fresh() {
				t.Fatalf("specific build failure lost: %+v", plans)
			}
			if keepPrevious && (len(plans[0].Runs) != 1 || plans[0].Runs[0].Image.Evidence.Error != cause) {
				t.Fatal("retained build image lost its failure cause")
			}
		})
	}
}
