package app

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"release-assistant/internal/githubread"
	"release-assistant/internal/release"
)

// monitorRelease publishes component choices before starting lower-priority reads.
func (s *Session) monitorRelease(ctx context.Context, generation uint64, config release.Config) {
	var first sync.WaitGroup
	for _, source := range config.Components {
		first.Go(func() { s.loadTags(ctx, generation, source) })
	}
	first.Wait()
	if ctx.Err() != nil {
		return
	}
	var monitors sync.WaitGroup
	for _, source := range append([]release.Source{config.ACS()}, config.Components...) {
		monitors.Go(func() { s.monitor(ctx, generation, source) })
	}
	monitors.Wait()
}

func (s *Session) loadTags(ctx context.Context, generation uint64, source release.Source) {
	s.mu.Lock()
	if s.generation != generation || s.tagsLoaded[source.Name] || s.choices.Tags[source.Name] != "" {
		s.mu.Unlock()
		return
	}
	version := s.config.Version
	s.record(release.Target{Repo: source.Repo}, "", "tags-start", "", "Reading component tags before PRs and build checks.", "system", fmt.Sprintf("Checking release tags on %s/%s.", consoleTarget(release.Target{Repo: source.Repo}), source.Branch))
	s.mu.Unlock()
	ctx = githubread.WithPriority(ctx, githubread.Tags, source.Repo)
	ctx, usage := githubread.Track(ctx, 80)
	ctx, cancel := context.WithTimeout(ctx, s.options.Timeout)
	defer cancel()
	request := DiscoveryRequest{Source: source, Version: version, Scope: ReadTags}
	request.Progress = func(repo release.Repository) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if ctx.Err() == nil && s.generation == generation {
			s.publishTags(source, repo)
		}
	}
	repo, err := s.discovery.Discover(ctx, request)
	for repo.TagsPending && err == nil && ctx.Err() == nil {
		request.Progress(repo)
		repo, err = s.discovery.Discover(ctx, request)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil || s.generation != generation {
		return
	}
	s.publishTags(source, repo)
	s.tagsLoaded[source.Name] = true
	s.initiallyLoaded[source.Name] = true
	message := ""
	if err != nil {
		message = err.Error()
	}
	s.recordReadResult(release.Target{Repo: source.Repo}, "tags", message)
	message = fmt.Sprintf("Found %s in %s after %s.", counted(len(repo.Tags), "release tag"), consoleTarget(release.Target{Repo: source.Repo}), counted(int(usage.Calls()), "GitHub request"))
	if err != nil || repo.TagsPending {
		message += " These results are incomplete."
	}
	s.record(release.Target{Repo: source.Repo}, repo.Head, "tags-complete", "", fmt.Sprintf("Observed %d component tags; complete=%t; GitHub reads=%d.", len(repo.Tags), err == nil && !repo.TagsPending, usage.Calls()), "system", message)
}

// publishTags merges only tag observations; unrelated evidence keeps its provenance.
// The caller holds the session lock.
func (s *Session) publishTags(source release.Source, repo release.Repository) {
	old := s.repos[source.Name]
	old.Source = source
	old.Head, old.Evidence = repo.Head, repo.Evidence
	old.Tags = slices.Clone(repo.Tags)
	old.TagsPending = repo.TagsPending
	for i := range old.Tags {
		old.Tags[i].Contains = nil
	}
	if old.Evidence.At.IsZero() {
		old.Evidence.At = time.Now()
	}
	s.repos[source.Name] = old
}
