package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"release-assistant/internal/githubread"
	"release-assistant/internal/release"
)

type (
	pollKind int
	pollID   struct {
		repo string
		kind pollKind
	}
	pollJob struct {
		kind     pollKind
		setting  RefreshKey
		identity string
	}
	pollRecord struct {
		finished, retryAfter time.Time
		identity             string
		failures             int
	}
)

const (
	pollTags pollKind = iota
	pollPRs
	pollChecks
	pollTagChecks
	pollBuilds
	pollImages
)

// next allows an early retry during startup. Later retries honor both the
// configured interval and the earliest time permitted by backoff or quota.
func (r pollRecord) next(interval time.Duration, startupUntil time.Time) time.Time {
	normal := r.finished.Add(interval)
	if r.retryAfter.IsZero() {
		return normal
	}
	if r.retryAfter.Before(startupUntil) || r.retryAfter.After(normal) {
		return r.retryAfter
	}
	return normal
}

// due reports whether this target has no observation or its current interval elapsed.
func (r pollRecord) due(identity string, now, next time.Time) bool {
	return r.finished.IsZero() || r.identity != identity || !now.Before(next)
}

// pollJobs selects intervals independently for each repository. A new tag or
// commit needs its own first observation, even when the old tag was just read.
func (s *Session) pollJobs(source release.Source) []pollJob {
	tag := s.choices.Tags[source.Name]
	sha := s.confirmedSHAs[source.Name]
	prs, checks, tags, tagChecks := BeforePRs, BeforePRChecks, BeforeTags, ComponentTagChecks
	acs := source.Name == "acs"
	if acs {
		prs, checks, tags, tagChecks = ACSPRs, ACSPRChecks, ACSTags, ACSTagChecks
		rc := s.repos[source.Name].Candidate
		if s.rcReady() {
			tag, sha = rc.Target.Tag, rc.SHA
		}
	} else if tag != "" {
		prs, checks, tags = AfterPRs, AfterPRChecks, AfterTags
	}
	jobs := []pollJob{{kind: pollTags, setting: tags}, {kind: pollPRs, setting: prs}}
	tracked := false
	for t, choice := range s.choices.PRs {
		if t.Repo == source.Repo && choice == release.Included {
			tracked = true
			break
		}
	}
	if tracked {
		jobs = append(jobs, pollJob{kind: pollChecks, setting: checks})
	}
	identity := tag + ":" + sha
	if _, verified := s.tagCheckTarget(s.repos[source.Name]); verified != "" {
		jobs = append(jobs, pollJob{pollTagChecks, tagChecks, identity})
	}
	if tag != "" || tracked {
		jobs = append(jobs, pollJob{pollBuilds, KonfluxBuilds, identity})
	}
	if acs {
		var images strings.Builder
		images.WriteString(identity)
		for _, component := range s.config.Components {
			fmt.Fprintf(&images, "/%s:%s", s.choices.Tags[component.Name], s.confirmedSHAs[component.Name])
		}
		jobs = append(jobs, pollJob{pollImages, ReleaseImages, images.String()})
	}
	return jobs
}

// hasOverdueObservations reports retained data that was already due at activation.
func (s *Session) hasOverdueObservations() bool {
	if s.resumedAt.IsZero() {
		return false
	}
	for _, source := range append([]release.Source{s.config.ACS()}, s.config.Components...) {
		for _, job := range s.pollJobs(source) {
			record := s.polls[pollID{source.Repo, job.kind}]
			if !record.finished.IsZero() && record.finished.Before(s.resumedAt) &&
				!record.finished.Add(s.options.RefreshIntervals[job.setting]).After(s.resumedAt) {
				return true
			}
		}
	}
	return false
}

// wakePolls asks sleeping workers to recompute their jobs. The caller holds s.mu.
func (s *Session) wakePolls() {
	close(s.pollChanged)
	s.pollChanged = make(chan struct{})
}

// monitor schedules repository reads or registry reads with independent timers.
func (s *Session) monitor(ctx context.Context, generation uint64, source release.Source, imagesOnly bool) {
	for ctx.Err() == nil {
		s.mu.Lock()
		if generation != s.generation {
			s.mu.Unlock()
			return
		}
		now := time.Now()
		if s.startupUntil.IsZero() {
			s.startupUntil = now.Add(5 * time.Minute)
		}
		var due []pollJob
		var next time.Time
		for _, job := range s.pollJobs(source) {
			if (job.kind == pollImages) != imagesOnly {
				continue
			}
			record := s.polls[pollID{source.Repo, job.kind}]
			at := record.next(s.options.RefreshIntervals[job.setting], s.startupUntil)
			if record.due(job.identity, now, at) {
				due = append(due, job)
				continue
			}
			if next.IsZero() || at.Before(next) {
				next = at
			}
		}
		if s.initiallyLoaded[source.Name] {
			priority := func(job pollJob) int {
				if (job.kind == pollChecks || job.kind == pollBuilds) &&
					s.polls[pollID{source.Repo, job.kind}].finished.IsZero() {
					return 0
				}
				return 1
			}
			slices.SortStableFunc(due, func(a, b pollJob) int { return priority(a) - priority(b) })
		}
		changed := s.pollChanged
		revision := s.workRevision
		startupTags := s.duringStartup() && source.Name != "acs" && s.choices.Tags[source.Name] == ""
		s.mu.Unlock()
		if len(due) == 0 {
			timer := time.NewTimer(time.Until(next))
			select {
			case <-ctx.Done():
			case <-changed:
			case <-timer.C:
			}
			timer.Stop()
			continue
		}
		cycle, usage := githubread.Track(ctx, 80)
		var tags sync.WaitGroup
		for _, job := range due {
			if ctx.Err() != nil {
				break
			}
			if startupTags && job.kind == pollTags {
				tags.Go(func() { s.runPoll(cycle, generation, revision, source, job, true, usage) })
			} else {
				s.runPoll(cycle, generation, revision, source, job, false, usage)
			}
		}
		tags.Wait()
	}
}

