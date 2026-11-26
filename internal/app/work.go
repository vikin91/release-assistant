package app

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"release-assistant/internal/release"
)

type sourceWork struct {
	revision      uint64
	scope         DiscoveryScope
	plans, images bool
	tagInventory  bool
	tagRefresh    string
	scheduled     bool
}

func (s *Session) rcReady() bool {
	return rcImageWaitReason(s.repos["acs"].Candidate) == ""
}

// RCImageWaitReason explains why monitoring cannot yet use the discovered RC.
func (s Snapshot) RCImageWaitReason() string {
	return rcImageWaitReason(s.Candidate())
}

func rcImageWaitReason(rc release.Candidate) string {
	switch {
	case rc.Evidence.Error != "":
		return "Unable to verify the current RC tag: " + rc.Evidence.Error
	case !rc.Evidence.Fresh():
		return "Waiting for GitHub to verify the current RC tag."
	case rc.Target.Tag == "":
		return "Waiting for a release candidate tag on the release branch."
	case !rc.Target.Valid() || rc.SHA == "":
		return "Waiting for GitHub to verify the RC commit."
	default:
		return ""
	}
}

// rcRetriesReady requires verified pinning before allowing RC retry writes.
func (s *Session) rcRetriesReady() bool {
	acs := s.repos["acs"]
	p := acs.PinningPR
	if !s.rcReady() || s.config.PinningPR == 0 || p == nil || p.Target != s.config.PinningTarget() ||
		!p.Evidence.Fresh() || !acs.PinningEvidence.Fresh() || p.Base != s.config.Branch ||
		!p.Merged || p.MergeSHA == "" || !acs.Pins.Evidence.Fresh() {
		return false
	}
	for _, source := range s.config.Components {
		selected := strings.TrimPrefix(s.choices.Tags[source.Name], "v")
		if selected == "" || strings.TrimPrefix(acs.Pins.Versions[source.Name], "v") != selected {
			return false
		}
	}
	return true
}

func mergeTags(old release.Repository, latest *release.Repository, request DiscoveryRequest) {
	if !request.Reads(ReadCoverage) {
		for i := range latest.Tags {
			for _, tag := range old.Tags {
				if tag.Name == latest.Tags[i].Name && tag.SHA == latest.Tags[i].SHA {
					latest.Tags[i].Contains = maps.Clone(tag.Contains)
					break
				}
			}
		}
	}
	readsTagInventory := request.Reads(ReadTags) && (request.Scope == ReadAll || request.ConfirmedTag == "") ||
		request.Source.Name == "acs" && request.Reads(ReadRC)
	if latest.TagsPending || !readsTagInventory {
		tags := slices.Clone(old.Tags)
		for i := range tags {
			if latest.Head != old.Head || latest.TagsPending && readsTagInventory || (request.Reads(ReadTags) && tags[i].Name == request.ConfirmedTag) {
				tags[i].Evidence.Known = false
			}
		}
		for _, tag := range latest.Tags {
			index := slices.IndexFunc(tags, func(t release.Tag) bool { return t.Name == tag.Name })
			if index < 0 {
				tags = append(tags, tag)
			} else {
				tags[index] = tag
			}
		}
		latest.Tags = tags
	}
	if !readsTagInventory {
		latest.TagsPending = latest.TagsPending || old.TagsPending || (latest.Source.Name != "acs" && old.Head != latest.Head)
	}
}

