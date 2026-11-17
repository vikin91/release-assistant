package app

import (
	"context"
	"testing"
	"testing/synctest"

	"release-assistant/internal/release"
)

type startupServices struct {
	*fixture
	tagStarted chan struct{}
}

func (f *startupServices) Discover(ctx context.Context, req DiscoveryRequest) (release.Repository, error) {
	if req.Scope == ReadTags {
		select {
		case f.tagStarted <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return release.Repository{}, ctx.Err()
	}
	return f.fixture.Discover(ctx, req)
}

func TestStartupPRDiscoveryDoesNotWaitForTags(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, base := newFixture()
		config := configuration()
		for _, source := range config.Components {
			repo := base.repos[source.Name]
			repo.Tags = nil
			repo.PRs = []release.PR{{Target: release.Target{Repo: source.Repo, PR: 1}, SHA: "pr", State: "open", Base: source.Branch, Evidence: observed()}}
			base.repos[source.Name] = repo
		}
		apply(t, s, Action{Kind: "configure", Config: config})
		services := &startupServices{fixture: base, tagStarted: make(chan struct{}, len(config.Components))}
		s.discovery = services
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { defer close(done); s.Run(ctx) }()
		synctest.Wait()
		defer func() { cancel(); <-done }()
		for range config.Components {
			<-services.tagStarted
		}
		x := s.Snapshot()
		for _, repo := range x.Repositories {
			if repo.Source.Name != "acs" && (len(repo.PRs) != 1 || !repo.PRs[0].Evidence.Fresh()) {
				t.Fatalf("PR inventory waited for missing tags: %+v", repo)
			}
		}
		if len(x.Choices.Tags) != 0 {
			t.Fatal("startup selected a component tag")
		}
	})
}
