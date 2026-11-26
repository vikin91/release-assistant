package app

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"release-assistant/internal/githubread"
	"release-assistant/internal/release"
)

// monitorComponentTags runs manual discovery and exact-tag revalidation in repository order.
func (s *Session) monitorComponentTags(ctx context.Context, generation uint64, sources []release.Source) {
	var lastSource string
	for ctx.Err() == nil {
		source, job, revision, next, changed := s.nextComponentTagPoll(generation, sources, lastSource)
		if source.Repo != "" {
			lastSource = source.Name
			if job.kind == pollManualComponentTags {
				s.runComponentTagAction(ctx, generation, revision, source, job.identity)
			} else {
				s.runPoll(ctx, generation, revision, source, job)
			}
			continue
		}
		var timer *time.Timer
		var timerC <-chan time.Time
		if !next.IsZero() {
			timer = time.NewTimer(time.Until(next))
			timerC = timer.C
		}
		select {
		case <-ctx.Done():
		case <-changed:
		case <-timerC:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

func (s *Session) nextComponentTagPoll(generation uint64, sources []release.Source, lastSource string) (release.Source, pollJob, uint64, time.Time, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var selected release.Source
	var job pollJob
	var next time.Time
	if generation != s.generation {
		return selected, job, s.workRevision, next, s.pollChanged
	}
	now := time.Now()
	for _, source := range sources {
		action := s.componentTagActions[source.Name]
		if action.kind != "" && (selected.Repo == "" || job.kind != pollManualComponentTags ||
			turnBefore(source.Name, selected.Name, lastSource)) {
			selected, job = source, pollJob{kind: pollManualComponentTags, identity: action.kind}
			continue
		}
		candidate := s.tagPollJob(source)
		if candidate.identity == "" {
			continue
		}
		record := s.polls[candidate.id(source)]
		at := record.next(s.options.RefreshIntervals[candidate.setting], s.startupUntil)
		if record.due(candidate.identity, now, at) {
			if selected.Repo == "" || (job.kind != pollManualComponentTags && turnBefore(source.Name, selected.Name, lastSource)) {
				selected, job = source, candidate
			}
		} else if next.IsZero() || at.Before(next) {
			next = at
		}
	}
	if selected.Repo != "" {
		if resume := s.githubReadResumeAt(); resume.After(now) {
			return release.Source{}, pollJob{}, s.workRevision, resume, s.pollChanged
		}
	}
	return selected, job, s.workRevision, next, s.pollChanged
}

// turnBefore orders ready sources after the last turn, wrapping at the end.
func turnBefore(source, other, last string) bool {
	after, otherAfter := source > last, other > last
	if after != otherAfter {
		return after
	}
	return source < other
}

// ComponentTagNames records the versions found in one component's release stream.
type ComponentTagNames struct {
	Names    []string
	Evidence release.Evidence
}

func (s *Session) savedComponentTagNames(work *releaseWork, profileID, component string) ComponentTagNames {
	if names, ok := work.componentTagNames[component]; ok {
		return names
	}
	if saved := s.dashboards.Profiles[profileID]; sameDashboardConfig(saved.Config, work.config) {
		names := saved.TagNames[component]
		names.Evidence = historicalEvidence(names.Evidence, release.Evidence{})
		return names
	}
	return ComponentTagNames{}
}

type componentTagAction struct {
	kind, tag string
	revision  uint64
}

func (s *Session) requestComponentTagAction(action Action) error {
	index := slices.IndexFunc(s.config.Components, func(source release.Source) bool {
		return source.Name == action.Component
	})
	if index < 0 {
		return fmt.Errorf("unknown component")
	}
	if s.componentTagActions[action.Component].kind != "" {
		return fmt.Errorf("a tag read is already queued for this component")
	}
	switch action.Kind {
	case "fetch-tag-names":
		if _, available := s.discovery.(ComponentTagNameReader); !available {
			return fmt.Errorf("tag-name discovery is unavailable")
		}
		target := release.Target{Repo: s.config.Components[index].Repo}
		s.record(target, "", "tag-names-requested", "", "", "user", fmt.Sprintf("Fetching %s tag names.", action.Component))
	case "verify-recent-tags":
		if len(s.componentTagCandidates(action.Component)) == 0 {
			return fmt.Errorf("fetch tag names before verifying recent tags")
		}
	case "select-fetched-tag":
		if !componentStreamTag(s.config.Components[index], action.Tag) {
			return fmt.Errorf("tag must be a final version in the configured component stream")
		}
		if !slices.Contains(s.componentTagCandidates(action.Component), action.Tag) {
			return fmt.Errorf("choose a tag from the current list of fetched names")
		}
		delete(s.readErrors, "tag-selection "+release.Target{Repo: s.config.Components[index].Repo}.String())
	}
	s.componentTagActions[action.Component] = componentTagAction{kind: action.Kind, tag: action.Tag, revision: s.workRevision}
	s.wakePolls()
	return nil
}

func componentStreamTag(source release.Source, name string) bool {
	major, minor, _, final := release.Version(name)
	return final && fmt.Sprintf("%d.%d", major, minor) == source.Stream
}

func (s *Session) componentTagCandidates(component string) []string {
	names := s.savedComponentTagNames(s.releaseWork, s.activeProfile, component).Names
	current := s.choices.Tags[component]
	if current == "" {
		current = s.pendingTags[component].Tag
	}
	candidates := make([]string, 0, min(s.tagChoiceLimit, len(names)))
	for _, name := range names {
		if name != current {
			candidates = append(candidates, name)
			if len(candidates) == s.tagChoiceLimit {
				break
			}
		}
	}
	return candidates
}

func (s *Session) runComponentTagAction(ctx context.Context, generation, revision uint64, source release.Source, action string) {
	s.mu.Lock()
	pending := s.componentTagActions[source.Name]
	if generation != s.generation || revision != s.workRevision || pending.kind != action {
		s.mu.Unlock()
		return
	}
	work := s.releaseWork
	startingHead := s.repos[source.Name].Head
	catalog := s.savedComponentTagNames(work, s.activeProfile, source.Name)
	candidates := s.componentTagCandidates(source.Name)
	if pending.kind == "select-fetched-tag" {
		candidates = []string{pending.tag}
	}
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(githubread.WithPriority(ctx, githubread.Tags, source.Repo), s.options.Timeout)
	defer cancel()

	operation := "tag-names"
	description := "the tag names on " + consoleBranch(source)
	var names []string
	var verified release.Repository
	var err error
	if action == "fetch-tag-names" {
		ctx = githubread.WithWork(ctx, source.Repo, "Component tag names")
		names, err = s.discovery.(ComponentTagNameReader).ReadComponentTagNames(ctx, source)
	} else {
		operation = "tag-verification"
		description = "recent tags on " + consoleBranch(source)
		if pending.kind == "select-fetched-tag" {
			operation = "tag-selection"
			description = pending.tag + " on " + consoleBranch(source)
		}
		ctx = githubread.WithWork(ctx, source.Repo, "Component tag verification")
		if len(candidates) > 0 {
			verified, err = s.discovery.Discover(ctx, DiscoveryRequest{Source: source, Scope: ReadTags, TagCandidates: candidates})
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation != s.generation || work != s.releaseWork || canceled(ctx) || work.componentTagActions[source.Name] != pending {
		return
	}
	delete(work.componentTagActions, source.Name)
	if action != "fetch-tag-names" && len(candidates) > 0 {
		work.componentTagNames[source.Name] = catalog
		err = s.mergeRecentTagVerification(source, startingHead, candidates, verified, err)
		if err == nil && pending.kind == "select-fetched-tag" {
			err = s.selectVerifiedFetchedTag(source, pending.tag)
		}
	}
	message := ""
	if err != nil {
		message = err.Error()
	}
	s.recordReadResult(release.Target{Repo: source.Repo}, operation, message, description)
	if action != "fetch-tag-names" {
		return
	}
	observation := ComponentTagNames{Names: names, Evidence: release.Evidence{At: time.Now(), Known: err == nil, Error: message}}
	if err != nil {
		before := s.savedComponentTagNames(work, s.activeProfile, source.Name)
		if before.Evidence.Known {
			observation = ComponentTagNames{Names: before.Names, Evidence: historicalEvidence(before.Evidence, observation.Evidence)}
		}
	}
	before := work.componentTagNames[source.Name]
	work.componentTagNames[source.Name] = observation
	if err == nil {
		s.record(release.Target{Repo: source.Repo}, "", "tag-names-fetched", "", "", "system",
			fmt.Sprintf("Fetched %s for %s.", counted(len(names), "tag name"), source.Name))
	}
	if err != nil || !slices.Equal(before.Names, observation.Names) {
		if repo, ok := work.repos[source.Name]; ok {
			repo.TagsPending = true
			work.repos[source.Name] = repo
		}
	}
	if s.activeProfile != "" && err == nil {
		saved := s.dashboardForUpdate()
		saved.TagNames = maps.Clone(saved.TagNames)
		if saved.TagNames == nil {
			saved.TagNames = make(map[string]ComponentTagNames)
		}
		saved.TagNames[source.Name] = observation
		s.storeDashboard(saved)
	}
}

// mergeRecentTagVerification updates tag evidence without disturbing PR and retry observations.
func (s *Session) mergeRecentTagVerification(source release.Source, startingHead string, candidates []string, latest release.Repository, err error) error {
	old := s.repos[source.Name]
	if latest.Head != "" && old.Head != startingHead && old.Head != latest.Head {
		return fmt.Errorf("the component branch changed during tag verification")
	}
	updated := old
	updated.Source = source
	if latest.Head == "" {
		latest.Head = old.Head
	} else {
		updated.Head = latest.Head
		updated.Evidence = latest.Evidence
		updated.Evidence.Error = ""
	}
	latest.Tags = slices.Clone(latest.Tags)
	for _, name := range candidates {
		if !slices.ContainsFunc(latest.Tags, func(tag release.Tag) bool { return tag.Name == name }) {
			latest.Tags = append(latest.Tags, release.Tag{Name: name, Evidence: release.Evidence{At: time.Now(), Error: "Tag verification did not complete."}})
		}
	}
	mergeTags(old, &latest, DiscoveryRequest{Scope: ReadTags, TagCandidates: candidates})
	updated.Tags, updated.TagsPending = latest.Tags, latest.TagsPending || err != nil
	catalog := s.componentTagNames[source.Name]
	if catalog.Evidence.Fresh() && err == nil {
		updated.TagsPending = false
		for _, name := range catalog.Names {
			index := slices.IndexFunc(updated.Tags, func(tag release.Tag) bool { return tag.Name == name })
			if index < 0 || !updated.Tags[index].Evidence.Fresh() || updated.Tags[index].Belongs == "" {
				updated.TagsPending = true
				break
			}
			if updated.Tags[index].Belongs == release.Yes {
				break
			}
		}
	}
	s.repos[source.Name] = updated
	s.observeDashboardRepo(updated, false, "")
	if s.componentTagSpan(old) != s.componentTagSpan(updated) {
		s.wakePolls()
	}
	return err
}

func (s *Session) selectVerifiedFetchedTag(source release.Source, name string) error {
	for _, tag := range s.repos[source.Name].Tags {
		if tag.Name != name || tag.SHA == "" || !tag.Evidence.Fresh() {
			continue
		}
		if tag.Belongs == release.No {
			return fmt.Errorf("tag %s is outside %s", name, source.Branch)
		}
		if tag.Belongs == release.Yes {
			if err := s.confirmComponentTag(source, tag); err != nil {
				return err
			}
			s.polls[pollID{source.Name, pollTags}] = pollRecord{identity: tag.Name, finished: tag.Evidence.At}
			return nil
		}
	}
	return fmt.Errorf("could not verify that tag %s belongs to %s", name, source.Branch)
}

func (s *Session) cancelFetchedTagSelection(component string) {
	if s.componentTagActions[component].kind == "select-fetched-tag" {
		delete(s.componentTagActions, component)
	}
}

// confirmComponentTag saves a tag with verified branch membership and schedules its monitoring.
// The caller holds the session lock.
func (s *Session) confirmComponentTag(source release.Source, tag release.Tag) error {
	if err := s.options.ReleaseState.saveComponentTag(s.config.Version, source, tag.Name, tag.SHA); err != nil {
		return err
	}
	s.cancelFetchedTagSelection(source.Name)
	delete(s.pendingTags, source.Name)
	if old := s.choices.Tags[source.Name]; old != "" && old != tag.Name {
		s.archiveTag(source.Name, source.Repo)
	}
	s.workRevision++
	s.selectComponentTag(source, tag.Name, tag.SHA)
	target := release.Target{Repo: source.Repo, Tag: tag.Name}
	s.dispatcher.Ignore(target, false)
	delete(s.readErrors, "tag-selection "+release.Target{Repo: source.Repo}.String())
	s.record(target, tag.SHA, "tag-confirmed", "", tag.Name, "user")
	s.revalidateRCPlan()
	s.drain()
	select {
	case s.changed <- struct{}{}:
	default:
	}
	return nil
}
