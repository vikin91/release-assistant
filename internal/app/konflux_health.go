package app

import (
	"context"
	"time"
)

func (s *Session) monitorKonfluxHealth(ctx context.Context) {
	for ctx.Err() == nil {
		if s.options.Profiles {
			s.mu.Lock()
			var next time.Time
			if s.konfluxHealth != nil {
				next = s.konfluxHealth.Next
			}
			s.mu.Unlock()
			if time.Now().Before(next) {
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Until(next)):
				}
			}
		}
		started := time.Now()
		probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		health := s.options.KonfluxProbe(probeCtx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		interval := 30 * time.Second
		if health.Healthy() {
			interval = 2 * time.Minute
		}
		health.CheckedAt, health.Next = time.Now(), started.Add(interval)
		s.mu.Lock()
		s.konfluxHealth = &health
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(health.Next)):
		}
	}
}
