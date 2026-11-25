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
		PauseDeferred, RateLimited, Canceled, CIStarts uint64
		HTTPWait, MaxHTTPWait, CIWait                  time.Duration
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

// RecordCIStart measures how long a scheduled CI Retest attempt waited after it was due.
func (t *Transport) RecordCIStart(ctx context.Context, due time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	row := t.statsFor(ctx)
	row.CIStarts++
	row.CIWait += max(0, t.now().Sub(due))
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
