package dispatch

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"sync"
	"time"

	"release-assistant/internal/release"
)

type (
	// Options configures startup write permission, attempt limits, and time for tests.
	Options struct {
		// EnableWrites allows grants and dispatch for this process; the default is read-only.
		EnableWrites bool
		Gap, SlowGap time.Duration
		Threshold    int
		Now          func() time.Time
	}
	// Event records a permission decision or one delivery attempt without credentials.
	Event struct {
		At                                     time.Time
		Target                                 release.Target
		SHA, Decision, Payload, Result, Origin string
	}
	// Offer is a proposal plus the current dispatcher-owned authority and cooldown.
	Offer struct {
		ID                  string
		Proposal            release.Proposal
		Allowed, CanApprove bool
		// Queueable remains true during a cooldown or an in-flight write on the target.
		Queueable bool
		Failures  int
		// CommentSent reports successful delivery of this exact proposal.
		CommentSent  bool
		WaitingUntil time.Time
		Reason       string
	}
	// View exposes authority separately from release inclusion.
	View struct {
		Target                   release.Target
		SHA                      string
		Granted, Ignored, OptOut bool
		Offers                   []Offer
		Message                  string
		Warnings                 []string
	}
	// Dispatcher owns all local write authority and serializes attempt admission.
	Dispatcher struct {
		mu                                   sync.Mutex
		reader                               Reader
		planner                              Planner
		writer                               Writer
		options                              Options
		targets                              map[release.Target]*targetState
		interactive, requirePRLabel, stopped bool
		events                               []Event
	}
	targetState struct {
		identity                   Identity
		granted, ignored, inFlight bool
		epoch                      uint64
		proposals                  []release.Proposal
		approved, sent             map[string]bool
		jobs                       map[string]*jobState
		last                       time.Time
		message                    string
	}
	jobState struct {
		seen  map[string]bool
		count int
		last  time.Time
	}
)

// New starts interactive with all grants off; writes require EnableWrites.
func New(r Reader, p Planner, w Writer, o Options) *Dispatcher {
	if o.Gap <= 0 {
		o.Gap = time.Minute
	}
	if o.SlowGap <= 0 {
		o.SlowGap = time.Hour
	}
	if o.Threshold <= 0 {
		o.Threshold = 10
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Dispatcher{reader: r, planner: p, writer: w, options: o, targets: map[release.Target]*targetState{}, interactive: true}
}
func (d *Dispatcher) state(t release.Target) *targetState {
	s := d.targets[t]
	if s == nil {
		s = &targetState{approved: map[string]bool{}, sent: map[string]bool{}, jobs: map[string]*jobState{}}
		d.targets[t] = s
	}
	return s
}
func (d *Dispatcher) event(t release.Target, sha, decision, payload, result, origin string) {
	d.events = append(d.events, Event{d.options.Now(), t, sha, decision, payload, result, origin})
}

// Events drains bounded-by-activity audit records for the application log.
func (d *Dispatcher) Events() []Event {
	d.mu.Lock()
	defer d.mu.Unlock()
	e := d.events
	d.events = nil
	return e
}

// ObserveIdentity synchronously revokes authority on head changes and opt-out labels.
func (d *Dispatcher) ObserveIdentity(i Identity) { d.mu.Lock(); defer d.mu.Unlock(); d.observe(i) }
func (d *Dispatcher) observe(i Identity) {
	s := d.state(i.Target)
	if s.identity.SHA != i.SHA || s.identity.Base != i.Base || i.OptOut || !i.Open {
		if s.granted || len(s.approved) > 0 {
			d.event(i.Target, i.SHA, "revoked", "", "identity changed or write-disabled", "system")
		}
		s.granted = false
		clear(s.approved)
		s.epoch++
		if s.identity.SHA != i.SHA {
			clear(s.jobs)
			clear(s.sent)
			s.proposals = nil
		}
	}
	s.identity = i
}

// Grant requires enabled writes and the exact observed commit; Ignore and opt-out override it.
func (d *Dispatcher) Grant(t release.Target, sha string, allowed bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if allowed && !d.options.EnableWrites {
		return fmt.Errorf("writes disabled; restart with -enable-writes to allow retry comments")
	}
	s := d.state(t)
	if allowed && (d.stopped || !t.Valid() || sha == "" || s.identity.SHA != sha || !s.identity.Evidence.Fresh() || !s.identity.Open || s.identity.OptOut || s.ignored) {
		return fmt.Errorf("current target identity does not permit RW")
	}
	s.granted = allowed
	s.epoch++
	clear(s.approved)
	d.event(t, sha, "RW", "", fmt.Sprint(allowed), "user")
	return nil
}

// Ignore cancels grants and proposals immediately.
func (d *Dispatcher) Ignore(t release.Target, ignored bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.state(t)
	s.ignored = ignored
	if ignored {
		s.granted = false
		s.proposals = nil
	}
	s.epoch++
	clear(s.approved)
	d.event(t, s.identity.SHA, "ignore", "", fmt.Sprint(ignored), "user")
}

// Interactive sets confirmation mode without changing the PR label requirement.
func (d *Dispatcher) Interactive(on bool) { d.SetWriteMode(&on, nil) }

// SetWriteMode applies both settings together so automatic dispatch sees the new label policy.
func (d *Dispatcher) SetWriteMode(interactive, requirePRLabel *bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if (interactive == nil || d.interactive == *interactive) &&
		(requirePRLabel == nil || d.requirePRLabel == *requirePRLabel) {
		return
	}
	if interactive != nil && d.interactive != *interactive {
		d.interactive = *interactive
		d.event(release.Target{}, "", "interactive", "", fmt.Sprint(*interactive), "user")
	}
	if requirePRLabel != nil && d.requirePRLabel != *requirePRLabel {
		d.requirePRLabel = *requirePRLabel
		d.event(release.Target{}, "", "require-pr-label", "", fmt.Sprint(*requirePRLabel), "user")
	}
	for _, s := range d.targets {
		clear(s.approved)
		s.epoch++
	}
}

// WritesEnabled reports the immutable startup permission for grants and dispatch.
func (d *Dispatcher) WritesEnabled() bool { return d.options.EnableWrites }

// IsInteractive reports the current mode.
func (d *Dispatcher) IsInteractive() bool { d.mu.Lock(); defer d.mu.Unlock(); return d.interactive }

// RequiresPRLabel reports whether PR comments require the GitHub label.
func (d *Dispatcher) RequiresPRLabel() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.requirePRLabel
}

