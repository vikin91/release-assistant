package app

import (
	"context"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"release-assistant/internal/githubread"
	"release-assistant/internal/release"
)

type tagDiscoverFunc func(context.Context, DiscoveryRequest) (release.Repository, error)

func (f tagDiscoverFunc) Discover(ctx context.Context, request DiscoveryRequest) (release.Repository, error) {
	return f(ctx, request)
}

func TestComponentTagBatchesRotateAndResume(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := newFixture()
		s.config = configuration()
		fact, collector := s.config.Components[0], s.config.Components[1]
		var mu sync.Mutex
		var calls []string
		wantCalls := func(want ...string) {
			t.Helper()
			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(calls, want) {
				t.Fatalf("batch turns = %v, want %v", calls, want)
			}
		}
		attempts := make(map[string]int)
		pauseUntil := time.Now().Add(30 * time.Second)
		s.discovery = tagDiscoverFunc(func(_ context.Context, request DiscoveryRequest) (release.Repository, error) {
			name := request.Source.Name
			if name == "acs" || request.Scope != ReadTags {
				t.Fatalf("tag worker issued a different discovery request: %+v", request)
			}
			mu.Lock()
			calls = append(calls, name)
			mu.Unlock()
			attempts[name]++
			repo := f.repos[name]
			if name != collector.Name {
				return repo, nil
			}
			if attempts[name] > 1 {
				current := s.Snapshot()
				if !slices.ContainsFunc(current.Repositories, func(observed release.Repository) bool {
					return observed.Source == collector && observed.TagsPending && len(observed.Tags) == 1 && observed.Tags[0].Evidence.Fresh()
				}) {
					t.Fatal("first batch was not published before the continuation")
				}
			}
			switch attempts[name] {
			case 1:
				repo.TagsPending = true
			case 2:
				repo.TagsPending = true
				return repo, &githubread.DeferredError{Reason: "GitHub allowance", Until: pauseUntil}
			default:
				repo.Tags = append(slices.Clone(repo.Tags), release.Tag{Name: "3.25.7", SHA: "prior", Belongs: release.Yes, Evidence: observed()})
			}
			return repo, nil
		})
		s.options.Poll = time.Second
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { defer close(done); s.monitorComponentTags(ctx, s.generation, s.config.Components) }()
		defer func() { cancel(); <-done }()
		synctest.Wait()
		wantCalls(collector.Name, fact.Name, collector.Name)
		time.Sleep(29 * time.Second)
		synctest.Wait()
		wantCalls(collector.Name, fact.Name, collector.Name)
		time.Sleep(time.Second)
		synctest.Wait()
		cancel()
		<-done
		wantCalls(collector.Name, fact.Name, collector.Name, collector.Name)
		if repo := s.repos[collector.Name]; repo.TagsPending || len(repo.Tags) != 2 {
			t.Fatalf("finished tag inventory = %+v", repo)
		}
	})
}

func TestComponentTagBatchDoesNotOverwriteConcurrentBranchObservation(t *testing.T) {
	s, f := newFixture()
	s.config = configuration()
	source := s.config.Components[0]
	s.repos[source.Name] = f.repos[source.Name]
	s.discovery = tagDiscoverFunc(func(_ context.Context, _ DiscoveryRequest) (release.Repository, error) {
		before := s.repos[source.Name]
		newer := before
		newer.Head = "new-head"
		s.repos[source.Name] = newer
		return before, nil
	})
	if err := s.fetchPoll(t.Context(), s.generation, s.workRevision, source, pollJob{kind: pollTags}); err != nil {
		t.Fatal(err)
	}
	if got := s.repos[source.Name]; got.Head != "new-head" || !got.TagsPending {
		t.Fatalf("a late tag batch overwrote concurrent discovery: %+v", got)
	}
}

func TestManualComponentTagRefreshWakesOnlyThatInventory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := newFixture()
		apply(t, s, Action{Kind: "configure", Config: configuration()})
		fact, collector := s.config.Components[0], s.config.Components[1]
		s.choices.Tags[collector.Name] = f.repos[collector.Name].Tags[0].Name
		s.confirmedSHAs[collector.Name] = f.repos[collector.Name].Tags[0].SHA
		for _, source := range s.config.Components {
			s.polls[pollID{source.Name, pollTags}] = pollRecord{finished: time.Now()}
		}
		requests := make(chan DiscoveryRequest, 3)
		next := release.Tag{Name: "3.25.9", SHA: "new-tag", Belongs: release.Yes, Evidence: observed()}
		s.discovery = tagDiscoverFunc(func(_ context.Context, request DiscoveryRequest) (release.Repository, error) {
			requests <- request
			repo := f.repos[request.Source.Name]
			repo.Tags = append(slices.Clone(repo.Tags), next)
			return repo, nil
		})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { defer close(done); s.monitorComponentTags(ctx, s.generation, s.config.Components) }()
		defer func() { cancel(); <-done }()
		synctest.Wait()

		apply(t, s, Action{Kind: "refresh-tags", Component: collector.Name})
		synctest.Wait()
		if len(requests) != 1 {
			t.Fatalf("expected one immediate request, got %d", len(requests))
		}
		request := <-requests
		if request.Source != collector || request.Scope != ReadTags || request.ConfirmedTag != "" || request.TagRefresh == "" {
			t.Fatalf("unexpected tag refresh: %+v", request)
		}
		snapshot := s.Snapshot()
		if snapshot.RefreshingTags[collector.Name] || snapshot.RefreshingTags[fact.Name] ||
			snapshot.Choices.Tags[collector.Name] != f.repos[collector.Name].Tags[0].Name ||
			!slices.ContainsFunc(snapshot.Repositories, func(repo release.Repository) bool {
				return repo.Source == collector && slices.ContainsFunc(repo.Tags, func(tag release.Tag) bool { return tag.Name == next.Name })
			}) {
			t.Fatalf("requested tag refresh did not complete while preserving the selection: %+v", snapshot.Repositories)
		}
	})
}
