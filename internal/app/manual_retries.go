package app

import (
	"context"
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
	if rc := repo.Candidate; repo.Source.Name == "acs" && rcImageWaitReason(rc) == "" {
		heads[rc.Target] = rc.SHA
	}
	for _, tag := range repo.Tags {
		if tag.Name == s.choices.Tags[repo.Source.Name] && tag.Evidence.Fresh() && tag.SHA != "" && tag.SHA == s.confirmedSHAs[repo.Source.Name] {
			heads[release.Target{Repo: repo.Source.Repo, Tag: tag.Name}] = tag.SHA
		}
	}
	return heads
}

// currentManualRetries copies only reports still backed by the current selection and revision.
func (s *Session) currentManualRetries(repo release.Repository) []release.ManualRetryReport {
	var reports []release.ManualRetryReport
	for t, sha := range s.manualRetryHeads(repo) {
		r, ok := s.manualRetries[t]
		if !ok || r.SHA != sha {
			continue
		}
		r.Jobs = slices.Clone(r.Jobs)
		r.Prow = slices.Clone(r.Prow)
		r.Warnings = slices.Clone(r.Warnings)
		reports = append(reports, r)
	}
	return reports
}

// refreshManualRetries spends the remaining source-cycle read budget on display-only offers.
func (s *Session) refreshManualRetries(ctx context.Context, generation, revision uint64, source release.Source) {
	reader := s.options.ManualRetries
	if reader == nil {
		return
	}
	s.mu.Lock()
	if generation != s.generation || revision != s.workRevision {
		s.mu.Unlock()
		return
	}
	heads := s.manualRetryHeads(s.repos[source.Name])
	s.mu.Unlock()
	ctx = githubread.WithPriority(ctx, githubread.Background, source.Repo)
	reports := make(map[release.Target]release.ManualRetryReport)
	for target, sha := range heads {
		report, err := reader.ManualRetries(ctx, target)
		if err != nil {
			report.Warnings = append(report.Warnings, err.Error())
		}
		if report.Target != target || report.SHA != sha {
			// Diagnostics may describe an unavailable read; commands always require a matching revision.
			report.Jobs, report.Prow = nil, nil
			report.Target, report.SHA = target, sha
			if err == nil {
				report.Warnings = append(report.Warnings, "The target revision changed during the manual retry check.")
			}
		}
		if len(report.Jobs)+len(report.Prow)+len(report.Warnings) > 0 {
			reports[target] = report
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation != s.generation || revision != s.workRevision {
		return
	}
	for target := range s.manualRetries {
		if target.Repo == source.Repo {
			delete(s.manualRetries, target)
		}
	}
	for target, report := range reports {
		s.manualRetries[target] = report
	}
}
