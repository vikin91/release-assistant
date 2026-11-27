package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"time"

	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

const (
	dashboardStateVersion = 1
	dashboardSaveInterval = 2 * time.Second
	dashboardFileSuffix   = ".observations.json"
)

type (
	dashboardFile struct {
		Version  int                       `json:"version"`
		Profiles map[string]dashboardState `json:"profiles"`
	}

	dashboardState struct {
		CreatedPinningPR dispatch.PinningReceipt              `json:"createdPinningPR,omitzero"`
		Config           release.Config                       `json:"config"`
		Repositories     map[string]release.Repository        `json:"repositories,omitempty"`
		TagNames         map[string]ComponentTagNames         `json:"componentTagNames,omitempty"`
		Plans            map[string]release.Plan              `json:"plans,omitempty"`
		Images           map[string]dashboardImages           `json:"images,omitempty"`
		Vulnerabilities  map[string]release.RCVulnerabilities `json:"vulnerabilities,omitempty"`
	}

	dashboardImages struct {
		Tag, SHA, Version string
		Rows              []release.Image
	}
)

func dashboardPath(statePath string) string {
	return strings.TrimSuffix(statePath, ".json") + dashboardFileSuffix
}

func (s *Session) loadDashboards() {
	s.dashboards = dashboardFile{Version: dashboardStateVersion, Profiles: make(map[string]dashboardState)}
	statePath := s.options.ReleaseState.path
	path := dashboardPath(statePath)
	data, err := os.ReadFile(path)
	legacyPath := statePath + dashboardFileSuffix
	fromLegacy := errors.Is(err, os.ErrNotExist) && legacyPath != path
	if fromLegacy {
		data, err = os.ReadFile(legacyPath)
	}
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var file dashboardFile
	if err == nil {
		err = json.Unmarshal(data, &file)
	}
	if err == nil && file.Version != dashboardStateVersion {
		err = fmt.Errorf("unsupported observation version %d", file.Version)
	}
	if err != nil {
		s.options.ReleaseState.warnings = append(s.options.ReleaseState.warnings,
			fmt.Sprintf("Could not load saved dashboard observations: %v", err))
		return
	}
	for _, profile := range s.options.ReleaseState.file.Profiles {
		if saved, ok := file.Profiles[profile.ID]; ok && sameDashboardConfig(saved.Config, profile.Config) {
			s.dashboards.Profiles[profile.ID] = saved
		}
	}
	s.dashboardDirty = fromLegacy
}

func sameDashboardConfig(a, b release.Config) bool {
	return a.Version == b.Version && a.Branch == b.Branch && slices.Equal(a.Components, b.Components)
}

// runDashboardWriter serializes checkpoints and flushes after monitoring stops.
func (s *Session) runDashboardWriter(stop <-chan struct{}) {
	ticker := time.NewTicker(dashboardSaveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.flushDashboards()
		case <-stop:
			s.flushDashboards()
			return
		}
	}
}

func (s *Session) flushDashboards() {
	s.mu.Lock()
	if !s.dashboardDirty {
		s.mu.Unlock()
		return
	}
	file := s.dashboards
	// Profile updates replace their observations, so the nested values stay immutable.
	file.Profiles = maps.Clone(file.Profiles)
	s.dashboardDirty = false
	s.mu.Unlock()

	path := dashboardPath(s.options.ReleaseState.path)
	err := writeStateJSON(path, file, "")

	s.mu.Lock()
	defer s.mu.Unlock()
	message := ""
	if err != nil {
		s.dashboardDirty = true
		message = err.Error()
	}
	s.recordReadResult(release.Target{}, "dashboard-state", message, "saved dashboard observations")
}

func (s *Session) dashboardForUpdate() dashboardState {
	saved := s.dashboards.Profiles[s.activeProfile]
	if !sameDashboardConfig(saved.Config, s.config) {
		return dashboardState{Config: profileConfig(s.config)}
	}
	return saved
}

func (s *Session) storeDashboard(saved dashboardState) {
	if reflect.DeepEqual(saved, s.dashboards.Profiles[s.activeProfile]) {
		return
	}
	s.dashboards.Profiles[s.activeProfile] = saved
	s.dashboardDirty = true
}

// historicalEvidence keeps verification and attempt times separate on a display copy.
func historicalEvidence(saved, attempt release.Evidence) release.Evidence {
	if saved.At.IsZero() || !saved.Known {
		return saved
	}
	saved.Historical = true
	if attempt.Error != "" {
		at := attempt.AttemptAt
		if at.IsZero() {
			at = attempt.At
		}
		if !at.Before(saved.AttemptAt) {
			saved.Error, saved.AttemptAt = attempt.Error, at
		}
	}
	return saved
}

