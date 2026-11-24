package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"release-assistant/internal/release"
)

func TestTrackImagePublishing(t *testing.T) {
	first := time.Now()
	missing := release.Image{
		Component: "fact", Family: "ci", Reference: "quay.io/fixture/fact:0.3.77", Present: release.No,
		Evidence: release.Evidence{Known: true, At: first},
	}
	later := missing
	later.Evidence.At = first.Add(time.Minute)

	observations := []release.Image{missing}
	trackImagePublishing(observations, nil, func(release.Image) bool { return false })
	if !observations[0].PublishingSince.IsZero() {
		t.Fatal("missing image started the clock without evidence of publication")
	}
	publishing := func(release.Image) bool { return true }
	trackImagePublishing(observations, nil, publishing)
	next := []release.Image{later}
	trackImagePublishing(next, observations, publishing)
	if !next[0].PublishingSince.Equal(first) {
		t.Fatalf("continued absence should keep the original timestamp, got %v", next[0].PublishingSince)
	}
	next[0].Present = release.Yes
	next[0].PublishingSince = time.Time{}
	resumed := []release.Image{later}
	trackImagePublishing(resumed, next, publishing)
	if !resumed[0].PublishingSince.Equal(later.Evidence.At) {
		t.Fatalf("a newly missing image should start a fresh window, got %v", resumed[0].PublishingSince)
	}
}

func TestACSRefreshAppliesImagesWhileDiscoveryIsBlocked(t *testing.T) {
	for name, fail := range map[string]bool{"discovery succeeds": false, "discovery fails": true} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, f := newFixture()
				apply(t, s, Action{Kind: "configure", Config: configuration()})
				f.started = make(chan struct{}, 1)
				f.block = make(chan struct{})
				f.fail["acs"] = fail

				imageStarted := make(chan struct{})
				s.images = imageReaderFunc(func(context.Context, release.Config, map[string]string) (release.Inventory, error) {
					close(imageStarted)
					return release.Inventory{
						ReleaseVersion: "5.0.1",
						Versions:       map[string]string{"fact": "0.3.77"},
						Images:         []release.Image{{Component: "fact", Family: "ci", Reference: "quay.io/fixture/fact:0.3.77", Present: release.Yes, Evidence: observed()}},
						Evidence:       observed(),
					}, nil
				})

				refreshDone := make(chan error, 1)
				go func() {
					refreshDone <- s.Refresh(t.Context(), s.Snapshot().Generation, configuration().ACS(), true)
				}()
				<-f.started
				<-imageStarted
				synctest.Wait()

				snapshot := s.Snapshot()
				if snapshot.Inventory.ReleaseVersion != "5.0.1" || len(snapshot.Inventory.Images) != 1 || snapshot.Inventory.Images[0].Present != release.Yes {
					t.Fatalf("image inspection did not commit while ACS discovery was blocked: %+v", snapshot.Inventory)
				}
				if snapshot.Assessment.Ready {
					t.Fatal("available registry image incorrectly made an incomplete release ready")
				}
				if status := conditionStatus(snapshot.Assessment.Conditions, "ACS release branch pins"); status == release.Yes {
					t.Fatal("image availability cleared unknown GitHub pin evidence")
				}

				close(f.block)
				if err := <-refreshDone; fail && err == nil {
					t.Fatal("expected ACS discovery failure")
				} else if !fail && err != nil {
					t.Fatalf("ACS refresh failed: %v", err)
				}
			})
		})
	}
}

func TestScheduledImageChecksDoNotDelayGitHubOrKonflux(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := newFixture()
		config := configuration()
		source := config.ACS()
		acs := f.repos[source.Name]
		rc := release.Candidate{
			Target: release.Target{Repo: source.Repo, Tag: "5.0.1-rc.0"}, SHA: acs.Head, Evidence: observed(),
		}
		acs.Candidate = rc
		f.repos[source.Name] = acs
		apply(t, s, Action{Kind: "configure", Config: config})
		s.options.RefreshIntervals[ACSPRs] = time.Second
		s.options.RefreshIntervals[KonfluxBuilds] = time.Second
		s.options.RefreshIntervals[ReleaseImages] = time.Hour
		var buildChecks, imageChecks atomic.Int32
		s.planner = buildPollPlanner(func(_ context.Context, target release.Target) (release.Plan, error) {
			if target == rc.Target {
				buildChecks.Add(1)
				return release.Plan{Target: target, SHA: rc.SHA, Evidence: observed()}, nil
			}
			return release.Plan{Target: target}, nil
		})
		s.images = imageReaderFunc(func(ctx context.Context, c release.Config, tags map[string]string) (release.Inventory, error) {
			if tags["acs"] == rc.Target.Tag {
				imageChecks.Add(1)
				<-ctx.Done()
				return release.Inventory{}, ctx.Err()
			}
			return f.Inspect(ctx, c, tags)
		})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { defer close(done); s.Run(ctx) }()
		defer func() { cancel(); <-done }()
		synctest.Wait()
		if got := imageChecks.Load(); got != 1 {
			t.Fatalf("expected image checks to start as soon as the RC was discovered, got %d", got)
		}
		prReads, buildReads := pollCount(f, source.Name, ReadPRs), buildChecks.Load()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if got := pollCount(f, source.Name, ReadPRs); got <= prReads {
			t.Fatalf("GitHub PR discovery stalled during image checks: %d reads", got)
		}
		if got := buildChecks.Load(); got <= buildReads {
			t.Fatalf("Konflux polling stalled during image checks: %d reads", got)
		}
		if got := imageChecks.Load(); got != 1 {
			t.Fatalf("image checks overlapped: %d inspections", got)
		}
	})
}

