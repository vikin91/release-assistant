package app

import (
	"context"
	"errors"
	"slices"
	"time"

	"release-assistant/internal/dispatch"
	"release-assistant/internal/githubread"
	"release-assistant/internal/release"
)

// refreshChangedPRHead verifies a planner-detected head without advancing the poll schedules.
func (s *Session) refreshChangedPRHead(ctx context.Context, generation, revision uint64, source release.Source, target release.Target, sha string) {
	s.mu.Lock()
	current := func() bool {
		choice := s.choices.PRs[target]
		return !canceled(ctx) && generation == s.generation && revision == s.workRevision && choice != release.Excluded && choice != release.Ignored
	}
	prs := s.repos[source.Name].PRs
	i := slices.IndexFunc(prs, func(pr release.PR) bool { return pr.Target == target && pr.State == "open" })
	if !current() || i < 0 {
		s.mu.Unlock()
		return
	}
	pending := prs[i]
	if pending.SHA == sha && pending.Evidence.Fresh() {
		delete(s.prRefreshHeads, target)
	}
	if pending.SHA == sha || s.prRefreshHeads[target] == sha {
		s.mu.Unlock()
		return
	}
	s.prRefreshHeads[target] = sha
	pending.SHA = sha
	pending.Evidence = release.Evidence{At: time.Now(), Error: "PR head changed; awaiting GitHub verification"}
	delete(s.plans, target)
	s.observePRHead(source, pending)
	s.mu.Unlock()

	reader, ok := s.discovery.(PRReader)
	if !ok {
		return
	}
	pr, err := reader.ReadPR(githubread.WithPriority(ctx, githubread.PRs, source.Repo), target)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !current() || !slices.ContainsFunc(s.repos[source.Name].PRs, func(pr release.PR) bool {
		return pr.Target == target && pr.SHA == sha && pr.Evidence == pending.Evidence
	}) {
		return
	}
	if err == nil && (pr.Target != target || pr.SHA == "" || !pr.Evidence.Fresh()) {
		err = errors.New("response did not verify the requested PR head")
	}
	if err != nil {
		pending.Evidence.Error = "Could not verify the new PR head on GitHub"
		s.observePRHead(source, pending)
		s.recordReadResult(target, "pr-head", err.Error(), "the new PR head for "+consoleTarget(target))
		return
	}
	s.recordReadResult(target, "pr-head", "", "the new PR head for "+consoleTarget(target))
	s.observePRHead(source, pr)
}

// observePRHead keeps PR and pinning evidence aligned under the session lock.
func (s *Session) observePRHead(source release.Source, pr release.PR) {
	old := s.repos[source.Name]
	request := DiscoveryRequest{Source: source, Tracked: []release.Target{pr.Target}, Scope: ReadTargets}
	repo := mergeDiscovery(old, release.Repository{Source: source, Head: old.Head, Evidence: old.Evidence, PRs: []release.PR{pr}}, request)
	s.recordDiscoveryChanges(old, repo, request)
	s.repos[source.Name] = repo
	s.dispatcher.ObserveIdentity(dispatch.Identity{Target: pr.Target, SHA: pr.SHA, Base: pr.Base, Open: pr.State == "open" && pr.Base == source.Branch, OptOut: pr.OptOut, CommentLabel: pr.CommentLabel, Evidence: pr.Evidence})
	s.revalidateRCPlan()
	s.drain()
}
