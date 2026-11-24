package app

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

type (
	// RetestItem identifies one pending retry for the lifetime of the queue item.
	RetestItem struct {
		ID                      uint64
		Target                  release.Target
		Title, Branch           string
		Kind, Payload, Job, URL string
		Blocked                 string
		ObservedAt              time.Time
		// MissingBuild marks a planner-verified start for a build with no observed run.
		MissingBuild bool
		Sending      bool
		key          retestKey
	}
	// RetestDecision records a queue action or the withdrawal of a pending item.
	RetestDecision struct {
		At                           time.Time
		ID                           uint64
		Target                       release.Target
		Payload, Result, Detail, URL string
	}
	retestKey struct {
		target              release.Target
		sha, kind, evidence string
	}
	retestTarget struct {
		sha, title, branch string
		noCIFailuresAt     time.Time
	}
)

// retestTargets limits the queue to verified release targets. The caller holds s.mu.
func (s *Session) retestTargets() map[release.Target]retestTarget {
	targets := make(map[release.Target]retestTarget)
	for _, repo := range s.repos {
		if target, sha := s.tagCheckTarget(repo); sha != "" {
			targets[target] = retestTarget{sha: sha}
		}
		addPR := func(pr release.PR) {
			if s.choices.PRs[pr.Target] == release.Included && pr.State == "open" && !pr.Merged &&
				pr.Base == repo.Source.Branch && pr.Evidence.Fresh() && pr.SHA != "" {
				target := retestTarget{sha: pr.SHA, title: pr.Title, branch: pr.Base}
				if checksWithoutFailures(pr) {
					target.noCIFailuresAt = pr.CheckResultsEvidence.At
				}
				targets[pr.Target] = target
			}
		}
		for _, pr := range repo.PRs {
			addPR(pr)
		}
		if pr := repo.PinningPR; pr != nil && pr.Target == s.config.PinningTarget() {
			delete(targets, pr.Target)
			addPR(*pr)
		}
	}
	return targets
}

// syncRetestQueue preserves order across polls and assigns IDs only to new requests.
// The caller holds s.mu.
func (s *Session) syncRetestQueue(views []dispatch.View, reports []release.ManualRetryReport) {
	targets := s.retestTargets()
	pending := make(map[retestKey]RetestItem)
	add := func(sha, evidence string, item RetestItem) {
		meta, ok := targets[item.Target]
		if !ok || meta.sha != sha {
			return
		}
		item.Title, item.Branch = meta.title, meta.branch
		item.key = retestKey{item.Target, sha, item.Kind, evidence}
		pending[item.key] = item
	}
	authority := make(map[release.Target]dispatch.View, len(views))
	for _, view := range views {
		authority[view.Target] = view
	}
	ci := make(map[release.Target]release.ManualRetryReport, len(reports))
	for _, report := range reports {
		ci[report.Target] = report
	}
	rc := s.repos["acs"].Candidate.Target
	rcReady, writes := s.rcRetriesReady(), s.dispatcher.WritesEnabled()
	for target, meta := range targets {
		plan, view, report := s.plans[target], authority[target], ci[target]
		offers := make(map[string]dispatch.Offer, len(view.Offers))
		for _, offer := range view.Offers {
			offers[offer.ID] = offer
		}
		if check := s.prRetryChecks[target]; check.SHA == meta.sha {
			if check.Konflux.Evidence.At.After(plan.Evidence.At) && check.Konflux.Evidence.At.After(view.PlanAt) {
				plan = check.Konflux
			}
			if check.CI.At.After(report.At) {
				report = check.CI
			}
		}
		// A newer dispatch read may withdraw a scheduled proposal before the next poll.
		proposals := plan.Proposals
		observedAt := plan.Evidence.At
		if view.SHA == meta.sha && !plan.Evidence.At.After(view.PlanAt) {
			proposals = nil
			observedAt = view.PlanAt
			for _, offer := range view.Offers {
				proposals = append(proposals, offer.Proposal)
			}
		}
		for _, proposal := range proposals {
			if proposal.Target != target || proposal.SHA != meta.sha {
				continue
			}
			id := dispatch.ID(proposal)
			offer, observed := offers[id]
			if offer.CommentSent {
				continue
			}
			blocked := offer.Reason
			switch {
			case !writes && blocked == "":
				blocked = "Writes are disabled."
			case target == rc && !rcReady:
				blocked = "Waiting for verified release component pins before posting."
			case !observed:
				blocked = "Waiting for the posting eligibility check."
			}
			var jobURL string
			if i := slices.IndexFunc(plan.Runs, func(run release.Run) bool {
				return run.ID == proposal.RunID && run.Job == proposal.Job
			}); i >= 0 {
				jobURL = plan.Runs[i].URL
			}
			add(proposal.SHA, id, RetestItem{
				Target: target, Kind: "konflux", Payload: proposal.Payload, Job: proposal.Job,
				URL: jobURL, Blocked: blocked, ObservedAt: observedAt, MissingBuild: proposal.StartsMissingPRBuild(),
			})
		}
		if report.Target != target || report.Pending || meta.noCIFailuresAt.After(report.At) {
			continue
		}
		for _, job := range report.Jobs {
			if strings.EqualFold(job.WorkflowName, "Test Konflux builds") {
				continue
			}
			name := job.Name
			if job.WorkflowName != "" {
				name = job.WorkflowName + " / " + name
			}
			add(report.SHA, fmt.Sprint(job.ID), RetestItem{
				Target: report.Target, Kind: "actions", Payload: job.RerunCommand(report.Target.Repo), Job: name, URL: job.URL, ObservedAt: report.At,
			})
		}
		for _, job := range report.Prow {
			add(report.SHA, job.Context+"\x00"+job.URL+"\x00"+job.Command, RetestItem{
				Target: report.Target, Kind: "prow", Payload: job.Command, Job: job.Context, URL: job.URL, ObservedAt: report.At,
			})
		}
	}
	kept := s.retestQueue[:0]
	for _, old := range s.retestQueue {
		if item, ok := pending[old.key]; ok {
			item.ID, item.Sending = old.ID, old.Sending
			kept = append(kept, item)
			delete(pending, old.key)
		} else if old.Sending {
			kept = append(kept, old)
		} else {
			s.recordRetest(old, "Expired", "The request is no longer eligible or in scope.", "")
		}
	}
	clear(s.retestQueue[len(kept):])
	s.retestQueue = kept
	var added []RetestItem
	for _, item := range pending {
		added = append(added, item)
	}
	slices.SortFunc(added, func(a, b RetestItem) int {
		return cmp.Or(s.compareRetestTargets(a.Target, b.Target), cmp.Compare(a.Job, b.Job),
			cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.key.evidence, b.key.evidence))
	})
	for _, item := range added {
		s.nextRetestID++
		item.ID = s.nextRetestID
		s.retestQueue = append(s.retestQueue, item)
	}
	slices.SortStableFunc(s.retestQueue, func(a, b RetestItem) int {
		return s.compareRetestTargets(a.Target, b.Target)
	})
}

