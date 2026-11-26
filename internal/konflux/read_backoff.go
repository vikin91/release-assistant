package konflux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	readTimeout  = 8 * time.Second
	probeTimeout = 3 * time.Second
	firstPause   = time.Minute
	maxPause     = 5 * time.Minute
	issueWindow  = 10 * time.Minute

	pipelineRunAPI = "PipelineRun API"
)

type (
	// ReadIssue summarizes an endpoint's reachability or authentication failure.
	ReadIssue struct {
		Cluster, Endpoint, Reason string
		LastFailure, RetryAt      time.Time
		Probing                   bool
		Unauthorized              bool
	}
	readCycleKey struct{}
	readCycle    struct {
		id       uint64
		deadline time.Time
	}
	planReadsKey struct{}
	planReads    struct {
		unavailable, deferred atomic.Bool
		login                 *ocLogin
	}
	readBackoff struct {
		base          http.RoundTripper
		now           func() time.Time
		mu            sync.Mutex
		endpoints     map[string]*endpointBackoff
		onPauseChange func(ReadIssue)
	}
	endpointBackoff struct {
		ReadIssue
		unauthorizedAt time.Time
		lastCycle      uint64
		epoch          uint64
		pause          time.Duration
	}
	readBody struct {
		io.ReadCloser
		finish func(error)
	}
)

var readCycleID atomic.Uint64

// WithReadCycle lets every lookup in one refresh contribute at most one failure per endpoint.
func WithReadCycle(ctx context.Context) context.Context {
	deadline, _ := ctx.Deadline()
	return context.WithValue(ctx, readCycleKey{}, readCycle{id: readCycleID.Add(1), deadline: deadline})
}

func (t *readBackoff) issues() []ReadIssue {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	var issues []ReadIssue
	for _, state := range t.endpoints {
		if !state.LastFailure.IsZero() && (state.pause > 0 || now.Sub(state.LastFailure) < issueWindow) {
			issues = append(issues, state.ReadIssue)
		}
		if !state.unauthorizedAt.IsZero() && now.Sub(state.unauthorizedAt) < issueWindow {
			issues = append(issues, ReadIssue{
				Cluster: state.Cluster, Endpoint: state.Endpoint, Reason: "HTTP 401 Unauthorized",
				LastFailure: state.unauthorizedAt, Unauthorized: true,
			})
		}
	}
	slices.SortFunc(issues, func(a, b ReadIssue) int {
		if a.Cluster != b.Cluster {
			return strings.Compare(a.Cluster, b.Cluster)
		}
		return strings.Compare(a.Endpoint, b.Endpoint)
	})
	return issues
}