func dashboardPR(previous, current release.PR) release.PR {
	if !current.Evidence.Fresh() && previous.Evidence.Known && previous.SHA != "" &&
		(current.SHA == "" || current.SHA == previous.SHA) {
		previous.Evidence = historicalEvidence(previous.Evidence, current.Evidence)
		previous.ChecksEvidence = historicalEvidence(previous.ChecksEvidence, current.ChecksEvidence)
		previous.CheckResultsEvidence = historicalEvidence(previous.CheckResultsEvidence, current.CheckResultsEvidence)
		return previous
	}
	if current.SHA != "" && current.SHA == previous.SHA && current.Base == previous.Base &&
		current.State == previous.State && current.State == "open" {
		for _, evidence := range []struct {
			current *release.Evidence
			saved   release.Evidence
		}{
			{&current.CheckResultsEvidence, previous.CheckResultsEvidence},
			{&current.ChecksEvidence, previous.ChecksEvidence},
		} {
			if !evidence.current.Fresh() && evidence.saved.Known {
				if !current.CheckResultsEvidence.Fresh() {
					current.Checks = previous.Checks
				}
				*evidence.current = historicalEvidence(evidence.saved, *evidence.current)
			}
		}
	}
	return current
}

func dashboardPins(previous, current release.Pins) release.Pins {
	if !current.Evidence.Fresh() && previous.Evidence.Known {
		previous.Evidence = historicalEvidence(previous.Evidence, current.Evidence)
		return previous
	}
	return current
}

func dashboardRepository(previous, current release.Repository, completeTags bool, problem string) release.Repository {
	result := cloneRepository(current)
	previous = cloneRepository(previous)
	if result.Source.Name == "" {
		result.Source = previous.Source
	}
	attempt := current.Evidence
	if problem != "" && attempt.Error == "" {
		attempt.Error = problem
		attempt.At = time.Now()
	}
	if !current.Evidence.Fresh() && previous.Evidence.Known && (current.Head == "" || current.Head == previous.Head) {
		result.Head = previous.Head
		result.Evidence = historicalEvidence(previous.Evidence, attempt)
	}
	if current.PinDiscovery.Evidence.At.IsZero() && previous.PinDiscovery.Evidence.Known {
		result.PinDiscovery = previous.PinDiscovery
		result.PinDiscovery.Evidence = historicalEvidence(previous.PinDiscovery.Evidence, current.PinDiscovery.Evidence)
	}
	if !current.PRListEvidence.Fresh() && previous.PRListEvidence.Known {
		result.PRListEvidence = historicalEvidence(previous.PRListEvidence, current.PRListEvidence)
	}
	for _, old := range previous.PRs {
		index := slices.IndexFunc(result.PRs, func(pr release.PR) bool { return pr.Target == old.Target })
		if index >= 0 {
			result.PRs[index] = dashboardPR(old, result.PRs[index])
		} else {
			result.PRs = append(result.PRs, dashboardPR(old, release.PR{Evidence: attempt}))
		}
	}
	for _, old := range previous.Tags {
		index := slices.IndexFunc(result.Tags, func(tag release.Tag) bool { return tag.Name == old.Name })
		if index >= 0 {
			tag := &result.Tags[index]
			if !tag.Evidence.Fresh() && (tag.SHA == "" || tag.SHA == old.SHA) && old.Evidence.Known {
				old.Evidence = historicalEvidence(old.Evidence, tag.Evidence)
				*tag = old
			}
		} else if !completeTags {
			old.Evidence = historicalEvidence(old.Evidence, attempt)
			result.Tags = append(result.Tags, old)
		}
	}
	oldRC, newRC := previous.Candidate, current.Candidate
	if !newRC.Evidence.Fresh() && oldRC.Evidence.Known {
		oldRC.Evidence = historicalEvidence(oldRC.Evidence, newRC.Evidence)
		result.Candidate = oldRC
	} else if (newRC.PRsPending || newRC.PRError != "") && newRC.Ahead > 0 &&
		newRC.Target == oldRC.Target && newRC.SHA != "" && newRC.SHA == oldRC.SHA && (len(oldRC.PRs) > 0 || len(oldRC.CommitsWithoutPR) > 0) {
		result.Candidate.PRs = oldRC.PRs
		result.Candidate.CommitsWithoutPR = oldRC.CommitsWithoutPR
		result.Candidate.PRsHistorical = oldRC.PRsHistorical || !sameCandidateRange(oldRC, newRC)
	}
	if !current.PreviousPins.Evidence.Fresh() && previous.PreviousPins.Evidence.Known {
		result.PreviousPublished = previous.PreviousPublished
	}
	result.PreviousPins = dashboardPins(previous.PreviousPins, current.PreviousPins)
	if current.Head == "" || current.Head == previous.Head {
		result.Pins = dashboardPins(previous.Pins, current.Pins)
	}
	if current.PinningPR == nil && !current.PinningEvidence.Fresh() && previous.PinningPR != nil {
		result.PinningPR = new(dashboardPR(*previous.PinningPR, release.PR{Evidence: current.PinningEvidence}))
		result.PinningEvidence = historicalEvidence(previous.PinningEvidence, current.PinningEvidence)
		result.PinningPins = dashboardPins(previous.PinningPins, current.PinningPins)
	} else if current.PinningPR != nil && previous.PinningPR != nil && current.PinningPR.Target == previous.PinningPR.Target {
		result.PinningPR = new(dashboardPR(*previous.PinningPR, *current.PinningPR))
		if current.PinningPR.SHA == previous.PinningPR.SHA && current.PinningPR.MergeSHA == previous.PinningPR.MergeSHA {
			result.PinningPins = dashboardPins(previous.PinningPins, current.PinningPins)
		}
	}
	return result
}

