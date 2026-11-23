package app

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"release-assistant/internal/konflux"
)

func TestKonfluxHealthPollingStartsImmediatelyAndFollowsStatus(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		session, _ := newFixture()
		observations := []konflux.Health{
			{Problem: "unavailable"},
			{APIReadable: true},
			{APIReadable: true, AdmittedAt: time.Now()},
			{Problem: "unavailable"},
			{APIReadable: true},
		}
		calls := make(chan time.Time, len(observations))
		session.options.KonfluxProbe = func(context.Context) konflux.Health {
			calls <- time.Now()
			result := observations[0]
			observations = observations[1:]
			return result
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		start := time.Now()
		go func() {
			defer close(done)
			session.Run(ctx)
		}()
		synctest.Wait()
		for i, interval := range []time.Duration{30 * time.Second, 30 * time.Second, 2 * time.Minute, 30 * time.Second, 30 * time.Second} {
			select {
			case at := <-calls:
				if !at.Equal(start) {
					t.Fatalf("probe %d started at %v, want %v", i, at, start)
				}
			default:
				t.Fatalf("probe %d did not run at %v", i, start)
			}
			health := session.Snapshot().KonfluxHealth
			if !health.Next.Equal(start.Add(interval)) {
				t.Fatalf("probe %d next check = %v, want %v", i, health.Next, start.Add(interval))
			}
			if i == 4 {
				break
			}
			time.Sleep(interval - time.Second)
			synctest.Wait()
			if len(calls) != 0 {
				t.Fatalf("probe %d ran before its interval elapsed", i+1)
			}
			time.Sleep(time.Second)
			synctest.Wait()
			start = start.Add(interval)
		}
		cancel()
		<-done
	})
}
