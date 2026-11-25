package app

import (
	"slices"
	"testing"
	"time"

	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

func queueID(t *testing.T, s *Session, proposal release.Proposal) uint64 {
	t.Helper()
	for _, item := range s.Snapshot().RetestQueue {
		if item.key.evidence == dispatch.ID(proposal) {
			return item.ID
		}
	}
	t.Fatalf("no queued retry for %s", proposal.Payload)
	return 0
}

func TestRetestQueueSkipAndDelivery(t *testing.T) {
	s, f, config := newPinningSession(t)
	proposal := f.plan.Proposals[0]
	other := proposal
	other.Job, other.RunID, other.Payload = "scanner", "18", "/konflux-retest scanner"
	f.plan.Proposals = append(f.plan.Proposals, other)
	if err := s.Refresh(t.Context(), s.Snapshot().Generation, config.ACS(), true); err != nil {
		t.Fatal(err)
	}
	pending := s.Snapshot().RetestQueue
	if len(pending) != 2 || pending[0].Blocked != "" || pending[1].Blocked != "" || f.writes != 0 {
		t.Fatalf("eligible interactive retries should be available for confirmation: %+v", pending)
	}
	job := release.ManualJob{ID: 91, Name: "integration", URL: "https://github.com/example/actions/runs/8/job/91"}
	s.manualRetries[proposal.Target] = release.ManualRetryReport{
		Target: proposal.Target, SHA: proposal.SHA, Jobs: []release.ManualJob{job},
	}
	before := s.Snapshot()
	if len(before.RetestQueue) != 3 {
		t.Fatalf("queue has %d items, want 3", len(before.RetestQueue))
	}
	first := before.RetestQueue[0]
	apply(t, s, Action{Kind: "queue-skip", QueueID: first.ID})
	skipped := s.Snapshot()
	if got := skipped.RetestQueue[2].ID; got != first.ID {
		t.Fatalf("skip moved ID %d to position with ID %d", first.ID, got)
	}
	slices.Reverse(f.plan.Proposals)
	if err := s.Refresh(t.Context(), skipped.Generation, config.ACS(), true); err != nil {
		t.Fatal(err)
	}
	refreshed := s.Snapshot()
	if !slices.Equal(refreshed.RetestQueue, skipped.RetestQueue) {
		t.Fatalf("poll reordered or duplicated rows: %+v", refreshed.RetestQueue)
	}
	for _, row := range refreshed.RetestQueue {
		if row.Kind == "actions" {
			if err := s.Apply(t.Context(), Action{Kind: "queue-send", QueueID: row.ID, Generation: refreshed.Generation}); err == nil || f.writes != 0 {
				t.Fatal("a manual command reached the writer")
			}
		}
	}
	id := queueID(t, s, proposal)
	apply(t, s, Action{Kind: "queue-send", QueueID: id})
	after := s.Snapshot()
	if f.writes != 1 || len(after.RetestQueue) != 2 ||
		slices.ContainsFunc(after.RetestQueue, func(item RetestItem) bool { return item.ID == id }) {
		t.Fatalf("confirmed delivery did not consume ID %d: %+v", id, after.RetestQueue)
	}
	if log := after.RetestLog; len(log) != 2 || log[0].ID != first.ID || log[0].Result != "Skipped" ||
		log[1].ID != id || log[1].Result != "Sent" || log[1].Payload != proposal.Payload {
		t.Fatalf("log did not record exact decisions: %+v", log)
	}
	if err := s.Apply(t.Context(), Action{Kind: "queue-send", QueueID: id, Generation: after.Generation}); err == nil || f.writes != 1 {
		t.Fatal("a completed queue ID could be replayed")
	}
}

func TestRetestQueueWithholdsAWithdrawnProposal(t *testing.T) {
	s, f, config := newPinningSession(t)
	proposal := f.plan.Proposals[0]
	if err := s.Refresh(t.Context(), s.Snapshot().Generation, config.ACS(), true); err != nil {
		t.Fatal(err)
	}
	apply(t, s, Action{Kind: "grant", Target: proposal.Target, SHA: proposal.SHA, Enabled: true})
	id := queueID(t, s, proposal)
	f.plan.Proposals = nil
	if err := s.Apply(t.Context(), Action{Kind: "queue-send", QueueID: id, Generation: s.Snapshot().Generation}); err == nil || f.writes != 0 {
		t.Fatal("a withdrawn proposal reached the writer")
	}
	after := s.Snapshot()
	if len(after.RetestQueue) != 0 || !slices.ContainsFunc(after.RetestLog, func(entry RetestDecision) bool {
		return entry.ID == id && entry.Result == "Withheld" && entry.Payload == proposal.Payload
	}) {
		t.Fatalf("queue did not show the withheld delivery: %+v", after.RetestLog)
	}
}

func TestRetestQueuePriorityAndTargetScope(t *testing.T) {
	s, _ := newFixture()
	config := configuration()
	apply(t, s, Action{Kind: "configure", Config: config})
	acs := config.ACS()
	fact, collector := config.Components[0], config.Components[1]
	if fact.Repo < collector.Repo {
		fact, collector = collector, fact
	}
	current := release.Target{Repo: acs.Repo, Tag: config.Version + "-rc.2"}
	previous := release.Target{Repo: acs.Repo, Tag: config.Version + "-rc.1"}
	for _, source := range []release.Source{acs, fact, collector} {
		repo := release.Repository{Source: source}
		for _, number := range []int{20, 3, 7} {
			target := release.Target{Repo: source.Repo, PR: number}
			pr := release.PR{Target: target, SHA: "head", State: "open", Base: source.Branch, Evidence: observed()}
			s.choices.PRs[target] = release.Included
			if number == 7 {
				pr.Base = "another-branch"
			}
			repo.PRs = append(repo.PRs, pr)
			s.manualRetries[target] = release.ManualRetryReport{
				Target: target, SHA: "head", Jobs: []release.ManualJob{{ID: 8, Name: "legitimate"}, {ID: 9, WorkflowName: "Test Konflux builds", Name: "wait-for-images"}},
			}
		}
		if source.Name == "acs" {
			repo.Candidate = release.Candidate{Target: current, SHA: "tag", Evidence: observed()}
			for _, target := range []release.Target{current, previous} {
				s.manualRetries[target] = release.ManualRetryReport{Target: target, SHA: "tag", Jobs: []release.ManualJob{{ID: 9}}}
			}
		} else {
			target := release.Target{Repo: source.Repo, Tag: "3.25.7"}
			s.choices.Tags[source.Name], s.confirmedSHAs[source.Name] = target.Tag, "tag"
			repo.Tags = []release.Tag{{Name: target.Tag, SHA: "tag", Evidence: observed()}}
			s.manualRetries[target] = release.ManualRetryReport{Target: target, SHA: "tag", Jobs: []release.ManualJob{{ID: 9}}}
		}
		s.repos[source.Name] = repo
	}
	want := []release.Target{
		current, {Repo: acs.Repo, PR: 3}, {Repo: acs.Repo, PR: 20},
		{Repo: collector.Repo, Tag: "3.25.7"}, {Repo: fact.Repo, Tag: "3.25.7"},
		{Repo: collector.Repo, PR: 3}, {Repo: collector.Repo, PR: 20}, {Repo: fact.Repo, PR: 3}, {Repo: fact.Repo, PR: 20},
	}
	var got []release.Target
	for _, item := range s.Snapshot().RetestQueue {
		got = append(got, item.Target)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("queue order and scope = %v, want %v", got, want)
	}
}

func TestRetestQueueKeepsIncludedComponentRetriesWhenPRReadFails(t *testing.T) {
	for _, mode := range []string{"read-only", "interactive", "automatic"} {
		t.Run(mode, func(t *testing.T) {
			s, f := newFixture()
			writer := &writeCounter{}
			s.dispatcher = dispatch.New(f, f, writer, dispatch.Options{EnableWrites: mode != "read-only"})
			s.dispatcher.Interactive(mode != "automatic")
			source := configuration().Components[1]
			repo := release.Repository{Source: source}
			for _, n := range []int{21, 22} {
				target := release.Target{Repo: source.Repo, PR: n}
				evidence := observed()
				evidence.Known = false
				evidence.Error = "GitHub unavailable"
				pr := release.PR{Target: target, SHA: "head", Base: source.Branch, State: "open", Evidence: evidence}
				repo.PRs = append(repo.PRs, pr)
				s.choices.PRs[target] = release.Included
				proposal := release.Proposal{Target: target, SHA: pr.SHA, Job: "build", RunID: "5", Payload: "/konflux-retest build"}
				plan := release.Plan{Target: target, SHA: pr.SHA, Evidence: observed(), Proposals: []release.Proposal{proposal}}
				s.plans[target] = plan
				s.dispatcher.ObserveIdentity(dispatch.Identity{Target: target, SHA: pr.SHA, Base: pr.Base, Open: true, Evidence: evidence})
				s.dispatcher.ObservePlan(plan)
				s.manualRetries[target] = release.ManualRetryReport{
					Target: target, SHA: pr.SHA, At: time.Now(), Prow: []release.ProwRetry{{Context: "ci/prow/unit", Command: "/test unit"}},
				}
			}
			s.repos[source.Name] = repo
			snapshot := s.Snapshot()
			if len(snapshot.RetestQueue) != 4 {
				t.Fatalf("want both retries for both PRs, got %+v", snapshot.RetestQueue)
			}
			for _, item := range snapshot.RetestQueue {
				if item.Kind == "konflux" {
					if item.Blocked == "" {
						t.Fatalf("PR #%d must have a reason that posting is blocked", item.Target.PR)
					}
					if err := s.Apply(t.Context(), Action{Kind: "queue-send", QueueID: item.ID, Generation: snapshot.Generation}); err == nil || writer.calls != 0 {
						t.Fatalf("PR #%d reached the writer without verified identity", item.Target.PR)
					}
				}
			}
			repo.PRs[0].SHA = "new-head"
			s.repos[source.Name] = repo
			queue := s.Snapshot().RetestQueue
			if len(queue) != 2 || queue[0].Target.PR != 22 || queue[1].Target.PR != 22 {
				t.Fatalf("head change should only remove retries belonging to the old revision: %+v", queue)
			}
		})
	}
}
