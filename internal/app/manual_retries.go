package app

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"release-assistant/internal/githubread"
	"release-assistant/internal/release"
)

// manualRetryHeads selects current target identities independently of Konflux or write permission.
// The caller holds the session lock.
func (s *Session) manualRetryHeads(repo release.Repository) map[release.Target]string {
	heads := make(map[release.Target]string)
	includePR := func(pr release.PR) {
		if s.choices.PRs[pr.Target] != release.Included || pr.State != "open" || pr.Merged ||
			pr.Base != repo.Source.Branch || !pr.Evidence.Fresh() || pr.SHA == "" {
			return
		}
		// A complete set of passing or pending results needs no further GitHub reads.
		if pr.CheckResultsEvidence.Fresh() && !slices.ContainsFunc(pr.Checks, func(c release.Check) bool { return c.Result() == release.No }) {
			return
		}
		heads[pr.Target] = pr.SHA
	}
	for _, pr := range repo.PRs {
		includePR(pr)
	}
	if pr := repo.PinningPR; pr != nil && pr.Target == s.config.PinningTarget() {
		delete(heads, pr.Target)
		includePR(*pr)
	}
	if target, sha := s.tagCheckTarget(repo); sha != "" {
		heads[target] = sha
	}
	return heads
}

// currentManualRetries copies only reports still backed by the current selection and revision.
func (s *Session) currentManualRetries(repo release.Repository) []release.ManualRetryReport {
	var reports []release.ManualRetryReport
	for t, sha := range s.manualRetryHeads(repo) {
		r, ok := s.manualRetries[t]
		if !ok || r.SHA != sha {
			if repo.Source.Name == "acs" && t.PR > 0 && s.options.ManualRetries != nil {
				reports = append(reports, release.ManualRetryReport{Target: t, SHA: sha, Pending: true})
			}
			continue
		}
		r.Jobs = slices.Clone(r.Jobs)
		r.Prow = slices.Clone(r.Prow)
		r.Warnings = slices.Clone(r.Warnings)
		r.OtherChecks = slices.Clone(r.OtherChecks)
		reports = append(reports, r)
	}
	return reports
}

// refreshManualRetries spends the remaining source-cycle read budget on display-only offers.
func (s *Session) refreshManualRetries(ctx context.Context, generation, revision uint64, source release.Source) {
	_ = s.readManualRetries(ctx, generation, revision, source, ReadAll)
}

func (s *Session) refreshPRRetries(ctx context.Context, generation, revision uint64, source release.Source) error {
	return s.readManualRetries(ctx, generation, revision, source, ReadPRs)
}

// readManualRetries replaces only the reports covered by this type of refresh.
func (s *Session) readManualRetries(ctx context.Context, generation, revision uint64, source release.Source, scope DiscoveryScope) error {
	inScope := func(target release.Target) bool {
		return scope == ReadAll || (scope == ReadPRs && target.PR > 0) || (scope == ReadTags && target.Tag != "")
	}
	reader := s.options.ManualRetries
	if reader == nil {
		return nil
	}
	s.mu.Lock()
	if generation != s.generation || revision != s.workRevision {
		s.mu.Unlock()
		return nil
	}
	repo := s.repos[source.Name]
	heads := s.manualRetryHeads(repo)
	s.mu.Unlock()
	ctx = githubread.WithPriority(ctx, githubread.Background, source.Repo)
	reports := make(map[release.Target]release.ManualRetryReport)
	var errs []error
	for target, sha := range heads {
		if !inScope(target) {
			continue
		}
		report, err := reader.ManualRetries(ctx, target)
		if err != nil {
			errs = append(errs, err)
			report.Incomplete = true
			report.Warnings = append(report.Warnings, err.Error())
		}
		if report.Target != target || report.SHA != sha {
			// Diagnostics may describe an unavailable read; commands always require a matching revision.
			report.Jobs, report.Prow, report.OtherChecks = nil, nil, nil
			report.Target, report.SHA = target, sha
			report.Incomplete = true
			if err == nil {
				report.Warnings = append(report.Warnings, "The target revision changed during the manual retry check.")
				errs = append(errs, fmt.Errorf("manual retry check for %s observed a different revision", target))
			}
		}
		if target == repo.Candidate.Target || (source.Name == "acs" && target.PR > 0) || len(report.Jobs)+len(report.Prow)+len(report.Warnings)+len(report.OtherChecks) > 0 {
			reports[target] = report
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation != s.generation || revision != s.workRevision {
		return nil
	}
	for target := range s.manualRetries {
		if target.Repo == source.Repo && inScope(target) {
			delete(s.manualRetries, target)
		}
	}
	for target, report := range reports {
		s.manualRetries[target] = report
	}
	return errors.Join(errs...)
}
