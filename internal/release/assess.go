package release

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

const (
	imageCI         = "ci"
	imageKonflux    = "konflux"
	openACSPRReason = "Included PR is open; review its checks and merge it into the ACS release branch"
)

var pinValueRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

// Assess derives release readiness and next steps from the supplied observations.
func Assess(in Input) Assessment {
	a := Assessment{ProposedTags: make(map[string]string), ReleaseGuides: make(map[string]string)}
	addCondition := func(condition Condition) {
		if condition.Status != "" {
			for _, evidence := range condition.Evidence {
				if !evidence.Fresh() {
					condition.Status = ""
					condition.Reason += "; current evidence is not fresh"
					break
				}
			}
		}
		condition.Evidence = nonNilEvidence(condition.Evidence)
		a.Conditions = append(a.Conditions, condition)
	}
	add := func(name string, status Truth, reason string, evidence ...Evidence) {
		addCondition(Condition{Name: name, Status: status, Reason: reason, Evidence: evidence})
	}

	configErr := in.Config.Validate()
	if configErr != nil {
		add("configuration", No, "Release configuration is incomplete or invalid: "+configErr.Error())
	} else {
		add("configuration", Yes, "Release configuration is valid")
	}

	repos := make(map[string]Repository, len(in.Repositories))
	for _, r := range in.Repositories {
		if _, exists := repos[r.Source.Name]; !exists {
			repos[r.Source.Name] = r
		}
	}
	prSources := append([]Source(nil), in.Config.Components...)
	prSources = append(prSources, in.Config.ACS())

	included := includedTargets(in.Choices.PRs)
	prStates := make(map[Target]Truth, len(included))
	var awaitingACSMerge bool
	for _, target := range included {
		pr, repo, found := findPR(repos, prSources, target)
		status, reason := assessIncludedPR(in.Config, target, pr, repo, found)
		prStates[target] = status
		var evidence []Evidence
		if found {
			evidence = []Evidence{repo.Evidence, pr.Evidence}
		}
		condition := Condition{Name: "included PR " + target.String(), Status: status, Reason: reason, Evidence: evidence}
		addCondition(condition)
		if condition.AwaitsACSMerge() {
			awaitingACSMerge = true
			continue
		}
		if status != Yes {
			recommendation := includedPRRecommendation(target, pr, status, reason)
			if target.Repo == in.Config.ACS().Repo {
				a.ACSRecommendations = append(a.ACSRecommendations, recommendation)
			} else {
				a.ComponentPRRecommendations = append(a.ComponentPRRecommendations, recommendation)
			}
		}
	}
	if awaitingACSMerge {
		a.ACSRecommendations = append(a.ACSRecommendations, "Merge included PRs and cut new RC.")
	}
	if len(a.ComponentPRRecommendations) == 0 {
		a.ComponentPRRecommendations = append(a.ComponentPRRecommendations, "Review component PRs for this patch and include the changes you want to merge into the release. Empty PR selection is valid.")
	}

	chosenVersions := make(map[string]string, len(in.Config.Components))
	tagCoverage := make(map[string]Condition, len(in.Config.Components))
	for _, source := range in.Config.Components {
		repo, found := repos[source.Name]
		confirmed := in.Choices.Tags[source.Name]
		coverage, tag, tagFound := assessComponentTag(source, repo, found, included, prStates, confirmed)
		tagCoverage[source.Name] = coverage
		if tagFound {
			if version, ok := tagVersion(source, tag.Name); ok {
				chosenVersions[source.Name] = version
			}
		}
		eligible, ok := highestEligibleTag(source, repo, found)
		if ok {
			a.ProposedTags[source.Name] = eligible.Name
		}
		baseline := compareComponentBaseline(source, repos["acs"].PreviousPins, repo, eligible)
		a.ComponentBaseline = append(a.ComponentBaseline, baseline)
		var evidence []Evidence
		if found {
			evidence = append(evidence, repo.Evidence)
		}
		if tagFound {
			evidence = append(evidence, tag.Evidence)
		}
		coverage.Name = "tag " + source.Name
		coverage.Evidence = evidence
		addCondition(coverage)
	}
	addTagRecommendations(&a, in, included, prStates, tagCoverage)
	if guidance := a.ComponentPRPreparation(); guidance != "" {
		a.ComponentPRRecommendations = append(a.ComponentPRRecommendations, guidance)
	}
	componentsAtBaseline := a.componentsAtBaseline()

	imageVersions := make(map[string]string, len(in.Config.Components))
	for _, source := range in.Config.Components {
		if version, ok := tagVersion(source, in.Choices.Tags[source.Name]); ok {
			imageVersions[source.Name] = version
		}
	}
	imageStatus, imageEvidence := assessImages(&a, in, imageVersions)
	for _, image := range a.Images {
		if !image.Required {
			continue
		}
		add("image "+image.Image.Component+" "+image.Image.Family, image.Status, image.Reason, image.Image.Evidence)
		if in.Choices.Tags[image.Image.Component] != "" && image.Status != Yes {
			a.ComponentReleaseRecommendations = append(a.ComponentReleaseRecommendations, componentImageRecommendation(image))
		}
	}
	if a.ImageProblem != "" {
		add("component images", "", a.ImageProblem, imageEvidence...)
	}

	a.Pins = assessPinSummary(in, repos, chosenVersions)
	var pinEvidence []Evidence
	if acs, ok := repos["acs"]; ok {
		pinEvidence = []Evidence{{Known: acs.Evidence.Known, At: acs.Evidence.At}, acs.Pins.Evidence}
	}
	add("ACS release branch pins", a.Pins.BranchStatus, a.Pins.BranchReason, pinEvidence...)
	missingTags := false
	for _, source := range in.Config.Components {
		missingTags = missingTags || in.Choices.Tags[source.Name] == ""
	}
	if missingTags {
		a.ACSRecommendations = append(a.ACSRecommendations, `Select component tags in "Component releases" tab.`)
	}
	if a.Pins.BranchStatus == No && !a.Pins.PROpen {
		a.ACSRecommendations = append(a.ACSRecommendations, "Review ACS release branch pins: "+strings.TrimSuffix(a.Pins.BranchReason, ".")+".")
	} else if a.Pins.BranchStatus == "" && (!missingTags || repos["acs"].Pins.Evidence.Error != "") {
		a.ACSRecommendations = append(a.ACSRecommendations, "Review ACS release branch pin discovery: "+strings.TrimSuffix(a.Pins.BranchReason, ".")+".")
	}

	assessPinningPR(&a, in, chosenVersions, tagCoverage, imageStatus)
	for _, repo := range in.Repositories {
		if repo.PreviousPublished == Yes && repo.Source.Name == "acs" && noOutstandingACSPR(repo) && !componentsAtBaseline {
			a.ACSRecommendations = append(a.ACSRecommendations, "The previous ACS patch is published and no remaining ACS PRs were observed, so a new patch may be unnecessary.")
		}
	}
	a.Ready = len(a.Conditions) > 0
	for _, condition := range a.Conditions {
		if condition.Status != Yes {
			a.Ready = false
			break
		}
	}
	if a.Ready {
		a.ACSRecommendations = append(a.ACSRecommendations, "The included PRs, selected component tags, images, and release branch pins are ready.")
	}
	if len(a.ACSRecommendations) == 0 {
		a.ACSRecommendations = append(a.ACSRecommendations, "Review ACS backports and dependency updates for this patch. Additional ACS PRs can be included and merged after pinning.")
	}
	a.Recommendations = append(a.Recommendations, a.ComponentPRRecommendations...)
	a.Recommendations = append(a.Recommendations, a.ComponentReleaseRecommendations...)
	a.Recommendations = append(a.Recommendations, a.ACSRecommendations...)
	return a
}

