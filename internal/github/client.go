// Package github implements bounded GitHub observations and isolated comment writes.
package github

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"release-assistant/internal/apilog"
	"release-assistant/internal/githubread"
)

const maxPages = 20

type (
	// Client reads GitHub's REST and GraphQL APIs.
	Client struct {
		baseURL      string
		token        string
		http         *http.Client
		backoff      readBackoff
		inventories  sync.Map
		tagPagesMu   sync.Mutex
		tagPages     map[string]tagPage
		rangePRCache sync.Map
		windowCache  *CommitWindowCache
	}
	// requestError separates safe UI causes from endpoint diagnostics.
	requestError struct {
		diagnostic, problem string
		cause               error
		statusCode          int
	}
)

// Error preserves endpoint diagnostics for the activity log.
func (e *requestError) Error() string { return e.diagnostic }

// Unwrap preserves cancellation and deadline classification.
func (e *requestError) Unwrap() error { return e.cause }

func requestProblem(err error) string {
	if failure, ok := errors.AsType[*requestError](err); ok {
		return failure.problem
	}
	return err.Error()
}

func transportProblem(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "Request timed out."
	case errors.Is(err, context.Canceled):
		return "Request canceled."
	default:
		return safeTransportCause(err).Error() + "."
	}
}

// New constructs a GitHub client. An empty base URL uses github.com's public API.
func New(baseURL, token string, httpClient *http.Client) *Client {
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	clientCopy := *httpClient
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), token: token, http: &clientCopy}
}

// WithCommitWindowCache attaches an optional discovery cache before the client serves requests.
func (c *Client) WithCommitWindowCache(cache *CommitWindowCache) *Client {
	c.windowCache = cache
	return c
}

func (c *Client) get(ctx context.Context, endpoint string, dst any) error {
	return c.getObservation(ctx, endpoint, dst, nil)
}

func (c *Client) getObservation(ctx context.Context, endpoint string, dst any, storable *bool) error {
	return c.readObservation(ctx, http.MethodGet, endpoint, nil, dst, storable)
}

func (c *Client) readObservation(ctx context.Context, method, endpoint string, payload io.Reader, dst any, storable *bool) (err error) {
	cycle, _ := ctx.Value(discoveryCycleKey{}).(*discoveryCycle)
	diff, rawDiff := dst.(*pinPRDiff)
	cacheable := cycle != nil && method == http.MethodGet && !rawDiff && cacheableObservation(endpoint)
	var capture bytes.Buffer
	if cacheable {
		if read, ok := cycle.reads[endpoint]; ok {
			if read.err != nil {
				return read.err
			}
			return c.decodeResponse(ctx, method, endpoint, json.NewDecoder(bytes.NewReader(read.data)), dst)
		}
		defer func() { cycle.reads[endpoint] = cachedRead{data: capture.Bytes(), err: err} }()
	}
	if err := c.backoff.problem(); err != nil {
		if cycle != nil {
			cycle.rateErr = err
		}
		return err
	}
	if cycle != nil {
		if err := cycle.take(); err != nil {
			return err
		}
	}
	requestURL := c.baseURL + endpoint
	req, err := http.NewRequestWithContext(ctx, method, requestURL, payload)
	if err != nil {
		return fmt.Errorf("build GitHub %s %s request failed", method, safeEndpoint(requestURL, c.token))
	}
	c.authorize(req)
	if rawDiff {
		req.Header.Set("Accept", "application/vnd.github.diff")
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return &requestError{diagnostic: fmt.Sprintf("GitHub %s %s unavailable: %s", method, safeEndpoint(req.URL.String(), c.token), safeTransportCause(err)), problem: transportProblem(err), cause: safeTransportCause(err)}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		failure := &requestError{diagnostic: fmt.Sprintf("GitHub %s %s rejected (HTTP %d): %s", method, safeEndpoint(req.URL.String(), c.token), resp.StatusCode, httpStatusHint(resp)), problem: fmt.Sprintf("%s (HTTP %d).", httpStatusHint(resp), resp.StatusCode), statusCode: resp.StatusCode}
		if rateLimited(resp) {
			c.backoff.record(resp, failure)
			if cycle != nil {
				cycle.rateErr = failure
			}
		}
		return failure
	}
	const maxResponseBytes = 8 << 20
	limited := &io.LimitedReader{R: resp.Body, N: maxResponseBytes}
	if rawDiff {
		data, err := io.ReadAll(limited)
		if err != nil || limited.N == 0 {
			return fmt.Errorf("GitHub pull request diff is incomplete")
		}
		*diff = pinPRDiff(data)
		return nil
	}
	var body io.Reader = limited
	if cacheable {
		body = io.TeeReader(body, &capture)
	}
	decoder := json.NewDecoder(body)
	if err := c.decodeResponse(ctx, method, endpoint, decoder, dst); err != nil {
		return err
	}
	if storable != nil && windowAllowsStorage(resp) && maxResponseBytes-limited.N <= maxCacheableWindowBytes {
		// Require actual EOF so a truncated or multi-value body cannot seed the cache.
		_, trailing := decoder.Token()
		*storable = trailing == io.EOF && maxResponseBytes-limited.N <= maxCacheableWindowBytes && ctx.Err() == nil
	}
	return nil
}

