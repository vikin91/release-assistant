package app

import (
	"context"
	"testing"
	"testing/synctest"

	"release-assistant/internal/githubread"
	"release-assistant/internal/release"
)

func TestStartupPollsPRsWithoutDiscoveringComponentTags(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, base := newFixture()
		config := configuration()
		for _, source := range config.Components {
			repo := base.repos[source.Name]
			repo.PRs = []release.PR{{Target: release.Target{Repo: source.Repo, PR: 1}, SHA: "pr", State: "open", Base: source.Branch, Evidence: observed()}}
			base.repos[source.Name] = repo
		}
		apply(t, s, Action{Kind: "configure", Config: config})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { defer close(done); s.Run(ctx) }()
		defer func() { cancel(); <-done }()
		synctest.Wait()
		x := s.Snapshot()
		for _, repo := range x.Repositories {
			if repo.Source.Name != "acs" && (len(repo.PRs) != 1 || !repo.PRs[0].Evidence.Fresh() || len(repo.Tags) != 0) {
				t.Fatalf("startup did not limit component work to the PR inventory: %+v", repo)
			}
		}
	})
}

type pendingStartupReads struct {
	*fixture
	finishPRs chan struct{}
}

func (f *pendingStartupReads) Discover(ctx context.Context, req DiscoveryRequest) (release.Repository, error) {
	repo, err := f.fixture.Discover(ctx, req)
	if err != nil {
		return repo, err
	}
	wait := f.finishPRs
	if req.CandidateProgress != nil {
		req.CandidateProgress(repo.Head, repo.Candidate)
	}
	select {
	case <-wait:
		return repo, nil
	case <-ctx.Done():
		return release.Repository{}, ctx.Err()
	}
}

func TestStartupRCAvailableWhilePRsAreLoading(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, base := newFixture()
		config := configuration()
		acs := base.repos["acs"]
		acs.Candidate = release.Candidate{Target: release.Target{Repo: acs.Source.Repo, Tag: "5.0.1-rc.2"}, SHA: acs.Head, Evidence: observed()}
		base.repos["acs"] = acs
		services := &pendingStartupReads{fixture: base, finishPRs: make(chan struct{})}
		s.discovery, s.options.GitHubReads = services, githubread.New(nil)
		apply(t, s, Action{Kind: "configure", Config: config})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { defer close(done); s.Run(ctx) }()
		defer func() { cancel(); <-done }()
		synctest.Wait()
		snapshot := s.Snapshot()
		if !snapshot.Loading || snapshot.Candidate().Target != acs.Candidate.Target || !snapshot.Candidate().Evidence.Fresh() {
			t.Fatal("verified RC was unavailable before PR discovery completed")
		}
		close(services.finishPRs)
	})
}