// AwaitsACSMerge reports whether this condition is covered by the shared ACS merge recommendation.
func (c Condition) AwaitsACSMerge() bool {
	return c.Status == No && c.Reason == openACSPRReason
}

func includedPRRecommendation(target Target, pr PR, status Truth, reason string) string {
	if status == "" {
		return fmt.Sprintf("Refresh the evidence for included PR %s: %s.", target, reason)
	}
	if strings.EqualFold(pr.State, "closed") && !pr.Merged {
		return fmt.Sprintf("Included PR %s is closed without merging. Reopen it or exclude it from this patch.", target)
	}
	if strings.EqualFold(pr.State, "open") && strings.HasPrefix(reason, "Included PR is open") {
		return fmt.Sprintf("Review the checks for included PR %s and merge it into %s when ready.", target, pr.Base)
	}
	return fmt.Sprintf("Review included PR %s: %s.", target, reason)
}

func componentImageRecommendation(image ImageStatus) string {
	reason := strings.TrimSuffix(image.Reason, ".")
	if image.Status == No {
		return "Wait for or investigate required component images: " + reason + "."
	}
	return "Refresh component image observations and registry access: " + reason + "."
}

func includedTargets(choices map[Target]Choice) []Target {
	var targets []Target
	for target, choice := range choices {
		if choice == Included {
			targets = append(targets, target)
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].String() < targets[j].String() })
	return targets
}

func findPR(repos map[string]Repository, sources []Source, target Target) (PR, Repository, bool) {
	for _, source := range sources {
		if source.Repo != target.Repo {
			continue
		}
		repo, ok := repos[source.Name]
		if !ok {
			return PR{}, Repository{}, false
		}
		for _, pr := range repo.PRs {
			if pr.Target == target {
				return pr, repo, true
			}
		}
		return PR{}, repo, false
	}
	return PR{}, Repository{}, false
}

func assessIncludedPR(config Config, target Target, pr PR, repo Repository, found bool) (Truth, string) {
	if !target.Valid() || target.PR == 0 {
		return "", "Included target is not a valid PR target"
	}
	if !found || !repo.Evidence.Fresh() {
		return "", "Included PR was not found in fresh repository discovery"
	}
	if !pr.Evidence.Fresh() {
		return "", "Included PR state is not fresh"
	}
	if pr.Base == "" {
		return "", "Included PR target branch is unknown"
	}
	configured := false
	for _, source := range config.Components {
		if source.Repo == target.Repo && source.Branch == pr.Base {
			configured = true
			break
		}
	}
	acs := config.ACS()
	if target.Repo == acs.Repo && pr.Base == config.Branch {
		configured = true
	}
	if !configured {
		return No, fmt.Sprintf("PR targets %q, not its configured release branch", pr.Base)
	}
	if pr.Merged {
		if pr.MergeSHA == "" {
			return "", "PR is marked merged but its merge commit is unknown"
		}
		if target.Repo == acs.Repo {
			return Yes, "Included PR is merged into its configured ACS release branch"
		}
		return Yes, "Included PR is merged into its configured component branch"
	}
	if strings.EqualFold(pr.State, "open") {
		if target.Repo == acs.Repo {
			return No, openACSPRReason
		}
		return No, "Included PR is open; wait for its merge and a tag covering the merge"
	}
	if strings.EqualFold(pr.State, "closed") {
		return No, "Included PR is closed without a recorded merge"
	}
	return "", "Included PR merge state is unknown"
}

func assessComponentTag(source Source, repo Repository, found bool, included []Target, prStates map[Target]Truth, confirmed string) (Condition, Tag, bool) {
	if confirmed == "" {
		return Condition{Reason: "No component tag is selected for this release yet. Component PRs can be prepared and merged before selecting a tag"}, Tag{}, false
	}
	if !found || !repo.Evidence.Fresh() || repo.Head == "" {
		return Condition{Reason: "Component branch head or discovery evidence is not fresh"}, Tag{}, false
	}
	tag, ok := findTag(repo.Tags, confirmed)
	if !ok {
		return Condition{Reason: "Confirmed tag was not found in current discovery"}, Tag{}, false
	}
	coverage := tagSuitability(source, repo, tag, included, prStates)
	coverage.Reason = "Confirmed tag " + confirmed + ": " + coverage.Reason
	return coverage, tag, true
}

