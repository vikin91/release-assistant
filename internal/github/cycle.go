package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// maxDiscoveryRequests leaves headroom below 5,000 reads/hour with four five-minute sources.
	maxDiscoveryRequests = 80
	// pinReadReserve keeps routine ACS pinning reads available after inventory work.
	pinReadReserve = 12
)

type (
	discoveryCycleKey struct{}
	discoveryCycle    struct {
		reads                      map[string]cachedRead
		limit, remaining, reserved int
		budgetErr, rateErr         error
	}
	cachedRead struct {
		data json.RawMessage
		err  error
	}
	readBackoff struct {
		mu    sync.Mutex
		until time.Time
		err   error
	}
)

// cacheableObservation excludes mutable PR identities and tag references.
func cacheableObservation(endpoint string) bool {
	parts := strings.SplitN(endpoint, "/", 5)
	if len(parts) != 5 || parts[1] != "repos" {
		return false
	}
	resource := parts[4]
	return strings.HasPrefix(resource, "commits/") ||
		strings.HasPrefix(resource, "contents/") ||
		strings.HasPrefix(resource, "rules/branches/") ||
		(strings.HasPrefix(resource, "branches/") && strings.HasSuffix(resource, "/protection/required_status_checks"))
}

func (d *discoveryCycle) take() error {
	if d.remaining <= d.reserved {
		if d.budgetErr == nil {
			d.budgetErr = fmt.Errorf("internal discovery request budget limit reached (%d reads per cycle, %d reserved by Release Assistant)", d.limit, d.reserved)
		}
		return d.budgetErr
	}
	d.remaining--
	return nil
}

func (b *readBackoff) problem() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if time.Now().Before(b.until) {
		return b.err
	}
	return nil
}

func (b *readBackoff) record(response *http.Response, err error) {
	now := time.Now()
	var until time.Time
	if seconds, parseErr := strconv.ParseUint(response.Header.Get("Retry-After"), 10, 32); parseErr == nil {
		until = now.Add(time.Duration(seconds) * time.Second)
	} else if date, parseErr := http.ParseTime(response.Header.Get("Retry-After")); parseErr == nil {
		until = date
	}
	if response.Header.Get("X-RateLimit-Remaining") == "0" {
		if reset, parseErr := strconv.ParseInt(response.Header.Get("X-RateLimit-Reset"), 10, 64); parseErr == nil && time.Unix(reset, 0).After(until) {
			until = time.Unix(reset, 0)
		}
	}
	if !until.After(now) {
		until = now.Add(time.Minute)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if until.After(b.until) {
		b.until = until
		b.err = fmt.Errorf("GitHub reads deferred until %s after rate limit: %w", until.UTC().Format(time.RFC3339), err)
	}
}

func rateLimited(response *http.Response) bool {
	return response.StatusCode == http.StatusTooManyRequests ||
		(response.StatusCode == http.StatusForbidden &&
			(response.Header.Get("X-RateLimit-Remaining") == "0" || response.Header.Get("Retry-After") != ""))
}
