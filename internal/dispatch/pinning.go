package dispatch

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"release-assistant/internal/release"
)

type (
	// PinningWriter performs the GitHub writes for one approved pinning PR.
	PinningWriter interface {
		OpenPinningPR(context.Context, release.PinningRequest) (int, error)
	}
	// PinningReader verifies duplicate evidence against the current branch and open PRs.
	PinningReader interface {
		ReadPinningDiffs(context.Context, release.PinningRequest, map[int]release.PinFileMatch) (release.PinDiscovery, error)
	}
	// PinningReceipt identifies the PR confirmed by GitHub.
	PinningReceipt struct {
		Number  int
		URL     string
		Branch  string
		Changes []release.PinChange
		At      time.Time
	}
)

// PinningAvailable reports whether the runtime provides a writer and a live verifier.
func (d *Dispatcher) PinningAvailable() bool {
	_, canRead := d.reader.(PinningReader)
	return canRead && d.options.PinningWriter != nil
}

// CreatePinningPR admits one explicit request and rechecks it before any GitHub write.
func (d *Dispatcher) CreatePinningPR(ctx context.Context, req release.PinningRequest, previous map[int]release.PinFileMatch, stillCurrent func() bool) (PinningReceipt, release.PinDiscovery, error) {
	if !validPinningRequest(req) {
		return PinningReceipt{}, release.PinDiscovery{}, fmt.Errorf("invalid StackRox pinning request")
	}
	scope, scoped := ctx.Value(scopeKey{}).(uint64)
	d.mu.Lock()
	switch {
	case !scoped || !d.options.EnableWrites || !d.PinningAvailable() || d.stopped || scope != d.scope:
		d.mu.Unlock()
		return PinningReceipt{}, release.PinDiscovery{}, fmt.Errorf("pinning PR creation is unavailable")
	case d.pinningInFlight:
		d.mu.Unlock()
		return PinningReceipt{}, release.PinDiscovery{}, fmt.Errorf("pinning PR creation is already in progress")
	}
	d.pinningInFlight = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.pinningInFlight = false
		d.mu.Unlock()
	}()

	reader := d.reader.(PinningReader)
	diff, err := reader.ReadPinningDiffs(ctx, req, previous)
	if err != nil {
		return PinningReceipt{}, diff, err
	}
	if duplicate := diff.Duplicate(release.PinSignature(req.Changes)); duplicate != 0 {
		return PinningReceipt{}, diff, fmt.Errorf("open PR #%d already proposes the same changes", duplicate)
	}
	if !diff.Evidence.Fresh() {
		return PinningReceipt{}, diff, fmt.Errorf("could not verify whether an identical PR is open")
	}
	if stillCurrent == nil || !stillCurrent() {
		return PinningReceipt{}, diff, fmt.Errorf("the selected tags, pins, or image readiness changed; review the updated page")
	}
	d.mu.Lock()
	allowed := !d.stopped && d.scope == scope && ctx.Err() == nil
	version := d.version
	d.mu.Unlock()
	if !allowed {
		return PinningReceipt{}, diff, fmt.Errorf("the active release changed or the request was canceled")
	}
	number, err := d.options.PinningWriter.OpenPinningPR(ctx, req)
	receipt := PinningReceipt{}
	decision := "pinning-pr"
	result := "Created PR #" + strconv.Itoa(number)
	if err == nil && number > 0 {
		receipt = PinningReceipt{
			Number: number, URL: "https://github.com/" + req.Repo + "/pull/" + strconv.Itoa(number),
			Branch: req.Branch, Changes: slices.Clone(req.Changes), At: d.options.Now(),
		}
	} else {
		if err == nil {
			err = fmt.Errorf("GitHub did not confirm the pull request number")
		}
		decision = "pinning-pr-error"
		result = err.Error()
	}
	d.mu.Lock()
	d.logEvent(version, release.Target{Repo: req.Repo, PR: number}, req.Head, decision, req.Branch, result, "user")
	d.mu.Unlock()
	return receipt, diff, err
}

func validPinningRequest(req release.PinningRequest) bool {
	paths := release.PinPaths(req.Version)
	if req.Repo != "stackrox/stackrox" || !release.ValidRef(req.Branch) || req.Head == "" || len(req.Changes) == 0 || len(req.Changes) > len(paths) {
		return false
	}
	seen := make(map[string]bool, len(req.Changes))
	for _, change := range req.Changes {
		validPath := false
		for _, path := range paths {
			validPath = validPath || change.Path == path
		}
		before, after := strings.TrimSpace(change.Before), strings.TrimSpace(change.After)
		expected, singleLine := release.ReplacementPin(change.Before, strings.TrimPrefix(after, "v"))
		if !validPath || seen[change.Path] || before == after || !singleLine || expected != change.After {
			return false
		}
		if _, _, _, ok := release.Version(after); !ok {
			return false
		}
		seen[change.Path] = true
	}
	return true
}