func tagSuitability(source Source, repo Repository, tag Tag, included []Target, prStates map[Target]Truth) Condition {
	if _, ok := tagVersion(source, tag.Name); !ok {
		return Condition{Status: No, Reason: "tag is not a final semantic version in the configured stream"}
	}
	if !repo.Evidence.Fresh() || !tag.Evidence.Fresh() || repo.Head == "" || tag.SHA == "" {
		return Condition{Reason: "branch head, tag identity, or tag evidence is not fresh"}
	}
	if tag.Belongs == No {
		return Condition{Status: No, Reason: "tag is not an ancestor of the configured component branch"}
	}
	if tag.Belongs != Yes {
		return Condition{Reason: "branch ancestry for this tag is unknown"}
	}
	for _, target := range included {
		if target.Repo != source.Repo || target.PR == 0 {
			continue
		}
		switch prStates[target] {
		case No:
			return Condition{Status: No, Reason: "an included PR is still open or otherwise not merged", TagIssue: TagPRUnmerged}
		case "":
			return Condition{Reason: "included PR merge state is unknown"}
		}
		pr, _, found := findPR(map[string]Repository{source.Name: repo}, []Source{source}, target)
		if !found || pr.MergeSHA == "" {
			return Condition{Reason: "included PR merge commit is unknown"}
		}
		if tag.Contains == nil {
			return Condition{Reason: "tag containment observations are missing"}
		}
		switch tag.Contains[pr.MergeSHA] {
		case Yes:
		case No:
			return Condition{Status: No, Reason: "tag does not contain included PR merge " + pr.MergeSHA, TagIssue: TagMissingMerge}
		default:
			return Condition{Reason: "tag containment for included PR merge is unknown"}
		}
	}
	return Condition{Status: Yes, Reason: "tag belongs to the configured branch and covers every included merged PR"}
}

func highestEligibleTag(source Source, repo Repository, found bool) (Tag, bool) {
	if !found || !repo.Evidence.Fresh() || repo.Head == "" {
		return Tag{}, false
	}
	var eligible []Tag
	for _, tag := range repo.Tags {
		if _, ok := tagVersion(source, tag.Name); ok && tag.SHA != "" && tag.Belongs == Yes && tag.Evidence.Fresh() {
			eligible = append(eligible, tag)
		}
	}
	sort.Slice(eligible, func(i, j int) bool {
		mi, ni, pi, _ := Version(strings.TrimPrefix(eligible[i].Name, "v"))
		mj, nj, pj, _ := Version(strings.TrimPrefix(eligible[j].Name, "v"))
		if mi != mj {
			return mi > mj
		}
		if ni != nj {
			return ni > nj
		}
		return pi > pj
	})
	if len(eligible) == 0 {
		return Tag{}, false
	}
	return eligible[0], true
}

func findTag(tags []Tag, name string) (Tag, bool) {
	for _, tag := range tags {
		if tag.Name == name {
			return tag, true
		}
	}
	return Tag{}, false
}

func tagVersion(source Source, name string) (string, bool) {
	version := strings.TrimPrefix(name, "v")
	major, minor, _, ok := Version(version)
	if !ok || fmt.Sprintf("%d.%d", major, minor) != source.Stream {
		return "", false
	}
	return version, true
}

