package dispatch

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"release-assistant/internal/apilog"
	"release-assistant/internal/release"
)

type (
	// Options configures startup write permission, attempt limits, and time for tests.
	Options struct {
		// EnableWrites allows grants and dispatch for this process; the default is read-only.
		EnableWrites bool
		// PinningWriter is available only to the explicit pinning action.
		PinningWriter PinningWriter
		Gap, SlowGap  time.Duration
		Threshold     int
		Now           func() time.Time
	}
	// Event records a permission decision or one delivery attempt without credentials.
	Event struct {
		At                                     time.Time
		Release                                string
		Target                                 release.Target
		SHA, Decision, Payload, Result, Origin string
	}
	// Offer is a proposal plus the current dispatcher-owned authority and cooldown.
	Offer struct {
		ID                  string
		Proposal            release.Proposal
		Allowed, CanApprove bool
		// Queueable reports whether the offer is still pending, including when posting is blocked.
		Queueable bool
		Failures  int
		// CommentSent reports successful delivery of this exact proposal.
		CommentSent  bool
		WaitingUntil time.Time
		Reason       string
	}
	// View exposes authority separately from release inclusion.
	View struct {
		Target release.Target
		SHA    string
		// PlanAt is the observation time of the proposal list.
		PlanAt                   time.Time
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
		pinningInFlight                      bool
		options                              Options
		targets                              map[release.Target]*targetState
		attempts                             map[commentKey]commentAttempt
		interactive, requirePRLabel, stopped bool
		events                               []Event
		suspended                            map[string]map[release.Target]*targetState
		releaseID, version                   string
		scope                                uint64
	}
	scopeKey    struct{}
	targetState struct {
		identity         Identity
		planAt           time.Time
		granted, ignored bool
		epoch            uint64
		proposals        []release.Proposal
		approved, sent   map[string]bool
		jobs             map[string]*jobState
		message          string
	}
	commentKey struct {
		target    release.Target
		sha, body string
	}
	commentAttempt struct {
		inFlight bool
		last     time.Time
	}
	jobState struct {
		seen  map[string]bool
		count int
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
	return &Dispatcher{reader: r, planner: p, writer: w, options: o,
		targets: map[release.Target]*targetState{}, attempts: map[commentKey]commentAttempt{}, interactive: true}
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
	d.logEvent(d.version, t, sha, decision, payload, result, origin)
}

func (d *Dispatcher) logEvent(version string, t release.Target, sha, decision, payload, result, origin string) {
	d.events = append(d.events, Event{d.options.Now(), version, t, sha, decision, payload, result, origin})
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
			s.planAt = time.Time{}
			clear(s.jobs)
			clear(s.sent)
			s.proposals = nil
		}
	}
	s.identity = i
}

// Grant sets automatic posting permission for the exact observed commit.
// Automatic posting still requires enabled writes and current eligibility.
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

// WithScope binds a send to the release selection that offered it.
func (d *Dispatcher) WithScope(ctx context.Context) context.Context {
	d.mu.Lock()
	defer d.mu.Unlock()
	return context.WithValue(ctx, scopeKey{}, d.scope)
}

// SelectRelease suspends the previous release's evidence and clears its posting authority.
func (d *Dispatcher) SelectRelease(id, version string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.suspended == nil {
		d.suspended = make(map[string]map[release.Target]*targetState)
	}
	d.revokeRelease()
	if d.releaseID != "" {
		d.suspended[d.releaseID] = d.targets
	}
	d.targets = d.suspended[id]
	delete(d.suspended, id)
	if d.targets == nil {
		d.targets = make(map[release.Target]*targetState)
	}
	d.releaseID, d.version = id, version
}

// ForgetRelease discards the suspended evidence of a removed release.
func (d *Dispatcher) ForgetRelease(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.suspended, id)
}

func (d *Dispatcher) revokeRelease() {
	for _, state := range d.targets {
		state.granted = false
		clear(state.approved)
		state.epoch++
	}
	d.scope++
	d.interactive = true
}

// Reset clears authority for a new configuration context.
func (d *Dispatcher) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.revokeRelease()
	d.targets = map[release.Target]*targetState{}
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

// ObservePlan counts distinct eligible attempts using their stable evidence identities.
func (d *Dispatcher) ObservePlan(p release.Plan) { d.mu.Lock(); defer d.mu.Unlock(); d.plan(p) }
func (d *Dispatcher) plan(p release.Plan) {
	s := d.state(p.Target)
	if s.ignored || s.identity.SHA == "" || s.identity.SHA != p.SHA ||
		(!p.Evidence.At.IsZero() && p.Evidence.At.Before(s.planAt)) {
		return
	}
	s.proposals = slices.Clone(p.Proposals)
	s.planAt = p.Evidence.At
	for _, r := range p.Runs {
		if r.ID == "" {
			continue
		}
		j := s.job(r.Job)
		if r.Status == "completed" && r.Outcome == "success" {
			j.count = 0
			j.seen[r.ID] = true
			continue
		}
		if r.Eligible {
			j.recordFailure(r.ID)
		}
	}
	for _, proposal := range p.Proposals {
		if p.Target.PR > 0 && proposal.Target == p.Target && proposal.SHA == p.SHA &&
			strings.HasPrefix(proposal.RunID, release.PRCheckEvidencePrefix) {
			s.job(proposal.Job).recordFailure(proposal.RunID)
		}
	}
}

