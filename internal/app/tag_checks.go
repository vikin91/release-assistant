package app

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"release-assistant/internal/release"
)

// tagCheckObservation binds a read, including a failed one, to the selection that requested it.
type tagCheckObservation struct {
	target     release.Target
	sha        string
	validation release.Validation
}

// tagCheckTarget returns the selected tag only while discovery verifies its commit.
// The caller holds the session lock.
func (s *releaseWork) tagCheckTarget(repo release.Repository) (release.Target, string) {
	if repo.Source.Name == "acs" {
		if rcImageWaitReason(repo.Candidate) == "" {
			return repo.Candidate.Target, repo.Candidate.SHA
		}
	} else {
		selected := s.choices.Tags[repo.Source.Name]
		for _, tag := range repo.Tags {
			if selected != "" && tag.Name == selected && tag.Evidence.Fresh() &&
				tag.SHA != "" && tag.SHA == s.confirmedSHAs[repo.Source.Name] {
				return release.Target{Repo: repo.Source.Repo, Tag: tag.Name}, tag.SHA
			}
		}
	}
	return release.Target{}, ""
}

func (s *Session) refreshTagChecks(ctx context.Context, generation, revision uint64, source release.Source) error {
	err := s.readSelectedTagChecks(ctx, generation, revision, source)
	return errors.Join(err, s.readManualRetries(ctx, generation, revision, source, ReadTags))
}

func (s *Session) readSelectedTagChecks(ctx context.Context, generation, revision uint64, source release.Source) error {
	reader, ok := s.discovery.(TagCheckReader)
	if !ok {
		return nil
	}
	s.mu.Lock()
	target, sha := s.tagCheckTarget(s.repos[source.Name])
	current := generation == s.generation && revision == s.workRevision
	s.mu.Unlock()
	if !current || sha == "" {
		return nil
	}
	validation, err := reader.ReadTagChecks(ctx, target)
	s.mu.Lock()
	defer s.mu.Unlock()
	currentTarget, currentSHA := s.tagCheckTarget(s.repos[source.Name])
	if generation != s.generation || revision != s.workRevision || canceled(ctx) || target != currentTarget || sha != currentSHA {
		return nil
	}
	if validation.SHA != sha {
		if err == nil {
			err = fmt.Errorf("the tag %s no longer resolves to the selected commit %s", target, sha)
		}
		validation.SHA = ""
		validation.Checks = nil
	}
	if err != nil {
		validation.Evidence.Known = false
		if validation.Evidence.Error == "" {
			validation.Evidence.Error = err.Error()
		}
	}
	previous := s.tagChecks[source.Name]
	var before checkActivity
	if previous.target == target && previous.sha == sha {
		before = rcCheckActivity(previous.validation)
	}
	s.recordCheckChange(target, sha, before, rcCheckActivity(validation))
	s.tagChecks[source.Name] = tagCheckObservation{target: target, sha: sha, validation: validation}
	return err
}

// plansForDisplay joins independent GitHub and Konflux observations only for the current tag revision.
// The joined plans are display data. The dispatcher receives only the planner's original evidence.
func (s *releaseWork) plansForDisplay() []release.Plan {
	plans := maps.Clone(s.plans)
	for source, observed := range s.tagChecks {
		repo := s.repos[source]
		target, sha := s.tagCheckTarget(repo)
		if source == "acs" {
			target, sha = repo.Candidate.Target, repo.Candidate.SHA
		}
		if observed.target != target || observed.sha != sha {
			continue
		}
		plan := plans[target]
		if plan.SHA != "" && plan.SHA != sha {
			plan = release.Plan{}
		}
		plan.Target, plan.SHA, plan.Validation = target, sha, observed.validation
		plans[target] = plan
	}
	copies := make([]release.Plan, 0, len(plans))
	for _, plan := range plans {
		copies = append(copies, clonePlan(plan))
	}
	return copies
}