func assessImages(a *Assessment, in Input, versions map[string]string) (Truth, []Evidence) {
	globalProblem := ""
	bindingProblem := ""
	switch {
	case len(in.Config.Components) == 0:
		globalProblem = "No component image checks are configured."
	case in.Inventory.ReleaseVersion == "":
		bindingProblem = "Image checks have not returned results for the configured ACS release yet."
		globalProblem = bindingProblem
	case in.Inventory.ReleaseVersion != strings.TrimPrefix(in.Config.Version, "v"):
		bindingProblem = "Image results do not match the configured ACS release version."
		globalProblem = bindingProblem
	}
	if globalProblem == "" && !in.Inventory.Evidence.Fresh() {
		globalProblem = "Component image checks do not have up-to-date results."
	}
	a.ImageProblem = globalProblem
	evidence := []Evidence{in.Inventory.Evidence}
	allPresent := len(in.Config.Components) > 0

	for _, source := range in.Config.Components {
		version := versions[source.Name]
		selectedTag := in.Choices.Tags[source.Name]
		for _, family := range []string{imageCI, imageKonflux} {
			matches := matchingImages(in.Inventory.Images, source.Name, family)
			if len(matches) == 0 {
				allPresent = false
				reason := fmt.Sprintf("The %s image for %s has no current registry result.", family, source.Name)
				if globalProblem != "" {
					reason = globalProblem
				}
				a.Images = append(a.Images, ImageStatus{Image: Image{Component: source.Name, Family: family}, SelectedTag: selectedTag, Required: true, Reason: reason})
				continue
			}
			for _, image := range matches {
				imageStatus := ImageStatus{Image: image, SelectedTag: selectedTag, Required: true}
				switch {
				case bindingProblem != "":
					imageStatus.Reason = bindingProblem
				case version == "" || selectedTag == "":
					imageStatus.Reason = fmt.Sprintf("The selected %s version is unavailable, so this image cannot be checked.", source.Name)
				case in.Inventory.Versions[source.Name] == "" || in.Inventory.Versions[source.Name] != version:
					imageStatus.Reason = fmt.Sprintf("Image results for %s do not match selected version %s.", source.Name, version)
				case image.Reference == "":
					imageStatus.Reason = fmt.Sprintf("The %s image reference for %s is unavailable.", family, source.Name)
				case !image.Evidence.Fresh() || image.Present == "":
					imageStatus.Reason = fmt.Sprintf("The %s image registry result for %s is unavailable.", family, source.Name)
				case image.Present == No:
					imageStatus.Status = No
					imageStatus.Reason = fmt.Sprintf("The %s image is missing: %s.", family, image.Reference)
				case image.Present == Yes:
					imageStatus.Status = Yes
					imageStatus.Reason = fmt.Sprintf("The %s image is available: %s.", family, image.Reference)
				default:
					imageStatus.Reason = fmt.Sprintf("Availability of the %s image for %s is unknown.", family, source.Name)
				}
				if imageStatus.Status != Yes {
					allPresent = false
				}
				a.Images = append(a.Images, imageStatus)
			}
		}
	}
	for _, image := range in.Inventory.Images {
		if image.Component == "acs" {
			status := assessObservedImage(image, in.Inventory.Versions["acs"], bindingProblem)
			status.Required = false
			a.Images = append(a.Images, status)
			continue
		}
		if !configuredImage(in.Config.Components, image.Component, image.Family) {
			a.Images = append(a.Images, ImageStatus{Image: image, Required: true, Reason: "This image does not match a configured component family, so its availability is unknown."})
			if a.ImageProblem == "" {
				a.ImageProblem = "Image results include an unconfigured component or family."
			}
			allPresent = false
			continue
		}
	}
	for _, image := range in.Inventory.ReleaseImages {
		status := assessObservedImage(image, in.Inventory.Versions["acs"], bindingProblem)
		status.Required = false
		a.Images = append(a.Images, status)
	}
	selected := 0
	for _, source := range in.Config.Components {
		if in.Choices.Tags[source.Name] != "" {
			selected++
		}
		for _, family := range []string{imageCI, imageKonflux} {
			if len(matchingImages(in.Inventory.Images, source.Name, family)) == 0 {
				a.ImageProblem = "One or more expected component image observations are missing."
			}
		}
	}
	if a.ImageProblem == "" {
		for _, image := range a.Images {
			if image.Required && image.Status == "" {
				a.ImageProblem = "Some component image results are incomplete: " + image.Reason
				break
			}
		}
	}
	if len(in.Inventory.Images) == 0 && globalProblem == "" {
		a.ImageProblem = "Expected component image observations are missing."
	}
	if missing := len(in.Config.Components) - selected; missing > 0 {
		a.ImageProblem = fmt.Sprintf("Select component tags first. %d selected, %d missing.", selected, missing)
	}
	if globalProblem != "" {
		allPresent = false
	}
	if allPresent {
		return Yes, evidence
	}
	for _, image := range a.Images {
		if image.Required && image.Status == No {
			return No, evidence
		}
	}
	return "", evidence
}

func configuredImage(sources []Source, component, family string) bool {
	if family != imageCI && family != imageKonflux {
		return false
	}
	for _, source := range sources {
		if source.Name == component {
			return true
		}
	}
	return false
}

func assessObservedImage(image Image, selectedTag, globalProblem string) ImageStatus {
	status := ImageStatus{Image: image, SelectedTag: selectedTag}
	switch {
	case globalProblem != "":
		status.Reason = globalProblem
	case image.Reference == "":
		status.Reason = "The image reference is unavailable."
	case !image.Evidence.Fresh() || image.Present == "":
		status.Reason = "The image registry result is unavailable."
	case image.Present == No:
		status.Status = No
		status.Reason = "The image is missing: " + image.Reference + "."
	case image.Present == Yes:
		status.Status = Yes
		status.Reason = "The image is available: " + image.Reference + "."
	default:
		status.Reason = "Image availability is unknown."
	}
	return status
}

func matchingImages(images []Image, component, family string) []Image {
	var matches []Image
	for _, image := range images {
		if image.Component == component && image.Family == family {
			matches = append(matches, image)
		}
	}
	return matches
}

func findRepository(repos []Repository, name string) (Repository, bool) {
	for _, repo := range repos {
		if repo.Source.Name == name {
			return repo, true
		}
	}
	return Repository{}, false
}

