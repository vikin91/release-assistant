package release

import (
	"fmt"
	"strings"
)

const (
	baselineNoTag       = "no-tag"
	baselineNewer       = "newer"
	baselineEqual       = "equal"
	baselineOlder       = "older"
	baselineUntaggedPin = "untagged-pin"
	baselineUnknown     = "unknown"
)

// ComponentPRPreparation gives the shared PR guidance once every component has a complete inventory without a tag.
func (a Assessment) ComponentPRPreparation() string {
	if len(a.ComponentBaseline) == 0 {
		return ""
	}
	for _, record := range a.ComponentBaseline {
		if record.Relation != baselineNoTag {
			return ""
		}
	}
	return "Component releases have not been cut for this ACS version yet, so PR preparation is the current step."
}

func (a Assessment) componentsAtBaseline() bool {
	for _, record := range a.ComponentBaseline {
		if record.Relation != baselineEqual || record.BranchAhead || record.OpenPRs > 0 {
			return false
		}
	}
	return len(a.ComponentBaseline) > 0
}

func compareComponentBaseline(source Source, previous Pins, repo Repository, newest Tag) BaselineRecord {
	record := BaselineRecord{
		Component: source.Name, PinFile: pinFile(previous, source.Name),
		PinnedVersion: strings.TrimPrefix(previous.Versions[source.Name], "v"),
		NewestTag:     newest.Name, NewestSHA: newest.SHA, BranchAhead: newest.SHA != "" && repo.Head != newest.SHA,
		Evidence: []Evidence{previous.Evidence, repo.Evidence},
	}
	for _, pr := range repo.PRs {
		if pr.Target.Repo == source.Repo && pr.Base == source.Branch && strings.EqualFold(pr.State, "open") {
			record.OpenPRs++
			record.Evidence = append(record.Evidence, pr.Evidence)
		}
	}
	var staleTags bool
	for _, tag := range repo.Tags {
		if _, inStream := tagVersion(source, tag.Name); !inStream {
			continue
		}
		record.Evidence = append(record.Evidence, tag.Evidence)
		staleTags = staleTags || !tag.Evidence.Fresh() || tag.SHA == "" || (tag.Belongs != Yes && tag.Belongs != No)
	}

	// A file read failure at the immutable ACS tag leaves the other values usable for comparisons with an observed tag.
	partialPins := previous.Evidence.Error != "" && !previous.Evidence.Known && !previous.Evidence.At.IsZero() && len(previous.Failures) > 0
	baselineKnown := previous.Evidence.Fresh() || (partialPins && newest.Name != "")
	component := componentTitle(source.Name)
	pin := record.PinFile + " " + record.PinnedVersion
	switch {
	case !baselineKnown || previous.Failures[source.Name] != "" || record.PinnedVersion == "":
		record.Relation = baselineUnknown
		record.Reason = fmt.Sprintf("%s comparison needs fresh previous ACS pin evidence for %s", component, strings.TrimSpace(pin))
	case repo.Head == "":
		record.Relation = baselineUnknown
		record.Reason = fmt.Sprintf("%s comparison against %s needs current evidence for branch %s", component, pin, source.Branch)
	case repo.TagsPending || staleTags:
		record.Relation = baselineUnknown
		record.Reason = fmt.Sprintf("%s comparison against %s needs a complete, current tag inventory", component, pin)
	case !repo.Evidence.Fresh():
		record.Relation = baselineUnknown
		record.Reason = fmt.Sprintf("%s comparison against %s needs fresh branch and tag inventory evidence for %s", component, pin, source.Branch)
	case newest.Name == "":
		record.Relation = baselineNoTag
		record.Reason = fmt.Sprintf("No component tag newer than %s was observed on %s/%s", pin, source.Repo, source.Branch)
	default:
		record.Relation, record.Reason = compareBaselineVersions(source, record)
	}
	if record.Relation == baselineUnknown && newest.Name != "" {
		record.Reason += fmt.Sprintf(", with %s as the highest observed eligible tag", newest.Name)
	}
	record.Reason += "."
	return record
}

func compareBaselineVersions(source Source, record BaselineRecord) (string, string) {
	component := componentTitle(source.Name)
	pin := record.PinFile + " " + record.PinnedVersion
	tag := fmt.Sprintf("The newest %s tag %s", component, record.NewestTag)
	_, _, newestPatch, _ := Version(record.NewestTag)
	_, _, pinnedPatch, _ := Version(record.PinnedVersion)
	// Within one stream the patch number orders final versions, even if the old tag is absent.
	if _, inStream := tagVersion(source, record.PinnedVersion); !inStream {
		return baselineUntaggedPin, fmt.Sprintf("The previous ACS pin %s has no matching eligible %s tag in stream %s, where the newest tag is %s", pin, component, source.Stream, record.NewestTag)
	}
	switch {
	case newestPatch > pinnedPatch:
		return baselineNewer, fmt.Sprintf("%s is newer than the previous ACS pin %s", tag, pin)
	case newestPatch < pinnedPatch:
		return baselineOlder, fmt.Sprintf("%s is older than the previous ACS pin %s", tag, pin)
	case record.BranchAhead || record.OpenPRs > 0:
		return baselineEqual, fmt.Sprintf("%s matches the previous ACS pin %s, but %s has advanced or has open PRs", tag, pin, source.Branch)
	default:
		return baselineEqual, fmt.Sprintf("%s matches the previous ACS pin %s and is at the branch tip", tag, pin)
	}
}
