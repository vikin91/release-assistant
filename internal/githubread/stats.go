package githubread

import (
	"cmp"
	"context"
	"slices"
	"time"
)

type (
	readWorkKey struct{}
	readWork    struct {
		repository, work string
	}

	// ReadStats contains process-lifetime counters for one repository and kind of work.
	ReadStats struct {
		Repository, Work                               string
		Reads, CycleDeferred, HourlyDeferred           uint64
		PauseDeferred, RateLimited, Canceled, TimedOut uint64
		CIStarts, CICompleted                          uint64
		HTTPWait, MaxHTTPWait, CIWait                  time.Duration
		CIElapsed, MaxCIElapsed                        time.Duration
	}
)

// WithWork labels measurements independently of the request's scheduling priority.
func WithWork(ctx context.Context, repository, work string) context.Context {
	return context.WithValue(ctx, readWorkKey{}, readWork{repository, work})
}

// Stats returns a sorted copy of the current GitHub read measurements.
func (t *Transport) Stats() []ReadStats {
	t.mu.Lock()
	rows := make([]ReadStats, 0, len(t.stats))
	for _, row := range t.stats {
		rows = append(rows, *row)
	}
	t.mu.Unlock()
	slices.SortFunc(rows, func(a, b ReadStats) int {
		return cmp.Or(cmp.Compare(a.Repository, b.Repository), cmp.Compare(a.Work, b.Work))
	})
	return rows
}

// RecordCIStart records the queue wait and returns a function that measures worker occupancy.
func (t *Transport) RecordCIStart(ctx context.Context, due time.Time) func() {
	t.mu.Lock()
	row := t.statsFor(ctx)
	started := t.now()
	row.CIStarts++
	row.CIWait += max(0, started.Sub(due))
	t.mu.Unlock()
	return func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		elapsed := max(0, t.now().Sub(started))
		row.CICompleted++
		row.CIElapsed += elapsed
		row.MaxCIElapsed = max(row.MaxCIElapsed, elapsed)
	}
}

// statsFor returns the counters under the transport lock.
func (t *Transport) statsFor(ctx context.Context) *ReadStats {
	key, ok := ctx.Value(readWorkKey{}).(readWork)
	if !ok {
		class, _ := ctx.Value(requestKey{}).(requestClass)
		key = readWork{class.source, "Other"}
	}
	row := t.stats[key]
	if row == nil {
		row = &ReadStats{Repository: key.repository, Work: key.work}
		t.stats[key] = row
	}
	return row
}