func mergeDiscovery(old, latest release.Repository, request DiscoveryRequest) release.Repository {
	mergeTags(old, &latest, request)
	if !request.Reads(ReadCoverage) {
		latest.CommitGraphLimitReached = old.CommitGraphLimitReached
	}
	if !request.Reads(ReadChecks) {
		byTarget := make(map[release.Target]release.PR, len(old.PRs))
		for _, p := range old.PRs {
			byTarget[p.Target] = p
		}
		for i := range latest.PRs {
			p := &latest.PRs[i]
			p.Checks, p.ChecksEvidence, p.CheckResultsEvidence, p.RequiredChecksError = nil, release.Evidence{}, release.Evidence{}, ""
			previous := byTarget[p.Target]
			if p.State == "open" && previous.State == "open" && p.SHA != "" && previous.SHA == p.SHA &&
				previous.Base == p.Base && p.Base == request.Source.Branch && p.Evidence.Fresh() && previous.Evidence.Fresh() {
				p.Checks = slices.Clone(previous.Checks)
				p.ChecksEvidence, p.CheckResultsEvidence = previous.ChecksEvidence, previous.CheckResultsEvidence
				p.RequiredChecksError = previous.RequiredChecksError
			}
		}
	}
	if !request.Reads(ReadPRs) {
		latest.PRListEvidence = old.PRListEvidence
		latest.PinDiscovery = old.PinDiscovery
		if old.Head != latest.Head {
			latest.PinDiscovery.Files = nil
			latest.PinDiscovery.Evidence.Known = false
		}
		for _, pr := range latest.PRs {
			match, ok := latest.PinDiscovery.Files[pr.Target.PR]
			if (ok && (match.Head != pr.SHA || pr.State != "open")) || (!ok && pr.State == "open") {
				latest.PinDiscovery.Evidence.Known = false
			}
		}
		prs := slices.Clone(old.PRs)
		for _, p := range latest.PRs {
			index := slices.IndexFunc(prs, func(pr release.PR) bool { return pr.Target == p.Target })
			if index < 0 {
				prs = append(prs, p)
			} else {
				prs[index] = p
			}
		}
		for i := range prs {
			if request.Reads(ReadTargets) && slices.Contains(request.Tracked, prs[i].Target) && !slices.ContainsFunc(latest.PRs, func(p release.PR) bool { return p.Target == prs[i].Target }) {
				prs[i].Evidence.Known = false
				prs[i].ChecksEvidence.Known = false
				prs[i].CheckResultsEvidence.Known = false
			}
		}
		latest.PRs = prs
	}
	if !request.Reads(ReadPRs) && latest.Head == old.Head && !old.Evidence.At.IsZero() {
		latest.Evidence = old.Evidence
	}
	if !request.Reads(ReadPreviousPins) {
		latest.PreviousPins, latest.PreviousPublished = old.PreviousPins, old.PreviousPublished
	}
	if !request.Reads(ReadPins) {
		latest.Pins, latest.PinningPR, latest.PinningPins, latest.PinningEvidence = old.Pins, old.PinningPR, old.PinningPins, old.PinningEvidence
		if latest.Head != old.Head {
			latest.Pins.Evidence.Known = false
		}
	}
	if pin := latest.PinningPR; pin != nil && (request.Reads(ReadPins) || request.Reads(ReadTargets) && slices.Contains(request.Tracked, pin.Target)) {
		// Both dashboard views must use the same revision. Pin-file reads stay
		// tied to the revision from which their contents were read.
		for _, p := range latest.PRs {
			if p.Target != pin.Target || (request.Reads(ReadPins) && (!latest.PinningEvidence.Fresh() || p.SHA != pin.SHA || p.Base != pin.Base || p.State != pin.State)) {
				continue
			}
			if p.SHA != pin.SHA || (p.Merged && p.MergeSHA != pin.MergeSHA) || p.Merged != pin.Merged || p.Base != pin.Base || !p.Evidence.Fresh() {
				latest.PinningPins.Evidence.Known = false
			}
			latest.PinningPR = new(p)
			if !request.Reads(ReadPins) {
				latest.PinningEvidence = p.Evidence
			}
			break
		}
	}
	if request.Source.Name == "acs" {
		switch {
		case request.Reads(ReadRC):
			// A tag refresh cannot replace PR associations for an identical commit range.
			if latest.Candidate.PRsPending && sameCandidateRange(old.Candidate, latest.Candidate) {
				latest.Candidate.PRs, latest.Candidate.PRError, latest.Candidate.PRsPending = old.Candidate.PRs, old.Candidate.PRError, old.Candidate.PRsPending
			}
		case latest.Head != old.Head:
			latest.Candidate = release.Candidate{}
		case !request.Reads(ReadPRs) || !sameCandidateRange(old.Candidate, latest.Candidate):
			latest.Candidate = old.Candidate
		}
	}
	return latest
}

// sameCandidateRange binds PR associations to the exact tag and branch commit SHAs.
func sameCandidateRange(a, b release.Candidate) bool {
	return a.Target == b.Target && a.SHA != "" && a.SHA == b.SHA && a.CompareURL != "" && a.CompareURL == b.CompareURL
}

// recordDiscoveryChanges reports newly available work and PR transitions once.
// The caller holds the session lock.
func (s *Session) recordDiscoveryChanges(old, latest release.Repository, request DiscoveryRequest) {
	if request.Reads(ReadTags) && request.ConfirmedTag == "" {
		newTags := 0
		for _, tag := range latest.Tags {
			if tag.Evidence.Fresh() && tag.Belongs == release.Yes && !slices.ContainsFunc(old.Tags, func(previous release.Tag) bool { return previous.Name == tag.Name }) {
				newTags++
			}
		}
		if newTags > 0 {
			message := fmt.Sprintf("Found %s on %s. Review component release choices.", counted(newTags, "additional release tag"), consoleBranch(latest.Source))
			if len(old.Tags) == 0 {
				message = discoverySummary(request, latest, latest.TagsPending)
			}
			s.record(release.Target{Repo: latest.Source.Repo}, "", "tags-discovered", "", message, "system", message)
		}
	}
	for _, pr := range latest.PRs {
		if !pr.Evidence.Fresh() {
			continue
		}
		index := slices.IndexFunc(old.PRs, func(previous release.PR) bool { return previous.Target == pr.Target })
		message := ""
		switch {
		case index < 0 && pr.State == "open":
			message = fmt.Sprintf("Found pull request %s targeting %s: %s.", consoleTarget(pr.Target), pr.Base, pr.Title)
		case index >= 0 && !old.PRs[index].Merged && pr.Merged:
			message = fmt.Sprintf("Pull request %s merged into %s at merge commit %s.", consoleTarget(pr.Target), pr.Base, pr.MergeSHA)
		case index >= 0 && old.PRs[index].State == "open" && pr.State == "closed":
			message = fmt.Sprintf("Pull request %s closed without merging.", consoleTarget(pr.Target))
		}
		if message != "" {
			s.record(pr.Target, pr.SHA, "pr-state", "", message, "system", message)
		}
	}
}
