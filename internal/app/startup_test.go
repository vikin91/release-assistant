package app

import (
	"context"
	"sync/atomic"
	"testing"

	"release-assistant/internal/release"
)

type startupServices struct {
	*fixture
	started chan release.Source
	lower   atomic.Int64
}

func (f *startupServices) Discover(ctx context.Context, req DiscoveryRequest) (release.Repository, error) {
	if req.Scope != ReadTags {
		f.lower.Add(1)
		return release.Repository{}, ctx.Err()
	}
	repo := release.Repository{Source: req.Source, Head: req.Source.Name + "-head", Evidence: observed(), Tags: []release.Tag{{Name: req.Source.Stream + ".1", SHA: req.Source.Name + "-tag", Belongs: release.Yes, Evidence: observed()}}}
	req.Progress(repo)
	f.started <- req.Source
	<-ctx.Done()
	return release.Repository{}, ctx.Err()
}

func (f *startupServices) Plan(context.Context, release.Target) (release.Plan, error) {
	f.lower.Add(1)
	return release.Plan{}, nil
}
func (f *startupServices) Inspect(context.Context, release.Config, map[string]string) (release.Inventory, error) {
	f.lower.Add(1)
	return release.Inventory{}, nil
}

func TestStartupPublishesSelectableTagsWhileDiscoveryIsPending(t *testing.T) {
	s, base := newFixture()
	config := release.Config{Version: "4.10.12", Branch: "release-4.10", PinningPR: 23958, Components: release.DefaultsFor("4.10.12")}
	for i := range config.Components {
		config.Components[i].Branch = "release-" + config.Components[i].Stream
	}
	apply(t, s, Action{Kind: "configure", Config: config})
	services := &startupServices{fixture: base, started: make(chan release.Source, 3)}
	s.discovery, s.planner, s.images = services, services, services
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	defer func() { cancel(); <-done }()
	for range config.Components {
		<-services.started
	}
	snapshot := s.Snapshot()
	if len(snapshot.Repositories) != 3 || services.lower.Load() != 0 {
		t.Fatalf("tags were delayed by unrelated work: repos=%d lower=%d", len(snapshot.Repositories), services.lower.Load())
	}
	for _, repo := range snapshot.Repositories {
		if len(repo.Tags) != 1 || !repo.Tags[0].Evidence.Fresh() || repo.Tags[0].Belongs != release.Yes {
			t.Fatalf("unusable early tag: %+v", repo)
		}
	}
	first := snapshot.Repositories[0]
	if err := s.Apply(t.Context(), Action{Kind: "tag", Generation: snapshot.Generation, Component: first.Source.Name, Tag: first.Tags[0].Name, SHA: first.Tags[0].SHA}); err != nil {
		t.Fatalf("cannot select a tag while other observations are pending: %v", err)
	}
}