func assessPinSummary(in Input, repos map[string]Repository, versions map[string]string) PinSummary {
	acs, found := repos["acs"]
	comparisonVersions := make(map[string]string, len(versions))
	for name, version := range versions {
		comparisonVersions[name] = version
	}
	for _, source := range in.Config.Components {
		if comparisonVersions[source.Name] == "" {
			if version, ok := tagVersion(source, in.Choices.Tags[source.Name]); ok {
				comparisonVersions[source.Name] = version
			}
		}
	}
	summary := PinSummary{
		BranchSummary: "Pending: reading ACS release branch pins.",
		PRSummary:     "No pinning PR configured.",
	}
	branchProblems := []string{}
	if !found {
		summary.BranchSummary = "Pending: discovering the ACS release branch."
	} else if acs.Head == "" || !acs.Evidence.Known || acs.Evidence.At.IsZero() {
		if acs.Evidence.Error != "" {
			summary.BranchSummary = "Unable to get the ACS release branch head."
			branchProblems = append(branchProblems, acs.Evidence.Error)
		} else {
			summary.BranchSummary = "Pending: reading the ACS release branch head."
		}
	} else if !acs.Pins.Evidence.Fresh() {
		if acs.Pins.Evidence.Error != "" {
			summary.BranchSummary = "Unable to get " + unreadPinFiles(acs.Pins, in.Config.Components) + " from the ACS release branch."
			branchProblems = pinReadProblems(acs.Pins, in.Config.Components)
			if len(branchProblems) == 0 {
				branchProblems = append(branchProblems, "Unable to read ACS release branch version files: "+acs.Pins.Evidence.Error)
			}
		} else {
			summary.BranchSummary = "Pending: reading ACS release branch version files."
		}
	} else {
		missingSelected := []string{}
		unknownPins := []string{}
		mismatches := []string{}
		for _, source := range in.Config.Components {
			selected := comparisonVersions[source.Name]
			actual, hasActual := acs.Pins.Versions[source.Name]
			if selected == "" {
				if in.Choices.Tags[source.Name] == "" {
					missingSelected = append(missingSelected, componentTitle(source.Name))
				} else {
					branchProblems = append(branchProblems, fmt.Sprintf("Selected %s tag %q does not provide a final %s version", componentTitle(source.Name), in.Choices.Tags[source.Name], source.Stream))
				}
			}
			if !hasActual || actual == "" {
				if failure := acs.Pins.Failures[source.Name]; failure != "" {
					unknownPins = append(unknownPins, fmt.Sprintf("%s: %s (%s)", pinFile(acs.Pins, source.Name), failure, componentTitle(source.Name)))
				} else {
					unknownPins = append(unknownPins, fmt.Sprintf("%s is empty or missing", pinFile(acs.Pins, source.Name)))
				}
				continue
			}
			if selected != "" && selected != actual {
				mismatches = append(mismatches, fmt.Sprintf("%s actual %s, selected %s", source.Name, actual, selected))
			}
		}
		if len(missingSelected) > 0 {
			summary.BranchSummary = "ACS branch pins: component tag selection required."
			branchProblems = append(branchProblems, "Choose "+joinComponentTitles(missingSelected)+" tags")
		}
		if len(unknownPins) > 0 {
			summary.BranchSummary = "Unable to get " + unreadPinFiles(acs.Pins, in.Config.Components) + " from the ACS release branch."
		}
		branchProblems = append(branchProblems, unknownPins...)
		switch {
		case len(mismatches) > 0:
			summary.BranchStatus = No
			summary.BranchSummary = "Actual ACS branch pins differ from selected component versions."
			branchProblems = append(mismatches, branchProblems...)
		case len(branchProblems) == 0:
			summary.BranchStatus = Yes
			summary.BranchSummary = "Actual ACS branch pins match every selected component version."
			summary.BranchReason = "Actual branch pins match selected versions."
		}
	}
	summary.BranchProblems = branchProblems
	if len(branchProblems) > 0 {
		summary.BranchReason = strings.Join(branchProblems, "; ") + "."
	} else if summary.BranchReason == "" {
		summary.BranchReason = summary.BranchSummary
	}

	if in.Config.PinningPR > 0 {
		summary.PRNumber = in.Config.PinningPR
		summary.PRURL = fmt.Sprintf("https://github.com/%s/pull/%d", in.Config.ACS().Repo, in.Config.PinningPR)
		summary.PRSummary = fmt.Sprintf("Pending: reading pinning PR #%d", in.Config.PinningPR)
		summary.PRReason = summary.PRSummary

	}
	if !found {
		return summary
	}
	pr := acs.PinningPR
	if in.Config.PinningPR > 0 {
		if pr != nil && (pr.Target.PR != in.Config.PinningPR || pr.Target.Repo != in.Config.ACS().Repo) {
			pr = nil
		}
		if acs.PinningEvidence.Error != "" {
			summary.PRSummary = fmt.Sprintf("Unable to get PR status and component versions from pinning PR #%d", in.Config.PinningPR)
			summary.PRProblems = []string{acs.PinningEvidence.Error}
			summary.PRReason = strings.Join(summary.PRProblems, "; ")
			return summary
		}
		if acs.PinningEvidence.Known && acs.PinningEvidence.At.IsZero() {
			summary.PRSummary = fmt.Sprintf("Pending: reading pinning PR #%d metadata", in.Config.PinningPR)
			summary.PRReason = summary.PRSummary
			return summary
		}
		if pr == nil {
			return summary
		}
	}
	if pr == nil {
		if acs.PinningEvidence.Error != "" {
			summary.PRSummary = "Unable to get pinning PR metadata"
			summary.PRProblems = []string{acs.PinningEvidence.Error}
			summary.PRReason = strings.Join(summary.PRProblems, "; ")
		} else if acs.PinningEvidence.Known || !acs.PinningEvidence.At.IsZero() {
			summary.PRSummary = "Pending: reading pinning PR metadata"
			summary.PRReason = summary.PRSummary
		}
		return summary
	}
	summary.PRNumber = pr.Target.PR
	if in.Config.PinningPR == 0 && pr.URL != "" {
		summary.PRURL = pr.URL
	} else if in.Config.PinningPR == 0 && ValidRepo(pr.Target.Repo) && pr.Target.PR > 0 {
		summary.PRURL = fmt.Sprintf("https://github.com/%s/pull/%d", pr.Target.Repo, pr.Target.PR)
	}
	metadata := acs.PinningEvidence
	if metadata.Known || !metadata.At.IsZero() || metadata.Error != "" {
		if !metadata.Fresh() {
			summary.PRSummary = fmt.Sprintf("Unable to get PR status and component versions from pinning PR #%d", pr.Target.PR)
			if metadata.Error != "" {
				summary.PRProblems = []string{metadata.Error}
			} else {
				summary.PRSummary = fmt.Sprintf("Pending: reading pinning PR #%d metadata", pr.Target.PR)
				summary.PRProblems = nil
			}
			summary.PRReason = strings.Join(summary.PRProblems, "; ")
			return summary
		}
	} else if !pr.Evidence.Fresh() {
		summary.PRSummary = fmt.Sprintf("Unable to get PR status and component versions from pinning PR #%d", pr.Target.PR)
		if pr.Evidence.Error != "" {
			summary.PRProblems = []string{pr.Evidence.Error}
		} else {
			summary.PRSummary = fmt.Sprintf("Pending: reading pinning PR #%d metadata", pr.Target.PR)
			summary.PRProblems = nil
		}
		summary.PRReason = strings.Join(summary.PRProblems, "; ")
		return summary
	}
	if pr.Target.Repo != in.Config.ACS().Repo || pr.Target.PR <= 0 || pr.Base != in.Config.Branch {
		summary.PRStatus = No
		summary.PRSummary = "Pinning PR target does not match configuration."
		summary.PRProblems = []string{fmt.Sprintf("Expected %s on %s; got %s on %s", in.Config.ACS().Repo, in.Config.Branch, pr.Target.Repo, pr.Base)}
		summary.PRReason = strings.Join(summary.PRProblems, "; ")
		return summary
	}
	if pr.Merged || strings.EqualFold(pr.State, "merged") {
		summary.PRStatus = summary.BranchStatus
		switch summary.PRStatus {
		case Yes:
			summary.PRSummary = "Merged; actual ACS branch pins match selected versions."
		case No:
			summary.PRSummary = "Merged."
		default:
			summary.PRSummary = "Merged."
		}
		summary.PRReason = summary.PRSummary + " " + summary.BranchReason
		summary.PRProblems = nil
		return summary
	}
	if strings.EqualFold(pr.State, "closed") {
		summary.PRStatus = No
		summary.PRSummary = "Closed without merge."
		summary.PRReason = summary.PRSummary
		return summary
	}
	if !strings.EqualFold(pr.State, "open") {
		summary.PRSummary = fmt.Sprintf("Pending: reading pinning PR #%d status", pr.Target.PR)
		summary.PRProblems = nil
		summary.PRReason = strings.Join(summary.PRProblems, "; ")
		return summary
	}
	summary.PROpen = true
	if !acs.PinningPins.Evidence.Fresh() {
		summary.PRSummary = "Pending: reading proposed component version files"
		if acs.PinningPins.Evidence.Error != "" {
			summary.PRSummary = fmt.Sprintf("Unable to get %s from pinning PR #%d", unreadPinFiles(acs.PinningPins, in.Config.Components), pr.Target.PR)
			summary.PRProblems = pinReadProblems(acs.PinningPins, in.Config.Components)
			if len(summary.PRProblems) == 0 {
				summary.PRProblems = []string{acs.PinningPins.Evidence.Error}
			}
		} else {
			summary.PRProblems = nil
		}
		summary.PRReason = strings.Join(summary.PRProblems, "; ")
		return summary
	}
	proposedStatus := comparePins(acs.PinningPins.Versions, comparisonVersions, in.Config.Components)
	if proposedStatus == No {
		summary.PRStatus = No
		summary.PRSummary = "Pinning PR versions differ from selected component versions."
		for _, source := range in.Config.Components {
			actual, selected := acs.PinningPins.Versions[source.Name], comparisonVersions[source.Name]
			if actual != "" && selected != "" && actual != selected {
				summary.PRProblems = append(summary.PRProblems, fmt.Sprintf("The pinning PR sets %s to %s; your selected %s version is %s.", pinFile(acs.PinningPins, source.Name), actual, componentTitle(source.Name), selected))
			}
		}
		summary.PRReason = strings.Join(summary.PRProblems, " ")
		return summary
	}
	if proposedStatus == "" {
		summary.PRSummary = "Pinning PR comparison: component tag selection required."
		if missing := missingVersionFiles(acs.PinningPins, in.Config.Components); missing != "" {
			summary.PRSummary = fmt.Sprintf("Unable to get %s from pinning PR #%d", missing, pr.Target.PR)
		}
		summary.PRProblems, summary.onlyMissingTags = missingPinProblems(acs.PinningPins, in.Config.Components, comparisonVersions, in.Choices.Tags)
		summary.PRReason = strings.Join(summary.PRProblems, "; ")
		return summary
	}
	summary.PRSummary = "The pinning PR sets all selected component versions; wait for it to merge."
	summary.PRReason = summary.PRSummary
	return summary
}

