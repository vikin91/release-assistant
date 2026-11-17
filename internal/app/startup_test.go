package app

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"release-assistant/internal/release"
)

func TestStartupLogsReadableTagProgress(t *testing.T) {
	for name, failed := range map[string]bool{
		"should describe successful tag discovery":        false,
		"should explain incomplete results after failure": true,
	} {
		t.Run(name, func(t *testing.T) {
			s, fixture := newFixture()
			s.config = configuration()
			fixture.fail["fact"] = failed
			var output bytes.Buffer
			s.log = NewLog(&output)
			s.loadTags(t.Context(), s.generation, s.config.Components[0])
			if !strings.Contains(output.String(), "Info: Checking release tags on fact/release-0.3.\n") {
				t.Fatalf("tag start is unreadable: %s", output.String())
			}
			want := "Info: Found 1 release tag while checking fact/release-0.3. Branch head of fact/release-0.3 is fact-head.\n"
			if failed {
				want = "Info: Found 0 release tags while checking fact/release-0.3. These results are incomplete.\n"
				if !strings.Contains(output.String(), "Error: Could not read release tags on fact/release-0.3. outage\n") {
					t.Fatalf("tag failure was hidden: %s", output.String())
				}
			}
			if !strings.Contains(output.String(), want) {
				t.Fatalf("tag completion is unreadable: %s", output.String())
			}
			if s.log.Recent()[0].Result != "Reading component tags before PRs and build checks." {
				t.Fatal("CLI wording changed the UI log message")
			}
		})
	}
}

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