func (s *Session) observeDashboardRepo(repo release.Repository, completeTags bool, problem string) {
	if s.activeProfile == "" {
		return
	}
	saved := s.dashboardForUpdate()
	repos := make(map[string]release.Repository, len(saved.Repositories)+1)
	maps.Copy(repos, saved.Repositories)
	repos[repo.Source.Name] = dashboardRepository(repos[repo.Source.Name], repo, completeTags, problem)
	saved.Repositories = repos
	images := maps.Clone(saved.Images)
	if binding, ok := images[repo.Source.Name]; ok {
		if dashboardTagChanged(repo, binding.Tag, binding.SHA, completeTags) {
			delete(images, repo.Source.Name)
		}
	}
	saved.Images = images
	saved.Plans = maps.Clone(saved.Plans)
	maps.DeleteFunc(saved.Plans, func(_ string, plan release.Plan) bool {
		if plan.Target.Repo != repo.Source.Repo {
			return false
		}
		if plan.Target.Tag != "" {
			return dashboardTagChanged(repo, plan.Target.Tag, plan.SHA, completeTags)
		}
		return slices.ContainsFunc(repo.PRs, func(pr release.PR) bool {
			return pr.Target == plan.Target && pr.Evidence.Fresh() &&
				(pr.State != "open" || pr.Base != repo.Source.Branch || pr.SHA != plan.SHA)
		})
	})
	s.storeDashboard(saved)
}

func dashboardTagChanged(repo release.Repository, name, sha string, complete bool) bool {
	if repo.Source.Name == "acs" {
		rc := repo.Candidate
		return rc.Evidence.Fresh() && (rc.Target.Tag != name || rc.SHA != sha)
	}
	for _, tag := range repo.Tags {
		if tag.Name == name {
			return tag.Evidence.Fresh() && (tag.SHA != sha || tag.Belongs == release.No)
		}
	}
	return complete
}

func dashboardPlan(previous, current release.Plan) release.Plan {
	if previous.SHA == "" || current.SHA != "" && current.SHA != previous.SHA {
		return current
	}
	if !current.Evidence.Fresh() && previous.Evidence.Known {
		current.SHA = previous.SHA
		current.Evidence = historicalEvidence(previous.Evidence, current.Evidence)
		current.Runs = slices.Clone(current.Runs)
		for _, old := range previous.Runs {
			index := slices.IndexFunc(current.Runs, func(run release.Run) bool {
				return old.Job == run.Job && (old.CheckID != 0 && old.CheckID == run.CheckID || old.ID != "" && old.ID == run.ID)
			})
			if index >= 0 && current.Runs[index].ID != "" {
				continue
			}
			if index < 0 {
				current.Runs = append(current.Runs, release.Run{Job: old.Job, CheckID: old.CheckID})
				index = len(current.Runs) - 1
			}
			if old.LastVerified.At.IsZero() && old.ID != "" {
				old.LastVerified = release.BuildObservation{Status: old.Status, Outcome: old.Outcome, At: previous.Evidence.At}
			}
			if !old.LastVerified.At.IsZero() {
				current.Runs[index].LastVerified = old.LastVerified
			}
		}
	}
	if !current.Validation.Evidence.Fresh() && previous.Validation.Evidence.Known {
		attempt := current.Validation.Evidence
		current.Validation = previous.Validation
		current.Validation.Evidence = historicalEvidence(current.Validation.Evidence, attempt)
	}
	return current
}