func (s *targetState) job(name string) *jobState {
	j := s.jobs[name]
	if j == nil {
		j = &jobState{seen: map[string]bool{}}
		s.jobs[name] = j
	}
	return j
}

func (j *jobState) recordFailure(id string) {
	if !j.seen[id] {
		j.count++
		j.seen[id] = true
	}
}

// ID binds approval to an exact immutable proposal.
func ID(p release.Proposal) string {
	return fmt.Sprintf("%x", sha256.Sum256(fmt.Appendf(nil,
		"%s\x00%s\x00%s\x00%s\x00%s", p.Target, p.SHA, p.Job, p.RunID, p.CommentBody(),
	)))
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
	case p.RunID == "" || (p.StartsMissingPRBuild() && (p.Target.PR <= 0 || !p.Target.Valid())):
		o.Reason = "stable retry evidence unavailable; copy only"
	case !d.interactive && !s.granted:
		o.Reason = "Automatic posting permission required"
	case o.CommentSent:
		o.Reason = "comment already sent for this run"
	}
	o.Queueable = !o.CommentSent
	attempt := d.attempts[commentKey{p.Target, p.SHA, p.CommentBody()}]
	if o.Reason == "" && attempt.inFlight {
		o.Reason = "comment dispatch in progress"
	}
	gap := d.options.Gap
	if j != nil && j.count >= d.options.Threshold {
		gap = max(gap, d.options.SlowGap)
	}
	if until := attempt.last.Add(gap); !attempt.last.IsZero() && d.options.Now().Before(until) {
		o.WaitingUntil = until
		if o.Reason == "" {
			o.Reason = "waiting for comment rate limit"
		}
	}
	o.CanApprove = o.Reason == ""
	return o
}

// Views returns copied display state; callers cannot mutate authority.
func (d *Dispatcher) Views() []View {
	d.mu.Lock()
	defer d.mu.Unlock()
	v := make([]View, 0, len(d.targets))
	for t, s := range d.targets {
		x := View{Target: t, SHA: s.identity.SHA, PlanAt: s.planAt, Granted: s.granted, Ignored: s.ignored, OptOut: s.identity.OptOut, Message: s.message}
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
	if scope, ok := ctx.Value(scopeKey{}).(uint64); ctx.Err() != nil || (ok && scope != d.scope) {
		d.mu.Unlock()
		return Delivery{}, fmt.Errorf("release selection changed; approval is stale")
	}
	version := d.version
	if version == "" {
		version = apilog.Release(ctx)
	}
	s := d.state(t)
	var proposal release.Proposal
	for _, p := range s.proposals {
		if p.Target == t && ID(p) == id {
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
	body := proposal.CommentBody()
	key := commentKey{t, proposal.SHA, body}
	attempt := d.attempts[key]
	attempt.inFlight = true
	d.attempts[key] = attempt
	if d.interactive {
		s.approved[id] = true
		d.event(t, proposal.SHA, "approve", body, "single use", "user")
	}
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		delete(s.approved, id)
		if attempt.last.IsZero() {
			delete(d.attempts, key)
		} else {
			attempt.inFlight = false
			d.attempts[key] = attempt
		}
	}()
	// Partial planner errors do not invalidate independently approved, identified jobs.
	fresh, _ := d.planner.Plan(ctx, t)
	identity, readErr := d.reader.ReadTarget(ctx, t)
	d.mu.Lock()
	if d.targets[t] != s || s.epoch != epoch {
		d.mu.Unlock()
		return Delivery{}, fmt.Errorf("permission changed; approval is stale")
	}
	verified := readErr == nil && identity.Target == t && identity.Evidence.Fresh() && ctx.Err() == nil
	if verified && !identity.Evidence.At.Before(s.identity.Evidence.At) {
		d.observe(identity)
	}
	matches := verified && identity.Open && !identity.OptOut && identity.SHA == proposal.SHA &&
		s.identity.Evidence.Fresh() && s.identity.Open && !s.identity.OptOut &&
		s.identity.SHA == identity.SHA && s.identity.Base == identity.Base &&
		fresh.Target == t && fresh.SHA == proposal.SHA
	if matches {
		if !fresh.Evidence.At.Before(s.planAt) {
			d.plan(fresh)
		}
		matchingProposal := func(p release.Proposal) bool { return ID(p) == id }
		matches = slices.ContainsFunc(fresh.Proposals, matchingProposal) && slices.ContainsFunc(s.proposals, matchingProposal)
	}
	labelMissing := d.requirePRLabel && t.PR > 0 && (!verified || !identity.CommentLabel || !s.identity.CommentLabel)
	authorized := (d.interactive && s.approved[id]) || (!d.interactive && s.granted)
	if !matches || labelMissing || s.epoch != epoch || !authorized || s.ignored || d.stopped || !d.options.EnableWrites {
		s.message = "Approval became stale; refreshed evidence does not support dispatch"
		reason := "stale"
		if labelMissing {
			reason = "label-unverified"
			s.message = "GitHub did not verify the " + release.PRCommentLabel + " label for this PR"
		}
		d.event(t, proposal.SHA, "withheld", body, reason, "system")
		message := s.message
		d.mu.Unlock()
		return Delivery{}, fmt.Errorf("%s", message)
	}
	delete(s.approved, id)
	attempt.last = d.options.Now()
	d.attempts[key] = attempt
	d.event(t, proposal.SHA, "dispatch", body, "attempt", "system")
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
	d.logEvent(version, t, proposal.SHA, "delivery", body, result.Status, "system")
	return result, err
}