func missingVersionFiles(pins Pins, sources []Source) string {
	var names []string
	for _, source := range sources {
		if pins.Failures[source.Name] != "" || pins.Versions[source.Name] == "" {
			names = append(names, pinFile(pins, source.Name))
		}
	}
	return strings.Join(names, ", ")
}

func unreadPinFiles(pins Pins, sources []Source) string {
	if missing := missingVersionFiles(pins, sources); missing != "" {
		return missing
	}
	var names []string
	for _, source := range sources {
		names = append(names, pinFile(pins, source.Name))
	}
	return strings.Join(names, ", ")
}

func missingPinProblems(pins Pins, sources []Source, selected, choices map[string]string) ([]string, bool) {
	problems := []string{}
	missingTags := []string{}
	for _, source := range sources {
		if selected[source.Name] == "" {
			if choices[source.Name] == "" {
				missingTags = append(missingTags, componentTitle(source.Name))
			} else {
				problems = append(problems, fmt.Sprintf("Selected %s tag %q does not provide a final %s version", componentTitle(source.Name), choices[source.Name], source.Stream))
			}
		}
		if pins.Versions[source.Name] == "" {
			if failure := pins.Failures[source.Name]; failure != "" {
				problems = append(problems, pinFile(pins, source.Name)+": "+failure)
			} else {
				problems = append(problems, pinFile(pins, source.Name)+" is empty or missing")
			}
		}
	}
	if len(missingTags) > 0 {
		return append([]string{"Choose " + joinComponentTitles(missingTags) + " tags"}, problems...), len(problems) == 0
	}
	return problems, false
}

func pinReadProblems(pins Pins, sources []Source) []string {
	var problems []string
	for _, source := range sources {
		if failure := pins.Failures[source.Name]; failure != "" {
			problems = append(problems, pinFile(pins, source.Name)+": "+failure)
		} else if pins.Versions[source.Name] == "" && pins.Evidence.Error != "" {
			problems = append(problems, pinFile(pins, source.Name)+": "+pins.Evidence.Error)
		}
	}
	return problems
}

func pinFile(pins Pins, component string) string {
	if filename := pins.Paths[component]; filename != "" {
		return filename
	}
	return map[string]string{"fact": "FACT_VERSION", "collector": "COLLECTOR_VERSION", "scanner": "SCANNER_VERSION"}[component]
}