func (s *Session) observeDashboardPlan(plan release.Plan) {
	if s.activeProfile == "" || !plan.Target.Valid() || plan.SHA == "" {
		return
	}
	saved := s.dashboardForUpdate()
	plans := make(map[string]release.Plan, len(saved.Plans)+1)
	maps.Copy(plans, saved.Plans)
	key := plan.Target.String()
	previous := plans[key]
	// A tag check and a Konflux poll independently update the same display plan.
	if previous.SHA == plan.SHA && plan.Evidence.At.IsZero() && !plan.Validation.Evidence.At.IsZero() {
		validation := dashboardPlan(previous, plan).Validation
		plan = clonePlan(previous)
		plan.Validation = validation
	} else {
		if previous.SHA == plan.SHA && plan.Validation.Evidence.At.IsZero() {
			plan.Validation = previous.Validation
		}
		plan = dashboardPlan(previous, clonePlan(plan))
	}
	plan.Proposals, plan.Advisories, plan.StartFailures = nil, nil, nil
	plan.Action, plan.Message = "", ""
	for i := range plan.Runs {
		plan.Runs[i].Eligible = false
	}
	plans[key] = plan
	saved.Plans = plans
	s.storeDashboard(saved)
}

func dashboardImageRows(saved, current []release.Image, problem release.Evidence) []release.Image {
	result := slices.Clone(current)
	for _, old := range saved {
		index := slices.IndexFunc(result, func(row release.Image) bool {
			return row.Component == old.Component && row.Family == old.Family && row.Name == old.Name &&
				(row.Reference == old.Reference || row.Reference == "")
		})
		if index < 0 {
			old.Evidence = historicalEvidence(old.Evidence, problem)
			result = append(result, old)
			continue
		}
		row := result[index]
		if row.Evidence.Fresh() && old.Evidence.Known && row.Evidence.At.Before(old.Evidence.At) {
			old.Evidence = historicalEvidence(old.Evidence, release.Evidence{})
			result[index] = old
			continue
		}
		if !row.Evidence.Fresh() {
			attempt := row.Evidence
			if row.Reference == "" {
				attempt = problem
			}
			if row.Present != "" && row.Evidence.At.Equal(old.Evidence.At) && attempt.Error != "" {
				attempt.At = problem.At
			}
			old.Evidence = historicalEvidence(old.Evidence, attempt)
			result[index] = old
		}
	}
	return result
}

func (s *Session) observeDashboardImages(inventory release.Inventory, includeRC, complete bool, readErr error) {
	if s.activeProfile == "" {
		return
	}
	saved := s.dashboardForUpdate()
	images := make(map[string]dashboardImages, len(saved.Images)+1)
	maps.Copy(images, saved.Images)
	for name, binding := range s.dashboardImageTags(s.releaseWork, s.repos["acs"].Candidate) {
		if !includeRC && name == "acs" || name != "acs" && s.choices.Tags[name] == "" {
			continue
		}
		rows := slices.Clone(inventory.Images)
		if name == "acs" {
			rows = slices.Clone(inventory.ReleaseImages)
		}
		rows = slices.DeleteFunc(rows, func(row release.Image) bool {
			return row.Component != name || row.Reference == "" ||
				(!row.Evidence.Fresh() && (!complete || row.Evidence.Error == ""))
		})
		old := images[name]
		if old.Tag != binding.Tag || old.SHA != binding.SHA {
			old = dashboardImages{}
		}
		problem := release.Evidence{}
		if complete && readErr != nil {
			problem = release.Evidence{Error: readErr.Error(), At: time.Now()}
		}
		if len(rows) == 0 && (len(old.Rows) == 0 || problem.Error == "") {
			continue
		}
		binding.Version = inventory.Versions[name]
		if binding.Version == "" {
			binding.Version = old.Version
		}
		binding.Rows = dashboardImageRows(old.Rows, rows, problem)
		images[name] = binding
	}
	saved.Images = images
	s.storeDashboard(saved)
}

