package app

import (
	"context"
	"errors"
	"fmt"

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