func componentTitle(component string) string {
	if component == "" {
		return "Component"
	}
	return strings.ToUpper(component[:1]) + component[1:]
}

func joinComponentTitles(components []string) string {
	return strings.Join(components, " and ")
}

func comparePins(actual, selected map[string]string, sources []Source) Truth {
	unknown, mismatch := false, false
	for _, source := range sources {
		want := selected[source.Name]
		got, exists := actual[source.Name]
		if want == "" || !exists {
			unknown = true
			continue
		}
		if got != want {
			mismatch = true
		}
	}
	if mismatch {
		return No
	}
	if unknown {
		return ""
	}
	return Yes
}

func assessPinningPR(a *Assessment, in Input, versions map[string]string, tagCoverage map[string]Condition, imageStatus Truth) {
	acs, found := findRepository(in.Repositories, "acs")
	if !found || !acs.Evidence.Fresh() {
		return
	}
	prProblem := a.Pins.PRSummary
	if len(a.Pins.PRProblems) > 0 {
		prProblem += ": " + strings.Join(a.Pins.PRProblems, "; ")
	}
	if in.Config.PinningPR > 0 && (acs.PinningPR == nil || acs.PinningPR.Target.PR != in.Config.PinningPR) {
		a.ACSRecommendations = append(a.ACSRecommendations, prProblem)
		return
	}
	if acs.PinningPR == nil {
		if imageStatus != Yes || !pinningReady(in, versions, tagCoverage) {
			return
		}
		if !acs.Pins.Evidence.Fresh() {
			return
		}
		if pinsMatch(acs.Pins.Versions, versions, in.Config.Components) {
			return
		}
		if pinningReady(in, versions, tagCoverage) {
			commands := pinningCommands(in.Config, acs.Pins.Paths, acs.Pins.Versions, versions, 0)
			if commands == "" {
				a.ACSRecommendations = append(a.ACSRecommendations, "Component versions and both image families are ready, but verified pin paths or current pin values are incomplete; refresh ACS pin discovery before generating update commands.")
			} else {
				a.ACSRecommendations = append(a.ACSRecommendations, "Component versions and both image families are ready; create an ACS pinning PR using the generated commands.")
				a.Commands = commands
			}
		}
		return
	}
	pr := *acs.PinningPR
	if pr.Target.Repo != in.Config.ACS().Repo || pr.Target.PR <= 0 || pr.Base != in.Config.Branch {
		a.ACSRecommendations = append(a.ACSRecommendations, prProblem)
		return
	}
	if strings.EqualFold(pr.State, "open") && !pr.Merged {
		if a.Pins.onlyMissingTags {
			return
		}
		a.ACSRecommendations = append(a.ACSRecommendations, prProblem)
		if !a.Pins.PROpen || !acs.PinningPins.Evidence.Fresh() {
			return
		}
		if pinsMatch(acs.PinningPins.Versions, versions, in.Config.Components) {
			return
		}
		if imageStatus == Yes && pinningReady(in, versions, tagCoverage) {
			commands := pinningCommands(in.Config, acs.Pins.Paths, acs.PinningPins.Versions, versions, pr.Target.PR)
			if commands == "" {
				a.ACSRecommendations = append(a.ACSRecommendations, "Update the existing pinning PR only after refreshing its per-file versions and verified paths; safe version edit commands cannot be generated from current evidence.")
			} else {
				a.ACSRecommendations = append(a.ACSRecommendations, "Update the existing open ACS pinning PR with the current confirmed component versions.")
				a.Commands = commands
			}
		}
		return
	}
	if acs.Pins.Evidence.Fresh() && pinsMatch(acs.Pins.Versions, versions, in.Config.Components) {
		return
	}
	if (pr.Merged || strings.EqualFold(pr.State, "closed")) && pr.Evidence.Fresh() && imageStatus == Yes && pinningReady(in, versions, tagCoverage) {
		commands := pinningCommands(in.Config, acs.Pins.Paths, acs.Pins.Versions, versions, 0)
		if commands != "" {
			a.ACSRecommendations = append(a.ACSRecommendations, "The supplied pinning PR is merged; create a new ACS pinning PR if the actual branch pins still need the current component versions.")
			a.Commands = commands
		}
	}
}

func addTagRecommendations(a *Assessment, in Input, included []Target, prStates map[Target]Truth, tagCoverage map[string]Condition) {
	for _, source := range in.Config.Components {
		a.ReleaseGuides[source.Name] = componentReleaseGuide(source.Name)
		coverage := tagCoverage[source.Name]
		if coverage.Status == Yes {
			continue
		}
		confirmed := in.Choices.Tags[source.Name]
		proposed := a.ProposedTags[source.Name]
		proposalStatus := Truth("")
		if proposed != "" {
			if repo, ok := findRepository(in.Repositories, source.Name); ok {
				if tag, ok := findTag(repo.Tags, proposed); ok {
					proposalStatus = tagSuitability(source, repo, tag, included, prStates).Status
				}
			}
		}
		if hasOpenIncludedPR(source, in.Repositories, included, prStates) {
			a.ComponentReleaseRecommendations = append(a.ComponentReleaseRecommendations, fmt.Sprintf("Wait for the included %s PRs to merge, then select an existing tag that covers their merge commits or cut a new tag and trigger its component release using the release guide.", source.Name))
			continue
		}
		if coverage.Status == "" {
			if confirmed == "" && proposed != "" {
				a.ComponentReleaseRecommendations = append(a.ComponentReleaseRecommendations, tagProposalText(source, proposed, proposalStatus, in.Config.Version))
			} else if confirmed == "" {
				a.ComponentReleaseRecommendations = append(a.ComponentReleaseRecommendations, fmt.Sprintf("No %s tag is selected yet. After reviewing component PRs, select a suitable existing %s-stream tag or cut a new tag and trigger its component release using the release guide.", source.Name, source.Stream))
			} else {
				a.ComponentReleaseRecommendations = append(a.ComponentReleaseRecommendations, fmt.Sprintf("%s tag selection needs verification. %s.", componentTitle(source.Name), coverage.Reason))
			}
			continue
		}
		recommendation := fmt.Sprintf("%s tag selection needs attention. %s.", componentTitle(source.Name), coverage.Reason)
		switch {
		case proposed != "" && proposed != confirmed && proposalStatus == Yes:
			recommendation += fmt.Sprintf(" Review and confirm eligible tag %s.", proposed)
		case proposed != "" && proposed != confirmed:
			recommendation += " " + tagProposalText(source, proposed, proposalStatus, in.Config.Version)
		default:
			recommendation += fmt.Sprintf(" Cut a covering tag on %s and trigger its component release using the release guide, then select the new tag.", source.Branch)
		}
		a.ComponentReleaseRecommendations = append(a.ComponentReleaseRecommendations, recommendation)
	}
}

