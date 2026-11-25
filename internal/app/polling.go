package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
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
		kind         pollKind
		setting      RefreshKey
		identity     string
		imageRefresh uint64
	}
	pollRecord struct {
		finished, retryAfter time.Time
		identity             string
		tagRefresh           string
		failures             int
		continuing           bool
		imageRefresh         uint64
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

func (job pollJob) id(source release.Source) pollID {
	if job.kind == pollTags && source.Name != "acs" {
		return pollID{source.Name, pollTags}
	}
	return pollID{source.Repo, job.kind}
}

// next resumes incomplete tag batches and applies the regular interval to completed polls.
func (r pollRecord) next(interval time.Duration, startupUntil time.Time) time.Time {
	if r.continuing {
		if r.retryAfter.IsZero() {
			return r.finished
		}
		return r.retryAfter
	}
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

func (s *Session) tagPollJob(source release.Source) pollJob {
	job := pollJob{kind: pollTags, setting: BeforeTags}
	if source.Name == "acs" {
		job.setting = ACSTags
		return job
	}
	if s.choices.Tags[source.Name] != "" {
		job.setting = AfterTags
	}
	record := s.polls[job.id(source)]
	job.identity = record.tagRefresh
	if job.identity == "" {
		job.identity = record.identity
	}
	return job
}

// pollJobs selects intervals independently for each repository. A new tag or
// commit needs its own first observation, even when the old tag was just read.
func (s *Session) pollJobs(source release.Source) []pollJob {
	tag := s.choices.Tags[source.Name]
	sha := s.confirmedSHAs[source.Name]
	prs, checks, tagChecks := BeforePRs, BeforePRChecks, ComponentTagChecks
	acs := source.Name == "acs"
	if acs {
		prs, checks, tagChecks = ACSPRs, ACSPRChecks, ACSTagChecks
		rc := s.repos[source.Name].Candidate
		if s.rcReady() {
			tag, sha = rc.Target.Tag, rc.SHA
		}
	} else if tag != "" {
		prs, checks = AfterPRs, AfterPRChecks
	}
	jobs := []pollJob{s.tagPollJob(source), {kind: pollPRs, setting: prs}}
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
		jobs = append(jobs, pollJob{kind: pollTagChecks, setting: tagChecks, identity: identity})
	}
	if tag != "" || tracked {
		jobs = append(jobs, pollJob{kind: pollBuilds, setting: KonfluxBuilds, identity: identity})
	}
	if acs {
		var images strings.Builder
		images.WriteString(identity)
		for _, component := range s.config.Components {
			fmt.Fprintf(&images, "/%s:%s", s.choices.Tags[component.Name], s.confirmedSHAs[component.Name])
		}
		fmt.Fprintf(&images, "/%d", s.imageRefresh)
		jobs = append(jobs, pollJob{kind: pollImages, setting: ReleaseImages, identity: images.String(), imageRefresh: s.imageRefresh})
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
			record := s.polls[job.id(source)]
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
			if (job.kind == pollImages) != imagesOnly || (job.kind == pollTags && source.Name != "acs") {
				continue
			}
			record := s.polls[job.id(source)]
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
					s.polls[job.id(source)].finished.IsZero() {
					return 0
				}
				return 1
			}
			slices.SortStableFunc(due, func(a, b pollJob) int { return priority(a) - priority(b) })
		}
		changed := s.pollChanged
		revision := s.workRevision
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
		cycle, _ := githubread.Track(ctx, 80)
		for _, job := range due {
			if ctx.Err() != nil {
				break
			}
			s.runPoll(cycle, generation, revision, source, job)
		}
	}
}

var pollReadNames = [...]string{
	pollTags: "Tag and RC discovery", pollPRs: "PR discovery",
	pollChecks: "PR status", pollTagChecks: "Tag checks",
	pollBuilds: "Konflux and build checks", pollImages: "Release images",
}

