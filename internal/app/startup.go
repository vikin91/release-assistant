package app

import (
	"context"
	"slices"
	"sync"

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
	monitors.Go(func() { s.monitorComponentTags(ctx, generation, config.Components) })
	monitors.Go(func() { s.monitor(ctx, generation, config.ACS(), true) })
	for _, source := range append([]release.Source{config.ACS()}, slices.Clone(config.Components)...) {
		monitors.Go(func() { s.monitor(ctx, generation, source, false) })
	}
	monitors.Wait()
}

func (s *Session) duringStartup() bool {
	return s.options.GitHubReads != nil && s.options.GitHubReads.DuringStartup()
}
