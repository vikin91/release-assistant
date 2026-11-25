package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"release-assistant/internal/githubread"
	"release-assistant/internal/release"
)

type scheduledPRRetry struct {
	target           release.Target
	source, sha      string
	generation       uint64
	queuedAt, nextAt time.Time
	urgent           bool
}

func (s *Session) enqueuePRRetries(generation, revision uint64, source release.Source) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.options.CIPlanner == nil || generation != s.generation || revision != s.workRevision {
		return
	}
	if s.ciRetryJobs == nil {
		s.ciRetryJobs = make(map[release.Target]scheduledPRRetry)
	}
	now := time.Now()
	added := false
	// CI Retest resolves the head itself, so an incomplete status poll can
	// still queue a check for the last observed head.
	for target, sha := range s.manualRetryHeads(s.repos[source.Name], false) {
		if target.PR == 0 {
			continue
		}
		if job, ok := s.ciRetryJobs[target]; ok && job.generation == generation && job.sha == sha {
			continue
		}
		report := s.manualRetries[target]
		check := s.prRetryChecks[target].CI
		first := report.SHA != sha && check.SHA != sha
		s.ciRetryJobs[target] = scheduledPRRetry{
			target: target, source: source.Name, sha: sha, generation: generation,
			queuedAt: now, urgent: first,
		}
		added = true
	}
	if added {
		s.wakePolls()
	}
}

// monitorPRRetries gives repositories turns between planner calls independently of their poll intervals.
func (s *Session) monitorPRRetries(ctx context.Context, generation uint64) {
	var lastRepo string
	for ctx.Err() == nil {
		job, next, changed := s.nextPRRetry(generation, lastRepo)
		if job.target.PR > 0 {
			lastRepo = job.target.Repo
			s.runPRRetry(ctx, job)
			continue
		}
		if next.IsZero() {
			select {
			case <-ctx.Done():
			case <-changed:
			}
			continue
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
		case <-changed:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// nextPRRetry keeps the selected job queued during its read so pollers can coalesce duplicates.
func (s *Session) nextPRRetry(generation uint64, lastRepo string) (scheduledPRRetry, time.Time, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var selected scheduledPRRetry
	var next time.Time
	if generation != s.generation {
		return selected, next, s.pollChanged
	}
	now := time.Now()
	heads := make(map[string]map[release.Target]string)
	for target, job := range s.ciRetryJobs {
		if _, ok := heads[job.source]; !ok {
			heads[job.source] = s.manualRetryHeads(s.repos[job.source], false)
		}
		check := s.prRetryChecks[target].CI
		if job.generation != generation || heads[job.source][target] != job.sha ||
			(check.SHA == job.sha && !check.Incomplete && !check.At.Before(job.queuedAt)) {
			delete(s.ciRetryJobs, target)
			continue
		}
		if job.nextAt.After(now) {
			if next.IsZero() || job.nextAt.Before(next) {
				next = job.nextAt
			}
			continue
		}
		if selected.target.PR == 0 || job.precedes(selected, lastRepo) {
			selected = job
		}
	}
	if selected.target.PR > 0 {
		if resume := s.githubReadResumeAt(); resume.After(now) {
			return scheduledPRRetry{}, resume, s.pollChanged
		}
	}
	return selected, next, s.pollChanged
}

// currentPRRetry checks the queued revision against current release evidence.
// The caller holds the session lock.
func (s *Session) currentPRRetry(job scheduledPRRetry) bool {
	return job.generation == s.generation && s.manualRetryHeads(s.repos[job.source], false)[job.target] == job.sha
}

func (job scheduledPRRetry) precedes(other scheduledPRRetry, lastRepo string) bool {
	if job.target.Repo != other.target.Repo {
		after, otherAfter := job.target.Repo > lastRepo, other.target.Repo > lastRepo
		if after != otherAfter {
			return after
		}
		return job.target.Repo < other.target.Repo
	}
	if job.urgent != other.urgent {
		return job.urgent
	}
	if !job.queuedAt.Equal(other.queuedAt) {
		return job.queuedAt.Before(other.queuedAt)
	}
	return job.target.PR < other.target.PR
}

func (s *Session) runPRRetry(ctx context.Context, job scheduledPRRetry) {
	if ctx.Err() != nil {
		return
	}
	readCtx, cancel := context.WithTimeout(ctx, s.options.Timeout)
	defer cancel()
	readCtx = githubread.WithPriority(readCtx, githubread.Background, job.target.Repo)
	report, changedHead, err := readManualRetry(readCtx, s.options.CIPlanner, job.target, job.sha)

	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil || s.ciRetryJobs[job.target] != job {
		return
	}
	if !s.currentPRRetry(job) {
		delete(s.ciRetryJobs, job.target)
		return
	}
	s.storeManualRetry(job.target, report)
	if err != nil && !changedHead {
		var retryAt time.Time
		if deferred, ok := errors.AsType[*githubread.DeferredError](err); ok {
			retryAt = deferred.Until
		} else if strings.Contains(err.Error(), "GitHub rate limit reached") {
			retryAt = s.githubReadResumeAt()
		}
		if retryAt.After(time.Now()) {
			job.nextAt, job.urgent = retryAt, true
			s.ciRetryJobs[job.target] = job
			return
		}
	}
	delete(s.ciRetryJobs, job.target)
}

func (s *Session) githubReadResumeAt() time.Time {
	if s.options.GitHubReads == nil {
		return time.Time{}
	}
	budget := s.options.GitHubReads.Budget()
	if budget.PausedUntil.After(budget.NextSlot) {
		return budget.PausedUntil
	}
	return budget.NextSlot
}