func (s *Session) fetchPoll(ctx context.Context, generation, revision uint64, source release.Source, job pollJob) error {
	work := sourceWork{revision: revision, scheduled: true}
	ctx = githubread.WithPriority(ctx, githubread.Builds, source.Repo)
	ctx = githubread.WithWork(ctx, source.Repo, pollReadNames[job.kind])
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
		err := s.refresh(ctx, generation, source, true, work)
		if !canceled(ctx) {
			s.enqueuePRRetries(generation, revision, source)
		}
		return err
	case pollTags:
		ctx = githubread.WithPriority(ctx, githubread.Tags, source.Repo)
		if source.Name == "acs" {
			work.scope = ReadRC | ReadPreviousPins
			break
		}
		work.scope = ReadTags
		s.mu.Lock()
		record := s.polls[job.id(source)]
		hasSavedTag := s.pendingTags[source.Name].Tag != "" && record.tagRefresh == ""
		continuing := record.continuing && record.identity == job.identity
		s.mu.Unlock()
		if continuing {
			ctx = githubread.WithPriority(ctx, githubread.Background, source.Repo)
		}
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
		work.tagInventory = true
		if record.tagRefresh == job.identity {
			work.tagRefresh = record.tagRefresh
		}
		return errors.Join(savedTagErr, s.refresh(ctx, generation, source, true, work))
	case pollTagChecks:
		return s.refreshTagChecks(ctx, generation, revision, source)
	case pollBuilds:
		work.plans = true
		return s.refresh(ctx, generation, source, false, work)
	case pollImages:
		s.mu.Lock()
		ready := s.rcReady()
		force := job.imageRefresh > s.polls[pollID{source.Repo, pollImages}].imageRefresh
		s.mu.Unlock()
		return s.refreshImagesScoped(ctx, generation, false, ready, true, force)
	}
	return s.refresh(ctx, generation, source, true, work)
}

func (s *Session) runPoll(ctx context.Context, generation, revision uint64, source release.Source, job pollJob) {
	s.mu.Lock()
	// Earlier work in the same cycle can discover a different RC or select a tag.
	id := job.id(source)
	previous := s.polls[id]
	next := previous.next(s.options.RefreshIntervals[job.setting], s.startupUntil)
	current := generation == s.generation && revision == s.workRevision && previous.due(job.identity, time.Now(), next) && slices.Contains(s.pollJobs(source), job)
	s.mu.Unlock()
	if !current {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, s.options.Timeout)
	defer cancel()
	err := s.fetchPoll(ctx, generation, revision, source, job)
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation != s.generation || revision != s.workRevision || errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	now := time.Now()
	record := pollRecord{identity: job.identity, finished: now, imageRefresh: job.imageRefresh}
	componentTags := job.kind == pollTags && source.Name != "acs"
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
		if deferred, ok := errors.AsType[*githubread.DeferredError](err); ok {
			if deferred.Until.After(record.retryAfter) {
				record.retryAfter = deferred.Until
			}
			record.continuing = componentTags
		}
		interval := s.options.RefreshIntervals[job.setting]
		delay = record.next(interval, s.startupUntil).Sub(now)
		if record.failures == 1 || previous.next(interval, s.startupUntil).Sub(previous.finished) != delay {
			message := fmt.Sprintf("The last monitoring attempt failed while %s. The next monitoring attempt for %s is in %s.", err, consoleBranch(source), retryDelay(delay))
			s.record(release.Target{Repo: source.Repo}, "", "retry-wait", "", message, "system", message)
		}
	} else if componentTags {
		record.continuing = s.repos[source.Name].TagsPending
	} else if job.kind == pollTags && s.repos[source.Name].TagsPending && now.Before(s.startupUntil) {
		record.retryAfter = now.Add(min(time.Minute, s.options.Poll))
	}
	requested := s.polls[id].tagRefresh
	if requested != job.identity || record.continuing {
		record.tagRefresh = requested
	}
	s.polls[id] = record
}