func tagProposalText(source Source, tag string, suitability Truth, acsVersion string) string {
	switch suitability {
	case Yes:
		return fmt.Sprintf("Select %s tag to include in ACS %s.", componentTitle(source.Name), acsVersion)
	case No:
		return fmt.Sprintf("Tag %s is the highest final %s-stream tag observed on the branch, but it does not cover the selected merged changes. Cut a covering tag on %s and trigger its component release using the release guide, then select it at https://github.com/%s/tags.", tag, source.Name, source.Branch, source.Repo)
	default:
		return fmt.Sprintf("Tag %s is the highest final %s-stream tag observed on the branch, but its coverage of selected changes is unknown. Refresh ancestry and merge containment before confirming it.", tag, source.Name)
	}
}

func hasOpenIncludedPR(source Source, repositories []Repository, included []Target, prStates map[Target]Truth) bool {
	for _, target := range included {
		if target.Repo == source.Repo && target.PR > 0 && prStates[target] == No {
			for _, repository := range repositories {
				if repository.Source.Name != source.Name {
					continue
				}
				for _, pr := range repository.PRs {
					if pr.Target == target && strings.EqualFold(pr.State, "open") {
						return true
					}
				}
			}
		}
	}
	return false
}

func pinningReady(in Input, versions map[string]string, tagCoverage map[string]Condition) bool {
	for _, source := range in.Config.Components {
		if in.Choices.Tags[source.Name] == "" || versions[source.Name] == "" || tagCoverage[source.Name].Status != Yes {
			return false
		}
	}
	return true
}

func pinsMatch(actual, versions map[string]string, sources []Source) bool {
	for _, source := range sources {
		if actual[source.Name] == "" || actual[source.Name] != versions[source.Name] {
			return false
		}
	}
	return true
}

func pinningCommands(config Config, paths, current, versions map[string]string, updatePR int) string {
	if config.Validate() != nil {
		return ""
	}
	var files []string
	for _, source := range config.Components {
		file := paths[source.Name]
		if !safePinPath(file) || !isFinalVersion(versions[source.Name]) || !pinValueRE.MatchString(current[source.Name]) {
			return ""
		}
		files = append(files, file)
	}
	sort.Strings(files)
	var b strings.Builder
	b.WriteString("(\nset -eu\ncdrox\n")
	b.WriteString("if test -n \"$(git status --porcelain)\"; then echo 'Working tree is not clean; stop.' >&2; exit 1; fi\n")
	if updatePR > 0 {
		fmt.Fprintf(&b, "gh pr checkout %d --repo %s\n", updatePR, config.ACS().Repo)
	} else {
		b.WriteString("git switch -c piotr/acs-" + config.Version + "-pinning " + config.Branch + "\n")
	}
	b.WriteString("python3 - <<'PY'\nfrom pathlib import Path\nupdates = [\n")
	for _, source := range config.Components {
		fmt.Fprintf(&b, "    ('%s', '%s', '%s'),\n", paths[source.Name], current[source.Name], versions[source.Name])
	}
	b.WriteString("]\nchanges = []\nfor name, old, new in updates:\n    path = Path(name)\n    text = path.read_text()\n    if text.count(old) != 1:\n        raise SystemExit(f'{name}: expected exactly one current pin {old!r}')\n    changes.append((path, text.replace(old, new, 1)))\nfor path, text in changes:\n    path.write_text(text)\nPY\n")
	b.WriteString("git add --")
	for _, file := range files {
		b.WriteString(" '" + file + "'")
	}
	b.WriteString("\ngit commit -m 'Update ACS component pins for " + config.Version + "'\n)\n")
	return b.String()
}

func isFinalVersion(value string) bool {
	_, _, _, ok := Version(value)
	return ok
}

func safePinPath(file string) bool {
	return file != "" && !path.IsAbs(file) && path.Clean(file) == file && file != "." && file != ".." && !strings.HasPrefix(file, "../") && !strings.ContainsAny(file, "'\n\r;$`\\")
}

func noOutstandingACSPR(repo Repository) bool {
	if !repo.Evidence.Fresh() {
		return false
	}
	for _, pr := range repo.PRs {
		if strings.EqualFold(pr.State, "open") {
			return false
		}
	}
	return true
}

func nonNilEvidence(evidence []Evidence) []Evidence {
	if len(evidence) == 0 {
		return nil
	}
	return evidence
}

// componentReleaseGuide links verified upstream instructions without initiating releases.
func componentReleaseGuide(name string) string {
	switch name {
	case "fact":
		return "https://github.com/stackrox/fact/blob/3d801bc53b40cbef966d5daca6ef3aa635d5de00/docs/release.md"
	case "collector":
		return "https://github.com/stackrox/collector/blob/129987e7488e0c4084290cb6f40e2aa04ba27065/docs/release.md"
	case "scanner":
		return "https://github.com/stackrox/scanner/blob/c4a592236e08ecfb7b98be29c70d698709e1585b/README.md#release-process"
	default:
		return ""
	}
}
