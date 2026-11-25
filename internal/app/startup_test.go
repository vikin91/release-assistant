package app

import (
	"context"
	"testing"
	"testing/synctest"

	"release-assistant/internal/githubread"
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
		s.discovery, s.options.GitHubReads = services, githubread.New(nil)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { defer close(done); s.Run(ctx) }()
		synctest.Wait()
		defer func() { cancel(); <-done }()
		select {
		case <-services.tagStarted:
		default:
			t.Fatal("no tag batch started")
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

type pendingStartupReads struct {
	*fixture
	finishPRs, finishTags chan struct{}
}

func (f *pendingStartupReads) Discover(ctx context.Context, req DiscoveryRequest) (release.Repository, error) {
	repo, err := f.fixture.Discover(ctx, req)
	if err != nil {
		return repo, err
	}
	wait := f.finishPRs
	if req.Progress != nil {
		partial := repo
		partial.TagsPending = true
		req.Progress(partial)
		wait = f.finishTags
	} else if req.CandidateProgress != nil {
		req.CandidateProgress(repo.Head, repo.Candidate)
	}
	select {
	case <-wait:
		return repo, nil
	case <-ctx.Done():
		return release.Repository{}, ctx.Err()
	}
}

func TestStartupChoicesAvailableWhilePRsAndTagsAreLoading(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, base := newFixture()
		config := configuration()
		acs := base.repos["acs"]
		acs.Candidate = release.Candidate{Target: release.Target{Repo: acs.Source.Repo, Tag: "5.0.1-rc.2"}, SHA: acs.Head, Evidence: observed()}
		base.repos["acs"] = acs
		services := &pendingStartupReads{fixture: base, finishPRs: make(chan struct{}), finishTags: make(chan struct{})}
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
		source := config.Components[1]
		tag := base.repos[source.Name].Tags[0]
		apply(t, s, Action{Kind: "tag", Component: source.Name, Tag: tag.Name, SHA: tag.SHA})
		close(services.finishPRs)
		close(services.finishTags)
		synctest.Wait()
		if got := s.Snapshot().Choices.Tags[source.Name]; got != tag.Name {
			t.Fatalf("selected %q, want %q", got, tag.Name)
		}
	})
}

func TestStartupTagsDiscardedAfterBranchHeadChanges(t *testing.T) {
	s, f := newFixture()
	current := f.repos["fact"]
	s.repos["fact"] = current
	earlier := release.Repository{Source: current.Source, Head: "earlier", Tags: []release.Tag{
		{Name: "0.3.1", SHA: "old-tag", Belongs: release.Yes, Evidence: observed()},
	}}
	s.mergeTagProgress(current.Source, earlier)
	got := s.repos["fact"]
	if got.Head != current.Head || got.TagsPending || len(got.Tags) != 1 || got.Tags[0].SHA != current.Tags[0].SHA {
		t.Fatalf("tag batch crossed branch heads: %+v", got)
	}
}