func (s *Session) fetchPoll(ctx context.Context, generation, revision uint64, source release.Source, job pollJob, startupTags bool, usage *githubread.Usage) error {
	work := sourceWork{revision: revision, scheduled: true}
	ctx = githubread.WithPriority(ctx, githubread.Builds, source.Repo)
	switch job.kind {
	case pollPRs:
		work.scope = ReadPRs | ReadTargets
		if source.Name == "acs" {
			work.scope |= ReadPins
		} else if job.setting == AfterPRs {
			work.scope |= ReadCoverage
		}
		ctx = githubread.WithPriority(ctx, githubread.PRs, source.Repo)
	case pollChecks:
		work.scope = ReadTargets | ReadChecks
		if err := s.refresh(ctx, generation, source, true, work); err != nil {
			return err
		}
		return s.refreshPRRetries(ctx, generation, revision, source)
	case pollTags:
		ctx = githubread.WithPriority(ctx, githubread.Tags, source.Repo)
		if source.Name == "acs" {
			work.scope = ReadRC | ReadPreviousPins
			break
		}
		work.scope = ReadTags
		s.mu.Lock()
		hasSavedTag := s.pendingTags[source.Name].Tag != ""
		s.mu.Unlock()
		var savedTagErr error
		if hasSavedTag {
			savedTagErr = s.refresh(ctx, generation, source, true, work)
			s.mu.Lock()
			current := generation == s.generation && revision == s.workRevision
			s.mu.Unlock()
			if !current {
				return savedTagErr
			}
		}
		if startupTags {
			return errors.Join(savedTagErr, s.readStartupTags(ctx, generation, revision, source, usage))
		}
		work.tagInventory = true
		return errors.Join(savedTagErr, s.refresh(ctx, generation, source, true, work))
	case pollTagChecks:
		return s.refreshTagChecks(ctx, generation, revision, source)
	case pollBuilds:
		work.plans = true
		return s.refresh(ctx, generation, source, false, work)
	case pollImages:
		s.mu.Lock()
		ready := s.rcReady()
		s.mu.Unlock()
		return s.refreshImagesScoped(ctx, generation, false, ready, true)
	}
	return s.refresh(ctx, generation, source, true, work)
}

func (s *Session) runPoll(ctx context.Context, generation, revision uint64, source release.Source, job pollJob, startupTags bool, usage *githubread.Usage) {
	s.mu.Lock()
	// Earlier work in the same cycle can discover a different RC or select a tag.
	id := pollID{source.Repo, job.kind}
	previous := s.polls[id]
	next := previous.next(s.options.RefreshIntervals[job.setting], s.startupUntil)
	current := generation == s.generation && revision == s.workRevision && previous.due(job.identity, time.Now(), next) && slices.Contains(s.pollJobs(source), job)
	s.mu.Unlock()
	if !current {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, s.options.Timeout)
	defer cancel()
	err := s.fetchPoll(ctx, generation, revision, source, job, startupTags, usage)
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation != s.generation || revision != s.workRevision || errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	now := time.Now()
	record := pollRecord{identity: job.identity, finished: now}
	if err != nil {
		record.failures = 1
		if previous.identity == job.identity {
			record.failures += previous.failures
		}
		delay := min(s.options.Poll, s.options.BackoffCap)
		for i := 0; i < record.failures && delay < s.options.BackoffCap; i++ {
			delay += min(delay, s.options.BackoffCap-delay)
		}
		record.retryAfter = now.Add(delay)
		if deferred, ok := errors.AsType[*githubread.DeferredError](err); ok && deferred.Until.After(record.retryAfter) {
			record.retryAfter = deferred.Until
		}
		interval := s.options.RefreshIntervals[job.setting]
		delay = record.next(interval, s.startupUntil).Sub(now)
		if record.failures == 1 || previous.next(interval, s.startupUntil).Sub(previous.finished) != delay {
			message := fmt.Sprintf("The last monitoring attempt failed while %s. The next monitoring attempt for %s is in %s.", err, consoleBranch(source), retryDelay(delay))
			s.record(release.Target{Repo: source.Repo}, "", "retry-wait", "", message, "system", message)
		}
	} else if job.kind == pollTags && s.repos[source.Name].TagsPending && now.Before(s.startupUntil) {
		// A bounded page is useful immediately, but does not complete the inventory.
		record.retryAfter = now.Add(min(time.Minute, s.options.Poll))
	}
	s.polls[id] = record
}
