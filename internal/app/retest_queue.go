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
		Sending                 bool
		key                     retestKey
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
	retestTarget struct{ sha, title, branch string }
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
				targets[pr.Target] = retestTarget{pr.SHA, pr.Title, pr.Base}
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
	add := func(target release.Target, sha, kind, evidence, payload, job, url, blocked string) {
		meta, ok := targets[target]
		if !ok || meta.sha != sha {
			return
		}
		key := retestKey{target, sha, kind, evidence}
		pending[key] = RetestItem{Target: target, Title: meta.title, Branch: meta.branch, Kind: kind,
			Payload: payload, Job: job, URL: url, Blocked: blocked, key: key}
	}
	rc := s.repos["acs"].Candidate.Target
	if s.dispatcher.IsInteractive() {
		for _, view := range views {
			plan := s.plans[view.Target]
			if plan.SHA != view.SHA || (view.Target == rc && !s.rcRetriesReady()) {
				continue
			}
			for _, offer := range view.Offers {
				if !offer.Queueable {
					continue
				}
				p := offer.Proposal
				var jobURL string
				if i := slices.IndexFunc(plan.Runs, func(run release.Run) bool { return run.ID == p.RunID && run.Job == p.Job }); i >= 0 {
					jobURL = plan.Runs[i].URL
				}
				add(p.Target, p.SHA, "konflux", offer.ID, p.Payload, p.Job, jobURL, offer.Reason)
			}
		}
	}
	for _, report := range reports {
		for _, job := range report.Jobs {
			if strings.EqualFold(job.WorkflowName, "Test Konflux builds") {
				continue
			}
			name := job.Name
			if job.WorkflowName != "" {
				name = job.WorkflowName + " / " + name
			}
			add(report.Target, report.SHA, "actions", fmt.Sprint(job.ID), job.RerunCommand(report.Target.Repo), name, job.URL, "")
		}
		for _, job := range report.Prow {
			add(report.Target, report.SHA, "prow", job.Context+"\x00"+job.URL+"\x00"+job.Command, job.Command, job.Context, job.URL, "")
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
	if item.Kind != "konflux" || item.Blocked != "" {
		s.mu.Unlock()
		return fmt.Errorf("queue item %d cannot be sent now", a.QueueID)
	}
	s.retestQueue[index].Sending = true
	s.mu.Unlock()

	delivery, err := s.dispatcher.Send(ctx, item.Target, item.key.evidence, true)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drain()
	index = slices.IndexFunc(s.retestQueue, func(row RetestItem) bool { return row.ID == item.ID })
	if delivery.Status == "sent" {
		if index >= 0 {
			s.retestQueue = slices.Delete(s.retestQueue, index, index+1)
		}
		s.recordRetest(item, "Sent", "GitHub confirmed the comment.", delivery.URL)
		return nil
	}
	if index >= 0 {
		s.retestQueue[index].Sending = false
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
	s.recordRetest(item, result, detail, "")
	return fmt.Errorf("queue item %d: %s", item.ID, detail)
}

func (s *Session) recordRetest(item RetestItem, result, detail, url string) {
	s.retestLog = append(s.retestLog, RetestDecision{
		At: time.Now(), ID: item.ID, Target: item.Target, Payload: item.Payload, Result: result, Detail: detail, URL: url,
	})
}
