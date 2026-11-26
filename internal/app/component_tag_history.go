package app

import (
	"context"
	"fmt"
	"slices"
	"time"

	"release-assistant/internal/release"
)

type (
	// ComponentTagDetails holds display evidence for the selected tag and its inventory.
	ComponentTagDetails struct {
		PRs                     []release.CandidatePR
		PRsKnown                bool
		DiscoveryError, PRError string
		NextRetry               time.Time
	}
	componentTagPRReader interface {
		ReadComponentTagPRs(context.Context, release.Source, string, string) ([]release.CandidatePR, error)
	}
	componentTagRange struct {
		tag, previousSHA, selectedSHA string
	}
	componentTagHistory struct {
		span componentTagRange
		prs  []release.CandidatePR
		err  string
	}
)

// componentTagSpan uses a complete inventory to find the closest earlier release
// in the configured stream. An unresolved earlier tag may be the predecessor.
func (s *releaseWork) componentTagSpan(repo release.Repository) componentTagRange {
	name := repo.Source.Name
	selected := s.choices.Tags[name]
	sha := s.confirmedSHAs[name]
	major, minor, selectedPatch, ok := release.Version(selected)
	if name == "acs" || repo.TagsPending || !ok || sha == "" {
		return componentTagRange{}
	}
	previousPatch := -1
	var previous release.Tag
	var selectedFound, ambiguous bool
	for _, tag := range repo.Tags {
		if tag.Name == selected {
			selectedFound = tag.SHA == sha && tag.Belongs == release.Yes
		}
		m, n, patch, valid := release.Version(tag.Name)
		if !valid || m != major || n != minor || patch >= selectedPatch || patch < previousPatch || tag.Belongs == release.No {
			continue
		}
		if patch > previousPatch {
			previous, previousPatch, ambiguous = tag, patch, false
		} else if tag.SHA != previous.SHA || tag.Belongs != previous.Belongs {
			ambiguous = true
		}
	}
	if !selectedFound || ambiguous || previous.SHA == "" || previous.Belongs != release.Yes {
		return componentTagRange{}
	}
	return componentTagRange{tag: selected, previousSHA: previous.SHA, selectedSHA: sha}
}

func (s *Session) refreshComponentTagHistory(ctx context.Context, generation, revision uint64, source release.Source) error {
	reader, ok := s.discovery.(componentTagPRReader)
	if !ok || source.Name == "acs" {
		return nil
	}
	s.mu.Lock()
	span := s.componentTagSpan(s.repos[source.Name])
	previous := s.componentTagHistory[source.Name]
	current := generation == s.generation && revision == s.workRevision
	s.mu.Unlock()
	if !current || span.tag == "" || (previous.span == span && previous.err == "") {
		return nil
	}
	prs, err := reader.ReadComponentTagPRs(ctx, source, span.previousSHA, span.selectedSHA)
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation != s.generation || revision != s.workRevision || canceled(ctx) || span != s.componentTagSpan(s.repos[source.Name]) {
		return nil
	}
	history := componentTagHistory{span: span, prs: prs}
	if err != nil {
		history.err = err.Error()
	}
	s.componentTagHistory[source.Name] = history
	s.recordReadResult(release.Target{Repo: source.Repo, Tag: span.tag}, "tag-history", history.err, "merged PRs for "+span.tag)
	if err != nil {
		return fmt.Errorf("reading merged PRs for %s: %w", span.tag, err)
	}
	return nil
}

func (s *Session) componentTagDetails(work *releaseWork, source release.Source, active bool) ComponentTagDetails {
	target := release.Target{Repo: source.Repo}
	details := ComponentTagDetails{DiscoveryError: work.readErrors["tags "+target.String()]}
	history := work.componentTagHistory[source.Name]
	span := work.componentTagSpan(work.repos[source.Name])
	if history.span == span && span.tag != "" {
		details.PRsKnown = history.err == ""
		details.PRs = slices.Clone(history.prs)
		details.PRError = history.err
	}
	if !active || (details.DiscoveryError == "" && details.PRError == "") {
		return details
	}
	var retry time.Time
	for _, job := range s.pollJobs(source) {
		if (job.kind != pollTags || details.DiscoveryError == "") && (job.kind != pollComponentTagHistory || details.PRError == "") {
			continue
		}
		record := work.polls[job.id(source)]
		if record.failures == 0 || record.identity != job.identity {
			continue
		}
		next := record.next(s.options.RefreshIntervals[job.setting], s.startupUntil)
		if resume := s.githubReadResumeAt(); job.kind == pollTags && resume.After(next) {
			next = resume
		}
		if retry.IsZero() || next.Before(retry) {
			retry = next
		}
	}
	if retry.After(time.Now()) {
		details.NextRetry = retry
	}
	return details
}
