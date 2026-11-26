package app

import (
	"context"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"release-assistant/internal/release"
)

type manualTagDiscovery struct {
	*fixture
	names []string
	lists []release.Source
}

func (f *manualTagDiscovery) ReadComponentTagNames(_ context.Context, source release.Source) ([]string, error) {
	f.lists = append(f.lists, source)
	return slices.Clone(f.names), nil
}

func TestManualComponentTagDiscoveryUsesOnlyTheRequestedScope(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := newFixture()
		apply(t, s, Action{Kind: "configure", Config: configuration()})
		source := s.config.Components[0]
		s.tagChoiceLimit = 2
		d := &manualTagDiscovery{fixture: f, names: []string{"0.3.83", "0.3.82", "0.3.81"}}
		s.discovery = d
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { defer close(done); s.monitorComponentTags(ctx, s.generation, s.config.Components) }()
		defer func() { cancel(); <-done }()
		synctest.Wait()
		time.Sleep(time.Hour)
		synctest.Wait()
		if len(d.lists) != 0 || len(f.requests) != 0 {
			t.Fatal("startup or polling read component tag choices without a click")
		}

		apply(t, s, Action{Kind: "fetch-tag-names", Component: source.Name})
		synctest.Wait()
		if !slices.Equal(d.lists, []release.Source{source}) || len(f.requests) != 0 {
			t.Fatalf("fetch names issued discovery reads: lists=%v requests=%v", d.lists, f.requests)
		}
		snapshot := s.Snapshot()
		if !slices.Equal(snapshot.ComponentTagNames[source.Name].Names, d.names) || !snapshot.ComponentTagNames[source.Name].Evidence.Fresh() {
			t.Fatalf("tag names were not displayed: %+v", snapshot.ComponentTagNames)
		}

		apply(t, s, Action{Kind: "verify-recent-tags", Component: source.Name})
		synctest.Wait()
		if len(d.lists) != 1 || len(f.requests) != 1 {
			t.Fatalf("verification issued unexpected reads: lists=%v requests=%v", d.lists, f.requests)
		}
		request := f.requests[0]
		if request.Source != source || request.Scope != ReadTags || !slices.Equal(request.TagCandidates, d.names[:2]) {
			t.Fatalf("verification request = %+v", request)
		}
	})
}

func TestManualVerificationDoesNotOverwriteConcurrentBranchObservation(t *testing.T) {
	s, f := newFixture()
	s.config = configuration()
	source := s.config.Components[0]
	current := f.repos[source.Name]
	s.repos[source.Name] = current
	newer := current
	newer.Head = "new-head"
	s.repos[source.Name] = newer
	err := s.mergeRecentTagVerification(source, current.Head, []string{current.Tags[0].Name}, current, nil)
	if got := s.repos[source.Name]; got.Head != "new-head" || err == nil {
		t.Fatalf("a late verification overwrote concurrent discovery: head=%s error=%v", got.Head, err)
	}
}
