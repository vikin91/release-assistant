package web

import (
	"net/url"
	"slices"
	"strconv"
	"strings"

	"release-assistant/internal/app"
	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

type (
	pageModel struct {
		Title, CSRF, Error   string
		Configured           bool
		Snapshot             app.Snapshot
		Repos                []repoModel
		TagRepos             []repoModel
		TagsSelected         bool
		TagUpdates           bool
		Problems             []release.Condition
		Plans                []planModel
		Authority            []authorityModel
		ComponentImages      []imageModel
		ReleaseImages        []imageModel
		ACS                  *release.Repository
		PinPRState           string
		PinningPR            *prModel
		PinningCanGrant      bool
		PinningHealth        release.Truth
		PinningFailures      []release.Check
		PinningRetests       []release.Proposal
		PinningPending       int
		PinningImages        []pinningImageModel
		PinningBuildProblems []string
		History              []historyModel
		ConfigForm           url.Values
		ConfiguredSources    map[string]release.Source
		ConfigDraft          bool
		ACS4                 bool
	}
	repoModel struct {
		Repository  release.Repository
		PRs         []prModel
		Tags        []tagModel
		OtherTags   []tagModel
		CurrentTag  string
		SelectedTag *tagModel
		NewerTag    string
	}
	prModel struct {
		PR          release.PR
		Choice      release.Choice
		ChoiceLabel string
		Authority   *dispatch.View
	}
	tagModel struct {
		Tag               release.Tag
		Current, Proposed bool
		Authority         *dispatch.View
		Component, Repo   string
		CSRF              string
		Generation        uint64
		Switch            bool
		WritesEnabled     bool
		CoverageStatus    release.Truth
		CoverageReason    string
		MergeCoverage     []mergeCoverageModel
	}
	mergeCoverageModel struct {
		SHA    string
		Status release.Truth
	}
	imageModel struct {
		Check          release.ImageStatus
		Name, Tag, URL string
	}
	pinningImageModel struct {
		Job, URL string
		Image    release.Image
		Status   release.Truth
		Reason   string
	}
	planModel struct {
		Plan    release.Plan
		Pinning bool
	}
	authorityModel struct{ View dispatch.View }
	historyModel   struct{ Cycle app.Cycle }
)

func makePage(snapshot app.Snapshot, csrf, errText string) pageModel {
	model := pageModel{
		CSRF: csrf, Error: errText, Snapshot: snapshot,
		ConfiguredSources: make(map[string]release.Source),
		ConfigForm: url.Values{
			"version": {snapshot.Config.Version}, "branch": {snapshot.Config.Branch}, "pinning_pr": {""},
			"fact_stream": {"0.4"}, "collector_stream": {"3.26"}, "scanner_stream": {"2.40"},
		},
	}
	model.Configured = snapshot.Config.Validate() == nil
	model.TagsSelected = allTagsSelected(snapshot)
	major, _, _, ok := release.Version(snapshot.Config.Version)
	model.ACS4 = ok && major == 4
	if snapshot.Config.PinningPR != 0 {
		model.ConfigForm.Set("pinning_pr", strconv.Itoa(snapshot.Config.PinningPR))
	}
	for _, source := range snapshot.Config.Components {
		model.ConfiguredSources[source.Name] = source
		model.ConfigForm.Set(source.Name+"_repo", source.Repo)
		if source.Stream != "" {
			model.ConfigForm.Set(source.Name+"_stream", source.Stream)
		}
	}
	setComponentBranches(model.ConfigForm, snapshot.Config)
	model.Title = "ACS Release Assistant"
	if snapshot.Config.Version != "" {
		model.Title = "ACS " + snapshot.Config.Version + " Release Assistant"
	}
	if model.Error == "" {
		if snapshot.Error != "" {
			model.Error = "Some checks could not complete. See the affected items below."
		}
	}
	if snapshot.LogError != "" {
		if model.Error != "" {
			model.Error += " · "
		}
		model.Error += "Action log: " + snapshot.LogError
	}
	retryTargets := make(map[release.Target]bool)
	pinTarget := snapshot.Config.PinningTarget()
	if pinTarget.PR > 0 && snapshot.Choices.PRs[pinTarget] != release.Ignored {
		retryTargets[pinTarget] = true
	}
	for _, source := range snapshot.Config.Components {
		if tag := snapshot.Choices.Tags[source.Name]; tag != "" {
			retryTargets[release.Target{Repo: source.Repo, Tag: tag}] = true
		}
	}
	for target, choice := range snapshot.Choices.PRs {
		if choice == release.Included && target.PR > 0 {
			retryTargets[target] = true
		}
	}
	inactivePRs := make(map[release.Target]bool)
	for _, repo := range snapshot.Repositories {
		if repo.Source.Name == "acs" {
			model.ACS = new(repo)
			if repo.PinningPR != nil && repo.PinningPR.Target == pinTarget {
				model.PinningPR = &prModel{PR: *repo.PinningPR, Choice: snapshot.Choices.PRs[pinTarget]}
				if repo.PinningPR.State != "open" || repo.PinningPR.Merged || repo.PinningPR.Base != snapshot.Config.Branch {
					inactivePRs[pinTarget] = true
				}
			}
		}
		for _, pr := range repo.PRs {
			closed := pr.State == "closed" || pr.State == "merged"
			if pr.Merged || closed {
				inactivePRs[pr.Target] = true
			}
		}
	}
	authorities := make(map[release.Target]dispatch.View, len(snapshot.Authority))
	for _, v := range snapshot.Authority {
		if v.Ignored || inactivePRs[v.Target] {
			continue
		}
		authorities[v.Target] = v
		if retryTargets[v.Target] {
			model.Authority = append(model.Authority, authorityModel{View: v})
		}
	}
	if model.PinningPR != nil {
		p := model.PinningPR.PR
		model.PinningHealth = p.ChecksStatus()
		for _, check := range p.Checks {
			if check.Required == release.No {
				continue
			}
			switch check.Result() {
			case release.No:
				model.PinningFailures = append(model.PinningFailures, check)
			case "":
				model.PinningPending++
			}
		}
		if authority, exists := authorities[pinTarget]; exists {
			model.PinningPR.Authority = &authority
		}
		model.PinningCanGrant = snapshot.WritesEnabled && p.Evidence.Fresh() && p.SHA != "" && p.State == "open" && !p.Merged && !p.OptOut && p.Base == snapshot.Config.Branch && model.PinningPR.Choice != release.Ignored
	}
	for _, repo := range snapshot.Repositories {
		r := repoModel{Repository: repo, CurrentTag: snapshot.Choices.Tags[repo.Source.Name]}
		for _, pr := range repo.PRs {
			if pr.State != "open" || pr.Merged || pr.Target == pinTarget {
				continue
			}
			choice := snapshot.Choices.PRs[pr.Target]
			if choice == release.Ignored {
				continue
			}
			label := string(choice)
			if label == "" {
				label = "undecided"
			}
			var authority *dispatch.View
			if v, ok := authorities[pr.Target]; ok {
				copy := v
				authority = &copy
			}
			r.PRs = append(r.PRs, prModel{PR: pr, Choice: choice, ChoiceLabel: label, Authority: authority})
		}
		for _, tag := range repo.Tags {
			var authority *dispatch.View
			if v, ok := authorities[release.Target{Repo: repo.Source.Repo, Tag: tag.Name}]; ok {
				copy := v
				authority = &copy
			}
			t := tagModel{
				Tag: tag, Current: tag.Name == r.CurrentTag,
				Proposed: snapshot.Assessment.ProposedTags[repo.Source.Name] == tag.Name, Authority: authority,
				Component: repo.Source.Name, Repo: repo.Source.Repo, CSRF: csrf, Generation: snapshot.Generation,
				WritesEnabled:  snapshot.WritesEnabled,
				Switch:         r.CurrentTag != "" && tag.Name != r.CurrentTag,
				CoverageReason: "Tag coverage observations are unavailable.",
			}
			for _, pr := range repo.PRs {
				if snapshot.Choices.PRs[pr.Target] == release.Included && pr.Merged && pr.MergeSHA != "" {
					t.MergeCoverage = append(t.MergeCoverage, mergeCoverageModel{SHA: pr.MergeSHA, Status: tag.Contains[pr.MergeSHA]})
				}
			}
			if t.Current {
				for _, condition := range snapshot.Assessment.Conditions {
					if condition.Name == "tag "+repo.Source.Name {
						t.CoverageStatus, t.CoverageReason = condition.Status, condition.Reason
						break
					}
				}
				r.SelectedTag = &t
			}
			if t.Proposed && newerTag(t.Tag.Name, r.CurrentTag) {
				r.NewerTag = t.Tag.Name
				model.TagUpdates = true
			}
			switch {
			case t.Proposed:
				r.Tags = slices.Insert(r.Tags, 0, t)
			case t.Current:
				r.Tags = append(r.Tags, t)
			default:
				r.OtherTags = append(r.OtherTags, t)
			}
		}
		model.Repos = append(model.Repos, r)
		if repo.Source.Name != "acs" {
			model.TagRepos = append(model.TagRepos, r)
		}
	}
	for _, condition := range snapshot.Assessment.Conditions {
		ownSummary := condition.Name == "configuration" || condition.Name == "component images" || condition.Name == "ACS release branch pins" ||
			strings.HasPrefix(condition.Name, "tag ") || strings.HasPrefix(condition.Name, "image ")
		if !ownSummary && condition.Status != release.Yes {
			model.Problems = append(model.Problems, condition)
		}
	}
	model.PinPRState = "Pending"
	if len(snapshot.Assessment.Pins.PRProblems) > 0 {
		model.PinPRState = "Read failed"
	}
	if model.ACS != nil && model.ACS.PinningPR != nil && model.ACS.PinningPR.Target.PR == snapshot.Assessment.Pins.PRNumber && model.ACS.PinningPR.Evidence.Fresh() {
		pr := model.ACS.PinningPR
		switch {
		case pr.Merged:
			model.PinPRState = "Merged"
		case pr.State == "open":
			model.PinPRState = "Open"
		case pr.State == "closed":
			model.PinPRState = "Closed"
		}
	}
	for _, check := range snapshot.Assessment.Images {
		row := imageModel{Check: check, Name: check.Image.Name}
		if index := strings.LastIndexByte(check.Image.Reference, ':'); index >= 0 {
			row.Tag = check.Image.Reference[index+1:]
			if row.Name == "" {
				repository := check.Image.Reference[:index]
				row.Name = repository[strings.LastIndexByte(repository, '/')+1:]
			}
			link, err := url.Parse(check.Image.Evidence.URL)
			if err == nil && link.Host == "quay.io" && strings.HasPrefix(link.Path, "/repository/") && link.Query().Get("tag") == row.Tag {
				row.URL = link.String()
			}
		}
		if check.Image.Component == "acs" {
			model.ReleaseImages = append(model.ReleaseImages, row)
		} else {
			model.ComponentImages = append(model.ComponentImages, row)
		}
	}
	for _, plan := range snapshot.Plans {
		if plan.Target == pinTarget && model.PinningPR != nil {
			if plan.Evidence.Error != "" {
				model.PinningBuildProblems = append([]string{plan.Evidence.Error}, plan.Warnings...)
			}
			pr := model.PinningPR.PR
			if plan.SHA == pr.SHA && plan.Evidence.Fresh() {
				model.PinningRetests = plan.Proposals
			}
			for _, run := range plan.Runs {
				if run.Image.Reference == "" {
					continue
				}
				row := pinningImageModel{Job: run.Job, URL: run.URL, Image: run.Image, Reason: run.Image.Evidence.Error}
				current := pr.Evidence.Fresh() && pr.State == "open" && !pr.Merged && pr.Base == snapshot.Config.Branch && pr.SHA == plan.SHA
				if current && run.Image.Evidence.Fresh() {
					row.Status = run.Image.Present
				} else if !current {
					row.Reason = pinningImageProblem(pr, plan, snapshot.Config.Branch)
				}
				model.PinningImages = append(model.PinningImages, row)
			}
		}
		if !retryTargets[plan.Target] || inactivePRs[plan.Target] {
			continue
		}
		model.Plans = append(model.Plans, planModel{Plan: plan, Pinning: plan.Target == pinTarget})
	}
	for _, cycle := range snapshot.History {
		model.History = append(model.History, historyModel{Cycle: cycle})
	}
	return model
}

func allTagsSelected(snapshot app.Snapshot) bool {
	if len(snapshot.Config.Components) == 0 {
		return false
	}
	for _, source := range snapshot.Config.Components {
		if snapshot.Choices.Tags[source.Name] == "" {
			return false
		}
	}
	return true
}

func newerTag(candidate, current string) bool {
	cm, cn, cp, candidateOK := release.Version(strings.TrimPrefix(candidate, "v"))
	m, n, p, currentOK := release.Version(strings.TrimPrefix(current, "v"))
	return candidateOK && currentOK && slices.Compare([]int{cm, cn, cp}, []int{m, n, p}) > 0
}

// componentBranch preserves configured CLI overrides for an unchanged component scope.
func componentBranch(config release.Config, name, repo, stream string) string {
	for _, source := range config.Components {
		if source.Name == name && source.Repo == repo && (source.Stream == stream || source.Stream == "") && source.Branch != "" {
			return source.Branch
		}
	}
	if stream == "" {
		return ""
	}
	return "release-" + stream
}

func setComponentBranches(form url.Values, config release.Config) {
	for _, name := range []string{"fact", "collector", "scanner"} {
		form.Set(name+"_branch", componentBranch(config, name, form.Get(name+"_repo"), form.Get(name+"_stream")))
	}
}

// pinVersion keeps failed reads distinct from pending requests and retained values.
func pinVersion(pins release.Pins, component string) string {
	if value := pins.Versions[component]; value != "" {
		if !pins.Evidence.Fresh() && len(pins.Failures) == 0 {
			return value + " (last observed)"
		}
		return value
	}
	if pins.Evidence.Fresh() {
		return "Missing"
	}
	if pins.Evidence.Error != "" || len(pins.Failures) > 0 {
		return "Unavailable"
	}
	return "Pending"
}

func pinningImageProblem(pr release.PR, plan release.Plan, branch string) string {
	switch {
	case pr.Evidence.Error != "":
		return "Unable to get the current pinning PR commit: " + pr.Evidence.Error
	case !pr.Evidence.Fresh():
		return "Pending: reading the current pinning PR commit."
	case pr.State != "open" || pr.Merged:
		return "Pinning PR is " + pr.State + "."
	case pr.Base != branch:
		return "PR targets " + pr.Base + "; expected " + branch + "."
	case plan.SHA == "":
		if plan.Evidence.Error != "" {
			return "Unable to get build results: " + plan.Evidence.Error
		}
		return "Pending build results."
	default:
		return "Build belongs to commit " + plan.SHA + "; current PR commit is " + pr.SHA + "."
	}
}