func (t *readBackoff) RoundTrip(req *http.Request) (*http.Response, error) {
	cluster, endpoint := readEndpoint(req)
	if cluster == "" {
		return t.base.RoundTrip(req)
	}
	reads, _ := req.Context().Value(planReadsKey{}).(*planReads)
	// The plan supplies the port from the same login as its token.
	if endpoint == pipelineRunAPI && reads != nil && reads.login != nil &&
		reads.login.port != "" && req.URL.Hostname() == reads.login.host {
		req = req.Clone(req.Context())
		req.URL.Host = net.JoinHostPort(reads.login.host, reads.login.port)
		req.Host = req.URL.Host
	}
	ctx := req.Context()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cycle, ok := ctx.Value(readCycleKey{}).(readCycle)
	if !ok {
		return t.base.RoundTrip(req)
	}
	deadline, hasDeadline := ctx.Deadline()
	enoughTime := !hasDeadline || time.Until(deadline) >= probeTimeout
	host := req.URL.Host
	now := t.now()
	t.mu.Lock()
	state := t.endpoints[host]
	if state == nil {
		state = &endpointBackoff{ReadIssue: ReadIssue{Cluster: cluster, Endpoint: endpoint}}
		t.endpoints[host] = state
	}
	probe := state.pause > 0
	if probe && (state.Probing || now.Before(state.RetryAt) || !enoughTime) {
		t.mu.Unlock()
		if reads != nil {
			reads.deferred.Store(true)
		}
		return nil, errors.New("read deferred by endpoint backoff")
	}
	epoch := state.epoch
	state.Probing = probe
	t.mu.Unlock()

	timeout := readTimeout
	if probe {
		timeout = probeTimeout
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	var status int
	var once sync.Once
	complete := func(err error) {
		once.Do(func() {
			defer cancel()
			now := t.now()
			callerExpired := !cycle.deadline.IsZero() && !time.Now().Before(cycle.deadline)
			switch {
			case status >= 500 || status == http.StatusTooManyRequests:
				if reads != nil {
					reads.unavailable.Store(true)
				}
				t.observe(state, cycle.id, epoch, now, fmt.Sprintf("HTTP %d", status), 0)
			case err == nil:
				t.observe(state, cycle.id, epoch, now, "", status)
			case !enoughTime || callerExpired || errors.Is(ctx.Err(), context.Canceled):
				t.observe(state, cycle.id, epoch, now, "", 0)
			default:
				reason := ""
				if networkErr, ok := errors.AsType[net.Error](err); ok {
					reason = "network request failed"
					if networkErr.Timeout() {
						reason = "request timed out"
					}
				} else if errors.Is(err, io.ErrUnexpectedEOF) {
					reason = "response ended early"
				}
				if reads != nil && reason != "" {
					reads.unavailable.Store(true)
				}
				t.observe(state, cycle.id, epoch, now, reason, 0)
			}
		})
	}
	resp, err := t.base.RoundTrip(req.WithContext(requestCtx))
	if err != nil {
		complete(err)
		return resp, err
	}
	status = resp.StatusCode
	if status < 200 || status >= 300 {
		complete(nil)
		return resp, nil
	}
	resp.Body = &readBody{ReadCloser: resp.Body, finish: complete}
	return resp, nil
}

func (t *readBackoff) observe(state *endpointBackoff, cycle, epoch uint64, now time.Time, reason string, responseStatus int) {
	t.mu.Lock()
	wasPaused := state.pause > 0
	defer func() {
		changed := wasPaused != (state.pause > 0)
		issue := state.ReadIssue
		t.mu.Unlock()
		if changed && t.onPauseChange != nil {
			t.onPauseChange(issue)
		}
	}()
	if state.epoch != epoch {
		return
	}
	state.Probing = false
	if responseStatus != 0 {
		// An HTTP response verifies reachability. Only a successful read clears a 401.
		switch {
		case responseStatus == http.StatusUnauthorized:
			state.unauthorizedAt = now
		case responseStatus >= 200 && responseStatus < 300:
			state.unauthorizedAt = time.Time{}
		}
		if state.pause > 0 {
			state.epoch++
		}
		state.LastFailure, state.RetryAt = time.Time{}, time.Time{}
		state.Reason, state.pause = "", 0
		return
	}
	if reason == "" {
		if state.pause > 0 {
			state.RetryAt = now.Add(state.pause)
		}
		return
	}
	if state.pause > 0 {
		state.pause = min(state.pause*2, maxPause)
	} else {
		if cycle == state.lastCycle {
			return
		}
		if !state.LastFailure.IsZero() && now.Sub(state.LastFailure) < issueWindow {
			state.pause = firstPause
			state.epoch++
		}
	}
	state.lastCycle, state.LastFailure, state.Reason = cycle, now, reason
	if state.pause > 0 {
		state.RetryAt = now.Add(state.pause)
	}
}

func (b *readBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) {
		b.finish(nil)
	} else if err != nil {
		b.finish(err)
	}
	return n, err
}

func (b *readBody) Close() error {
	err := b.ReadCloser.Close()
	b.finish(err)
	return err
}

func readEndpoint(req *http.Request) (cluster, endpoint string) {
	if req.Method != http.MethodGet || req.URL.Scheme != "https" || req.URL.Port() != "" {
		return "", ""
	}
	host := req.URL.Hostname()
	if !strings.HasSuffix(host, ".openshiftapps.com") {
		return "", ""
	}
	switch {
	case strings.HasPrefix(req.URL.Path, "/apis/tekton.dev/"):
		if cluster, ok := strings.CutPrefix(host, "api."); ok {
			return cluster, pipelineRunAPI
		}
	case strings.HasPrefix(req.URL.Path, "/apis/results.tekton.dev/"):
		if cluster, ok := strings.CutPrefix(host, "tekton-results-tekton-results.apps."); ok {
			return cluster, "Tekton Results"
		}
	}
	return "", ""
}
