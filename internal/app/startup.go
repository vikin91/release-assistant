package app

import (
	"context"
	"slices"
	"sync"

	"release-assistant/internal/release"
)

// monitorRelease keeps each repository progressing independently of selected tags.
func (s *Session) monitorRelease(ctx context.Context, generation uint64, config release.Config) {
	var monitors sync.WaitGroup
	for _, source := range append([]release.Source{config.ACS()}, slices.Clone(config.Components)...) {
		monitors.Go(func() { s.monitor(ctx, generation, source) })
	}
	monitors.Wait()
}
