package app

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"release-assistant/internal/githubread"
	"release-assistant/internal/release"
)

// monitorRelease keeps each repository progressing independently of selected tags.
func (s *Session) monitorRelease(ctx context.Context, generation uint64, config release.Config) {
	if s.options.GitHubReads != nil {
		s.options.GitHubReads.StartMonitoring()
	}
	var monitors sync.WaitGroup
	if s.options.CIPlanner != nil {
		monitors.Go(func() { s.monitorPRRetries(ctx, generation) })
	}
	monitors.Go(func() { s.monitor(ctx, generation, config.ACS(), true) })
	for _, source := range append([]release.Source{config.ACS()}, slices.Clone(config.Components)...) {
		monitors.Go(func() { s.monitor(ctx, generation, source, false) })
	}
	monitors.Wait()
}

func (s *Session) duringStartup() bool {
	return s.options.GitHubReads != nil && s.options.GitHubReads.DuringStartup()
}

// mergeStartupTags attaches independently discovered tags to the branch head observed by PR discovery.
// Before the first PR observation, tags can supply the head but cannot verify the PR inventory.
func (s *Session) mergeStartupTags(source release.Source, latest release.Repository) {
	old := s.repos[source.Name]
	if old.Head != "" && old.Head != latest.Head {
		old.TagsPending = true
	} else {
		old.Source, old.Head = source, latest.Head
		mergeTags(old, &latest, DiscoveryRequest{Scope: ReadTags})
		old.Tags, old.TagsPending = latest.Tags, latest.TagsPending
	}
	s.repos[source.Name] = old
	if !latest.TagsPending && old.Head == latest.Head {
		s.observeDashboardRepo(old, !old.TagsPending, "")
	} else {
		s.observeDashboardRepo(old, false, latest.Evidence.Error)
	}
}

// readStartupTags publishes component choices independently of the concurrent PR refresh.
func (s *Session) readStartupTags(ctx context.Context, generation, revision uint64, source release.Source, usage *githubread.Usage) error {
	ctx, cancel := context.WithTimeout(ctx, s.options.Timeout)
	defer cancel()
	current := func() bool { return generation == s.generation && revision == s.workRevision }
	request := DiscoveryRequest{Source: source, Scope: ReadTags, Progress: func(partial release.Repository) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if current() && ctx.Err() == nil {
			s.mergeStartupTags(source, partial)
		}
	}}
	priority := githubread.Tags
	for {
		before := usage.Calls()
		repo, err := s.discovery.Discover(githubread.WithPriority(ctx, priority, source.Repo), request)
		s.mu.Lock()
		if !current() || ctx.Err() == context.Canceled {
			s.mu.Unlock()
			return nil
		}
		message := ""
		if err != nil {
			repo.TagsPending = true
			message = err.Error()
		}
		s.recordReadResult(release.Target{Repo: source.Repo}, "tags", message, "release tags on "+consoleBranch(source))
		s.mergeStartupTags(source, repo)
		restored := s.verifySavedTag(source, s.repos[source.Name].Tags)
		if s.initializePinnedTags() || restored {
			select {
			case s.changed <- struct{}{}:
			default:
			}
		}
		more := current() && repo.TagsPending && repo.Head == s.repos[source.Name].Head
		s.mu.Unlock()
		if err != nil {
			return fmt.Errorf("reading release tags on %s: %w", consoleBranch(source), err)
		}
		// Yield priority after the first batch. Resume within the same budget
		// only if the reader made progress and there is room for another batch.
		if !more || !s.duringStartup() || ctx.Err() != nil || usage.Calls() == before || usage.Remaining() <= TagBatchRequests {
			return nil
		}
		priority = githubread.Background
	}
}
