package app

import (
	"context"
	"time"

	"release-assistant/internal/release"
)

// monitorComponentTags runs one inventory batch per turn with the shared GitHub limiter.
func (s *Session) monitorComponentTags(ctx context.Context, generation uint64, sources []release.Source) {
	var lastSource string
	for ctx.Err() == nil {
		source, job, revision, next, changed := s.nextComponentTagPoll(generation, sources, lastSource)
		if source.Repo != "" {
			lastSource = source.Name
			s.runPoll(ctx, generation, revision, source, job)
			continue
		}
		var timer *time.Timer
		var timerC <-chan time.Time
		if !next.IsZero() {
			timer = time.NewTimer(time.Until(next))
			timerC = timer.C
		}
		select {
		case <-ctx.Done():
		case <-changed:
		case <-timerC:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

func (s *Session) nextComponentTagPoll(generation uint64, sources []release.Source, lastSource string) (release.Source, pollJob, uint64, time.Time, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var selected release.Source
	var job pollJob
	var next time.Time
	if generation != s.generation {
		return selected, job, s.workRevision, next, s.pollChanged
	}
	now := time.Now()
	var selectedManual bool
	for _, source := range sources {
		candidate := s.tagPollJob(source)
		record := s.polls[candidate.id(source)]
		at := record.next(s.options.RefreshIntervals[candidate.setting], s.startupUntil)
		if record.due(candidate.identity, now, at) {
			manual := record.tagRefresh != "" && record.identity != candidate.identity
			if selected.Repo == "" || (manual && !selectedManual) ||
				(manual == selectedManual && turnBefore(source.Name, selected.Name, lastSource)) {
				selected, job = source, candidate
				selectedManual = manual
			}
		} else if next.IsZero() || at.Before(next) {
			next = at
		}
	}
	if selected.Repo != "" {
		if resume := s.githubReadResumeAt(); resume.After(now) {
			return release.Source{}, pollJob{}, s.workRevision, resume, s.pollChanged
		}
	}
	return selected, job, s.workRevision, next, s.pollChanged
}

// turnBefore orders ready sources after the last turn, wrapping at the end.
func turnBefore(source, other, last string) bool {
	after, otherAfter := source > last, other > last
	if after != otherAfter {
		return after
	}
	return source < other
}

// mergeTagProgress publishes verified items while the inventory is still loading.
func (s *Session) mergeTagProgress(source release.Source, partial release.Repository) {
	old := s.repos[source.Name]
	if old.Head != "" && old.Head != partial.Head {
		return
	}
	old.Source, old.Head = source, partial.Head
	partial.TagsPending = true
	mergeTags(old, &partial, DiscoveryRequest{Scope: ReadTags})
	old.Tags, old.TagsPending = partial.Tags, true
	s.repos[source.Name] = old
}