func (s *Session) compareRetestTargets(a, b release.Target) int {
	acs := s.config.ACS().Repo
	priority := func(target release.Target) int {
		n := 0
		if target.Repo != acs {
			n += 2
		}
		if target.PR > 0 {
			n++
		}
		return n
	}
	return cmp.Or(cmp.Compare(priority(a), priority(b)), cmp.Compare(a.Repo, b.Repo),
		cmp.Compare(a.PR, b.PR), cmp.Compare(a.Tag, b.Tag))
}

// MatchesProposal reports whether the queue item represents this exact Konflux retry.
func (item RetestItem) MatchesProposal(proposal release.Proposal) bool {
	return item.Kind == "konflux" && item.Target == proposal.Target && item.key.evidence == dispatch.ID(proposal)
}

// applyRetestDecision consumes the ID of the displayed item. It releases s.mu
// before dispatch so fresh GitHub reads never block queue rendering.
func (s *Session) applyRetestDecision(ctx context.Context, a Action) error {
	var reports []release.ManualRetryReport
	for _, repo := range s.repos {
		reports = append(reports, s.currentManualRetries(repo)...)
	}
	s.syncRetestQueue(s.dispatcher.Views(), reports)
	index := slices.IndexFunc(s.retestQueue, func(item RetestItem) bool { return item.ID == a.QueueID })
	if index < 0 {
		s.mu.Unlock()
		return fmt.Errorf("queue item %d is no longer pending", a.QueueID)
	}
	item := s.retestQueue[index]
	if item.Sending {
		s.mu.Unlock()
		return fmt.Errorf("queue item %d is already sending", a.QueueID)
	}
	if a.Kind == "queue-skip" {
		s.retestQueue = append(slices.Delete(s.retestQueue, index, index+1), item)
		s.recordRetest(item, "Skipped", "Moved to the bottom of this target’s queue.", "")
		s.mu.Unlock()
		return nil
	}
	if item.Blocked != "" {
		s.mu.Unlock()
		return fmt.Errorf("queue item %d: %s", item.ID, item.Blocked)
	}
	if item.Kind != "konflux" || !s.dispatcher.IsInteractive() {
		s.mu.Unlock()
		return fmt.Errorf("queue item %d cannot be sent now", a.QueueID)
	}
	s.retestQueue[index].Sending = true
	work := s.releaseWork
	ctx = s.dispatcher.WithScope(ctx)
	s.mu.Unlock()

	delivery, err := s.dispatcher.Send(ctx, item.Target, item.key.evidence, true)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drain()
	index = slices.IndexFunc(work.retestQueue, func(row RetestItem) bool { return row.ID == item.ID })
	if delivery.Status == "sent" {
		if index >= 0 {
			work.retestQueue = slices.Delete(work.retestQueue, index, index+1)
		}
		work.recordRetest(item, "Sent", "GitHub confirmed the comment.", delivery.URL)
		return nil
	}
	if index >= 0 {
		work.retestQueue[index].Sending = false
	}
	result, detail := "Failed", "GitHub did not accept the comment."
	switch delivery.Status {
	case "":
		result, detail = "Withheld", "The request could not be authorized."
	case "uncertain":
		result, detail = "Uncertain", "GitHub did not confirm delivery."
	}
	if err != nil {
		detail = err.Error()
	}
	work.recordRetest(item, result, detail, "")
	return fmt.Errorf("queue item %d: %s", item.ID, detail)
}

func (s *releaseWork) recordRetest(item RetestItem, result, detail, url string) {
	s.retestLog = append(s.retestLog, RetestDecision{
		At: time.Now(), ID: item.ID, Target: item.Target, Payload: item.Payload, Result: result, Detail: detail, URL: url,
	})
}
