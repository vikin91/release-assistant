package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"release-assistant/internal/apilog"
	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

// inspectPinningImages checks only outputs bound to the configured open pinning PR head.
func (s *Session) inspectPinningImages(ctx context.Context, generation uint64, plan *release.Plan) error {
	s.mu.Lock()
	selected := generation == s.generation && plan.Target == s.config.PinningTarget() && s.choices.PRs[plan.Target] != release.Ignored
	current := false
	if selected {
		for _, pr := range s.repos["acs"].PRs {
			if pr.Target == plan.Target && pr.SHA == plan.SHA && pr.State == "open" && !pr.Merged && pr.Base == s.config.Branch && pr.Evidence.Fresh() && pr.ChecksStatus() == release.Yes {
				current = true
				break
			}
		}
	}
	s.mu.Unlock()
	if !current {
		return nil
	}
	var issues []error
	for i := range plan.Runs {
		run := &plan.Runs[i]
		if run.ID == "" || run.Image.Reference == "" || !run.Image.Evidence.Fresh() {
			continue
		}
		image, err := s.images.InspectReference(ctx, run.Image.Reference)
		if image.Reference != run.Image.Reference {
			err = fmt.Errorf("registry observation did not match the reported build image")
			image = run.Image
			image.Evidence.Known = false
			image.Evidence.Error = err.Error()
		}
		run.Image = image
		if err != nil {
			issues = append(issues, err)
		}
	}
	err := errors.Join(issues...)
	if canceled(ctx) {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation == s.generation {
		message := ""
		if err != nil {
			message = err.Error()
		}
		s.recordReadResult(plan.Target, "pinning-images", message, "build images for "+consoleRevision(plan.Target, plan.SHA))
	}
	return err
}

// includePinningPR preserves the ordinary choice so clearing the pinning role restores it.
func (s *Session) includePinningPR() {
	t := s.config.PinningTarget()
	if t.PR == 0 {
		return
	}
	for _, kind := range []pollKind{pollPRs, pollChecks, pollBuilds} {
		delete(s.polls, pollID{t.Repo, kind})
	}
	s.priorPinChoice = s.choices.PRs[t]
	s.choices.PRs[t] = release.Included
	if s.priorPinChoice != release.Included {
		s.pendingPRChecks[t] = true
	}
	if s.priorPinChoice == release.Ignored || s.priorPinChoice == release.Excluded {
		s.dispatcher.Ignore(t, false)
	}
}

// clearPinningSelection drops evidence and grants associated with the outgoing selection.
// The caller holds the session lock.
func (s *Session) clearPinningSelection() {
	s.clearRCPlan()
	if s.config.PinningPR > 0 {
		t := s.config.PinningTarget()
		if s.priorPinChoice == "" {
			delete(s.choices.PRs, t)
		} else {
			s.choices.PRs[t] = s.priorPinChoice
		}
		if s.priorPinChoice != release.Included {
			delete(s.pendingPRChecks, t)
			delete(s.prRetryChecks, t)
			delete(s.plans, t)
			s.dispatcher.ObserveIdentity(dispatch.Identity{Target: t})
		} else {
			_ = s.dispatcher.Grant(t, "", false)
		}
		if s.priorPinChoice == release.Ignored || s.priorPinChoice == release.Excluded {
			s.dispatcher.Ignore(t, true)
		}
	}
	repo, exists := s.repos["acs"]
	if !exists {
		return
	}
	repo.PinningPR = nil
	repo.PinningPins = release.Pins{}
	repo.PinningEvidence = release.Evidence{}
	repo.PinDiscovery = release.PinDiscovery{}
	s.repos["acs"] = repo
}

// confirmPinningPR binds the selection to the displayed configuration and head across the live identity read.
func (s *Session) confirmPinningPR(ctx context.Context, a Action) error {
	s.mu.Lock()
	source := s.config.ACS()
	reviewed := func() bool {
		return slices.ContainsFunc(s.repos["acs"].PRs, func(pr release.PR) bool {
			return pr.Target == a.Target && pr.Base == source.Branch && pr.SHA == a.SHA
		})
	}
	if a.Generation != s.generation || a.Target.Repo != source.Repo || !a.Target.Valid() || a.Target.PR <= 0 || a.SHA == "" || !reviewed() {
		s.mu.Unlock()
		return fmt.Errorf("PR evidence changed; review the current ACS release PR list")
	}
	ctx = apilog.WithRelease(ctx, s.config.Version)
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, s.options.Timeout)
	defer cancel()
	repo, err := s.discovery.Discover(ctx, DiscoveryRequest{Source: source, Tracked: []release.Target{a.Target}, Scope: ReadTargets})
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.Generation != s.generation {
		return fmt.Errorf("configuration changed; review the ACS release PR list")
	}
	valid := slices.ContainsFunc(repo.PRs, func(pr release.PR) bool {
		return pr.Target == a.Target && pr.Base == source.Branch && pr.SHA == a.SHA && pr.Evidence.Fresh() && (pr.State == "open" || pr.State == "closed" || pr.State == "merged")
	})
	s.workRevision++
	select {
	case s.changed <- struct{}{}:
	default:
	}
	if err != nil {
		return fmt.Errorf("could not verify pinning PR: %w", err)
	}
	if !valid || !reviewed() {
		return fmt.Errorf("PR changed; refreshing discovery for another review")
	}
	if err := s.options.ReleaseState.savePinningChoice(s.config.Version, savedPinningChoice{Branch: source.Branch, Number: a.Target.PR}); err != nil {
		return err
	}
	s.clearPinningSelection()
	s.pendingPinningPR = 0
	s.config.PinningPR = a.Target.PR
	s.includePinningPR()
	s.config.DetectPinningPR = false
	s.config.ManualPinningPR = 0
	s.generation++
	s.record(a.Target, a.SHA, "pinning-pr-selected", "", "Selected as pinning PR.", "user")
	return nil
}

func (s *Session) logPinDiscovery(old, current release.PinDiscovery) {
	if old.Evidence.Fresh() == current.Evidence.Fresh() && old.Evidence.Error == current.Evidence.Error && maps.Equal(old.Files, current.Files) {
		return
	}
	s.recordReadResult(release.Target{Repo: s.config.ACS().Repo}, "pin-file-discovery", current.Evidence.Error, "pinning PR candidates")
	role := "Pinning PR candidate"
	if current.Evidence.Fresh() && current.Candidates() == 1 {
		role = "Suggested pinning PR"
	}
	for _, n := range slices.Sorted(maps.Keys(current.Files)) {
		m := current.Files[n]
		if m.Reason != "" {
			message := fmt.Sprintf("%s #%d. %s", role, n, m.Reason)
			s.record(release.Target{Repo: s.config.ACS().Repo, PR: n}, m.Head, "pinning-pr-candidate", "", message, "system", message)
		}
	}
}