// Reset clears authority for a new configuration context.
func (d *Dispatcher) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, s := range d.targets {
		s.granted = false
		s.epoch++
	}
	d.targets = map[release.Target]*targetState{}
	d.interactive = true
}

// Stop prevents any new write admission during shutdown.
func (d *Dispatcher) Stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stopped = true
	for _, s := range d.targets {
		s.granted = false
		clear(s.approved)
		s.epoch++
	}
}

// ObservePlan uses distinct stable runs, never polls or comments, for failure counts.
func (d *Dispatcher) ObservePlan(p release.Plan) { d.mu.Lock(); defer d.mu.Unlock(); d.plan(p) }
func (d *Dispatcher) plan(p release.Plan) {
	s := d.state(p.Target)
	if s.ignored || s.identity.SHA == "" || s.identity.SHA != p.SHA {
		return
	}
	s.proposals = slices.Clone(p.Proposals)
	for _, r := range p.Runs {
		if r.ID == "" {
			continue
		}
		j := s.jobs[r.Job]
		if j == nil {
			j = &jobState{seen: map[string]bool{}}
			s.jobs[r.Job] = j
		}
		if r.Status == "completed" && r.Outcome == "success" {
			j.count = 0
			j.seen[r.ID] = true
			continue
		}
		if r.Eligible && !j.seen[r.ID] {
			j.count++
			j.seen[r.ID] = true
		}
	}
}