func (c *Client) decodeResponse(ctx context.Context, method, endpoint string, decoder *json.Decoder, dst any) error {
	if err := decoder.Decode(dst); err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		_, networkFailure := errors.AsType[net.Error](err)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || networkFailure {
			return &requestError{diagnostic: fmt.Sprintf("GitHub %s %s response unavailable: %s", method, safeEndpoint(c.baseURL+endpoint, c.token), safeTransportCause(err)), problem: transportProblem(err), cause: safeTransportCause(err)}
		}
		return &requestError{diagnostic: fmt.Sprintf("GitHub %s %s response malformed", method, safeEndpoint(c.baseURL+endpoint, c.token)), problem: "GitHub returned an invalid JSON response."}
	}
	return nil
}

func safeEndpoint(rawURL, token string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "/"
	}
	return apilog.Resource(parsed, token)
}

func httpStatusHint(resp *http.Response) string {
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return "Authentication failed; GitHub rejected the token"
	case http.StatusForbidden:
		if resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != "" {
			return "GitHub rate limit reached"
		}
		return "Access denied by token permissions or repository policy"
	case http.StatusNotFound:
		return "Not found or inaccessible with the current GitHub token"
	case http.StatusTooManyRequests:
		return "GitHub rate limit reached"
	default:
		if resp.StatusCode >= 500 {
			return "GitHub server error"
		}
		return "GitHub rejected the request"
	}
}

func safeTransportCause(err error) error {
	if deferred, ok := errors.AsType[*githubread.DeferredError](err); ok {
		return deferred
	}
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	}
	if _, ok := errors.AsType[*net.DNSError](err); ok {
		return errors.New("DNS lookup failed")
	}
	if _, ok := errors.AsType[*tls.RecordHeaderError](err); ok {
		return errors.New("TLS handshake failed")
	}
	if _, ok := errors.AsType[tls.RecordHeaderError](err); ok {
		return errors.New("TLS handshake failed")
	}
	if _, ok := errors.AsType[x509.UnknownAuthorityError](err); ok {
		return errors.New("TLS certificate verification failed")
	}
	if _, ok := errors.AsType[x509.CertificateInvalidError](err); ok {
		return errors.New("TLS certificate verification failed")
	}
	if _, ok := errors.AsType[x509.HostnameError](err); ok {
		return errors.New("TLS certificate verification failed")
	}
	if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		return errors.New("network timeout")
	}
	return errors.New("network failure")
}

func (c *Client) authorize(req *http.Request) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

func repoPath(repo string) string { return "/repos/" + repo }
func segment(s string) string     { return url.PathEscape(s) }

func (c *Client) pages(ctx context.Context, endpoint string, consume func(json.RawMessage) error) error {
	for page := 1; page <= maxPages; page++ {
		sep := "?"
		if strings.Contains(endpoint, "?") {
			sep = "&"
		}
		var data []json.RawMessage
		if err := c.get(ctx, fmt.Sprintf("%s%sper_page=100&page=%d", endpoint, sep, page), &data); err != nil {
			return err
		}
		if data == nil {
			return fmt.Errorf("GitHub returned an invalid page")
		}
		for _, item := range data {
			if err := consume(item); err != nil {
				return err
			}
		}
		if len(data) < 100 {
			return nil
		}
	}
	return fmt.Errorf("GitHub result exceeded pagination limit")
}