func TestRefreshImagesDiscardsResultsAfterSelectionChanges(t *testing.T) {
	for name, changeConfig := range map[string]bool{"component tag changes": false, "ACS release configuration changes": true} {
		t.Run(name, func(t *testing.T) {
			s, f := newFixture()
			c := configuration()
			if !changeConfig {
				f.repos["fact"] = addFactTag(f.repos["fact"], "0.3.78")
			}
			apply(t, s, Action{Kind: "configure", Config: c})
			refreshAll(t, s)
			factTag := f.repos["fact"].Tags[0]
			apply(t, s, Action{Kind: "tag", Component: "fact", Tag: factTag.Name, SHA: factTag.SHA})
			generation := s.Snapshot().Generation
			if err := s.refreshImages(t.Context(), generation, false); err != nil {
				t.Fatalf("initial image refresh failed: %v", err)
			}

			inspectStarted := make(chan struct{})
			inspectRelease := make(chan struct{})
			inspectDone := make(chan struct{})
			s.images = imageReaderFunc(func(context.Context, release.Config, map[string]string) (release.Inventory, error) {
				close(inspectStarted)
				<-inspectRelease
				close(inspectDone)
				return release.Inventory{ReleaseVersion: "stale", Versions: map[string]string{"fact": "0.3.77"}, Evidence: observed()}, nil
			})
			refreshDone := make(chan error, 1)
			go func() { refreshDone <- s.refreshImages(t.Context(), generation, false) }()
			<-inspectStarted

			if changeConfig {
				c.Version = "5.0.2"
				apply(t, s, Action{Kind: "configure", Config: c})
			} else {
				next := f.repos["fact"].Tags[1]
				apply(t, s, Action{Kind: "tag", Component: "fact", Tag: next.Name, SHA: next.SHA})
			}
			close(inspectRelease)
			if err := <-refreshDone; err != nil {
				t.Fatalf("stale image refresh returned error: %v", err)
			}
			<-inspectDone

			snapshot := s.Snapshot()
			if snapshot.Inventory.ReleaseVersion == "stale" {
				t.Fatal("image inventory from an obsolete selection was committed")
			}
			if !changeConfig && snapshot.Inventory.Versions["fact"] != "0.3.77" {
				t.Fatalf("component tag change lost the current inventory: %+v", snapshot.Inventory)
			}
		})
	}
}

func TestACSRefreshCancellationUnblocksImageInspection(t *testing.T) {
	s, f := newFixture()
	apply(t, s, Action{Kind: "configure", Config: configuration()})
	f.started = make(chan struct{}, 1)
	f.block = make(chan struct{})

	inspectStarted := make(chan struct{})
	inspectDone := make(chan struct{})
	s.images = imageReaderFunc(func(ctx context.Context, _ release.Config, _ map[string]string) (release.Inventory, error) {
		close(inspectStarted)
		<-ctx.Done()
		close(inspectDone)
		return release.Inventory{}, ctx.Err()
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	refreshDone := make(chan error, 1)
	go func() { refreshDone <- s.Refresh(ctx, s.Snapshot().Generation, configuration().ACS(), true) }()
	<-f.started
	<-inspectStarted
	cancel()
	if err := <-refreshDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("ACS refresh error = %v, want context cancellation", err)
	}
	<-inspectDone
}

func addFactTag(repo release.Repository, version string) release.Repository {
	repo.Tags = append(repo.Tags, release.Tag{Name: version, SHA: "fact-next", Belongs: release.Yes, Evidence: observed()})
	return repo
}

func conditionStatus(conditions []release.Condition, name string) release.Truth {
	for _, condition := range conditions {
		if condition.Name == name {
			return condition.Status
		}
	}
	return ""
}