func (s *Session) dashboardImageTags(work *releaseWork, rc release.Candidate) map[string]dashboardImages {
	tags := make(map[string]dashboardImages)
	for _, component := range work.config.Components {
		tag := work.choices.Tags[component.Name]
		sha := work.confirmedSHAs[component.Name]
		if tag == "" {
			tag, sha = work.pendingTags[component.Name].Tag, work.pendingTags[component.Name].SHA
		}
		if tag != "" && sha != "" {
			tags[component.Name] = dashboardImages{Tag: tag, SHA: sha}
		}
	}
	if rc.Target.Tag != "" && rc.SHA != "" {
		tags["acs"] = dashboardImages{Tag: rc.Target.Tag, SHA: rc.SHA}
	}
	return tags
}

func dashboardRevisions(repos []release.Repository, tags map[string]dashboardImages) map[release.Target]string {
	result := make(map[release.Target]string)
	for _, repo := range repos {
		if pr := repo.PinningPR; pr != nil && pr.State == "open" && pr.Base == repo.Source.Branch {
			result[pr.Target] = pr.SHA
		}
		for _, pr := range repo.PRs {
			if pr.State == "open" && pr.Base == repo.Source.Branch {
				result[pr.Target] = pr.SHA
			}
		}
		if tag, ok := tags[repo.Source.Name]; ok {
			result[release.Target{Repo: repo.Source.Repo, Tag: tag.Tag}] = tag.SHA
		}
	}
	return result
}

// applyDashboard adds historical results after live queue and authority evaluation.
func (s *Session) applyDashboard(x *Snapshot, work *releaseWork, profileID string) {
	saved, ok := s.dashboards.Profiles[profileID]
	if !ok || !sameDashboardConfig(work.config, saved.Config) {
		return
	}
	x.CreatedPinningPR = saved.CreatedPinningPR
	sources := append([]release.Source{work.config.ACS()}, work.config.Components...)
	if x.Monitoring && !x.Loading {
		x.Loading = slices.ContainsFunc(sources, func(source release.Source) bool {
			return slices.ContainsFunc(s.pollJobs(source), func(job pollJob) bool {
				return s.polls[job.id(source)].finished.IsZero()
			})
		})
	}
	for _, source := range sources {
		old, ok := saved.Repositories[source.Name]
		if !ok || old.Source != source {
			continue
		}
		index := slices.IndexFunc(x.Repositories, func(repo release.Repository) bool { return repo.Source == source })
		var current release.Repository
		if index >= 0 {
			current = x.Repositories[index]
		}
		if pin := max(work.config.PinningPR, work.pendingPinningPR); old.PinningPR != nil && pin != 0 && old.PinningPR.Target.PR != pin {
			old.PinningPR, old.PinningPins, old.PinningEvidence = nil, release.Pins{}, release.Evidence{}
		}
		repo := dashboardRepository(old, current, false, "")
		if index < 0 {
			x.Repositories = append(x.Repositories, repo)
		} else {
			x.Repositories[index] = repo
		}
	}
	applyDashboardVulnerabilities(x, saved.Vulnerabilities)
	tags := s.dashboardImageTags(work, x.Candidate())
	revisions := dashboardRevisions(x.Repositories, tags)
	for _, previous := range saved.Plans {
		if previous.SHA == "" || revisions[previous.Target] != previous.SHA {
			continue
		}
		index := slices.IndexFunc(x.Plans, func(plan release.Plan) bool { return plan.Target == previous.Target })
		if index < 0 {
			x.Plans = append(x.Plans, dashboardPlan(clonePlan(previous), release.Plan{Target: previous.Target, SHA: previous.SHA}))
		} else {
			x.Plans[index] = dashboardPlan(clonePlan(previous), x.Plans[index])
		}
	}
	for name, binding := range tags {
		old := saved.Images[name]
		if old.Tag != binding.Tag || old.SHA != binding.SHA || len(old.Rows) == 0 {
			continue
		}
		if x.Inventory.Versions == nil {
			x.Inventory.Versions = make(map[string]string)
		}
		x.Inventory.ReleaseVersion = work.config.Version
		x.Inventory.Versions[name] = old.Version
		if name == "acs" {
			x.Inventory.ReleaseImages = dashboardImageRows(old.Rows, x.Inventory.ReleaseImages, release.Evidence{})
		} else {
			x.Inventory.Images = dashboardImageRows(old.Rows, x.Inventory.Images, release.Evidence{})
		}
	}
}