// ID binds approval to an exact immutable proposal.
func ID(p release.Proposal) string {
	return fmt.Sprintf("%x", sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%s\x00%s\x00%s", p.Target, p.SHA, p.Job, p.RunID, p.Payload)))
}
func (d *Dispatcher) offer(s *targetState, p release.Proposal) Offer {
	id := ID(p)
	o := Offer{ID: id, Proposal: p, Allowed: s.granted, CommentSent: s.sent[id]}
	j := s.jobs[p.Job]
	if j != nil {
		o.Failures = j.count
	}
	switch {
	case d.stopped:
		o.Reason = "server stopping"
	case !d.options.EnableWrites:
		o.Reason = "writes disabled; restart with -enable-writes to allow retry comments"
	case s.ignored:
		o.Reason = "ignored"
	case s.identity.OptOut:
		o.Reason = "no-release-assistant disables writes"
	case !s.identity.Evidence.Fresh() || s.identity.SHA != p.SHA || !s.identity.Open:
		o.Reason = "current target identity unknown or changed"
	case d.requirePRLabel && p.Target.PR > 0 && !s.identity.CommentLabel:
		o.Reason = "PR requires the " + release.PRCommentLabel + " label"
	case p.RunID == "":
		o.Reason = "stable run evidence unavailable; copy only"
	case !s.granted:
		o.Reason = "RW permission required"
	case o.CommentSent:
		o.Reason = "comment already sent for this run"
	}
	o.Queueable = o.Reason == ""
	if o.Queueable && s.inFlight {
		o.Reason = "comment dispatch in progress"
	}
	until := s.last.Add(d.options.Gap)
	if s.last.IsZero() {
		until = time.Time{}
	}
	if j != nil && j.count >= d.options.Threshold && !j.last.IsZero() {
		until = maxTime(until, j.last.Add(d.options.SlowGap))
	}
	if d.options.Now().Before(until) {
		o.WaitingUntil = until
		if o.Reason == "" {
			o.Reason = "waiting for comment rate limit"
		}
	}
	o.CanApprove = o.Reason == ""
	return o
}
func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// Views returns copied display state; callers cannot mutate authority.
func (d *Dispatcher) Views() []View {
	d.mu.Lock()
	defer d.mu.Unlock()
	v := make([]View, 0, len(d.targets))
	for t, s := range d.targets {
		x := View{Target: t, SHA: s.identity.SHA, Granted: s.granted, Ignored: s.ignored, OptOut: s.identity.OptOut, Message: s.message}
		for job, state := range s.jobs {
			if state.count >= d.options.Threshold {
				x.Warnings = append(x.Warnings, fmt.Sprintf("%s: %d distinct unsuccessful eligible runs; retry gap %s", job, state.count, d.options.SlowGap))
			}
		}
		slices.Sort(x.Warnings)
		for _, p := range s.proposals {
			x.Offers = append(x.Offers, d.offer(s, p))
		}
		v = append(v, x)
	}
	slices.SortFunc(v, func(a, b View) int {
		if a.Target.String() < b.Target.String() {
			return -1
		}
		if a.Target == b.Target {
			return 0
		}
		return 1
	})
	return v
}

// Send revalidates remote evidence, then admits exactly one attempt under local authority.
// Admission is the dispatch boundary: subsequent revocation cannot recall that request.
func (d *Dispatcher) Send(ctx context.Context, t release.Target, id string, approve bool) (Delivery, error) {
	d.mu.Lock()
	s := d.state(t)
	var proposal release.Proposal
	for _, p := range s.proposals {
		if ID(p) == id {
			proposal = p
			break
		}
	}
	if proposal.SHA == "" {
		d.mu.Unlock()
		return Delivery{}, fmt.Errorf("proposal is stale")
	}
	offer := d.offer(s, proposal)
	if !offer.CanApprove {
		d.mu.Unlock()
		return Delivery{}, fmt.Errorf("%s", offer.Reason)
	}
	if d.interactive && !approve {
		d.mu.Unlock()
		return Delivery{}, fmt.Errorf("single-use confirmation required")
	}
	if approve && !d.interactive {
		d.mu.Unlock()
		return Delivery{}, fmt.Errorf("interactive mode is no longer enabled")
	}
	epoch := s.epoch
	s.inFlight = true
	if d.interactive {
		s.approved[id] = true
		d.event(t, proposal.SHA, "approve", proposal.Payload, "single use", "user")
	}
	d.mu.Unlock()
	defer func() { d.mu.Lock(); s.inFlight = false; d.mu.Unlock() }()
	// Partial planner errors do not invalidate independently approved, identified jobs.
	fresh, _ := d.planner.Plan(ctx, t)
	identity, readErr := d.reader.ReadTarget(ctx, t)
	d.mu.Lock()
	if d.targets[t] != s || s.epoch != epoch {
		d.mu.Unlock()
		return Delivery{}, fmt.Errorf("permission changed; approval is stale")
	}
	if readErr != nil {
		identity = Identity{Target: t, SHA: s.identity.SHA}
		identity.Evidence.Error = "fresh authorization unavailable"
	}
	d.observe(identity)
	matches := readErr == nil && identity.Evidence.Fresh() && identity.Open && !identity.OptOut && identity.SHA == proposal.SHA && fresh.Target == t && fresh.SHA == proposal.SHA
	if matches {
		d.plan(fresh)
		matches = slices.ContainsFunc(fresh.Proposals, func(p release.Proposal) bool { return ID(p) == id && p.RunID != "" })
	}
	labelMissing := d.requirePRLabel && t.PR > 0 && !identity.CommentLabel
	if !matches || labelMissing || s.epoch != epoch || !s.granted || s.ignored || d.stopped || !d.options.EnableWrites || (d.interactive && !s.approved[id]) {
		clear(s.approved)
		s.message = "Approval became stale; refreshed evidence does not support dispatch"
		reason := "stale"
		if labelMissing {
			reason = "label-unverified"
			s.message = "GitHub did not verify the " + release.PRCommentLabel + " label for this PR"
		}
		d.event(t, proposal.SHA, "withheld", proposal.Payload, reason, "system")
		message := s.message
		d.mu.Unlock()
		return Delivery{}, fmt.Errorf("%s", message)
	}
	delete(s.approved, id)
	now := d.options.Now()
	s.last = now
	j := s.jobs[proposal.Job]
	if j == nil {
		j = &jobState{seen: map[string]bool{}}
		s.jobs[proposal.Job] = j
	}
	j.last = now
	d.event(t, proposal.SHA, "dispatch", proposal.Payload, "attempt", "system")
	d.mu.Unlock()
	result, err := d.writer.Write(ctx, proposal)
	d.mu.Lock()
	defer d.mu.Unlock()
	if !slices.Contains([]string{"sent", "failed", "uncertain"}, result.Status) {
		result.Status = "uncertain"
	}
	if result.Status == "sent" {
		s.sent[id] = true
	}
	s.message = "Delivery: " + result.Status
	if result.URL != "" {
		s.message += " " + result.URL
	}
	d.event(t, proposal.SHA, "delivery", proposal.Payload, result.Status, "system")
	return result, err
}
