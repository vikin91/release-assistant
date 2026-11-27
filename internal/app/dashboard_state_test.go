package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

func TestDashboardRestartsInactiveThenRefreshesIndependently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	state, err := OpenReleaseState(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	config := configuration()
	other := configuration()
	other.Version, other.Branch = "5.1.1", "release-5.1"
	for _, configured := range []release.Config{other, config} {
		if err := state.SetInitialRelease(configured, false); err != nil {
			t.Fatal(err)
		}
	}
	fact := config.Components[0]
	const tag = "0.3.77"
	const sha = "fact-head"
	if err := state.saveComponentTag(config.Version, fact, tag, sha); err != nil {
		t.Fatal(err)
	}
	target := release.Target{Repo: fact.Repo, PR: 42}
	if err := state.savePRChoices(config.Version, map[release.Target]string{target: sha}, release.Included); err != nil {
		t.Fatal(err)
	}
	_, f := newFixture()
	fresh := observed()
	repo := f.repos[fact.Name]
	repo.PRs = []release.PR{{
		Target: target, SHA: sha, State: "open", Base: fact.Branch, Title: "Saved PR",
		Evidence: fresh, ChecksEvidence: fresh, CheckResultsEvidence: fresh,
		Checks: []release.Check{{Name: "test", Status: "completed", Conclusion: "failure"}},
	}}
	f.repos[fact.Name] = repo
	buildTarget := release.Target{Repo: fact.Repo, Tag: tag}
	makeSession := func(state *ReleaseState) *Session {
		return New(f, f, f, dispatch.New(f, f, f, dispatch.Options{EnableWrites: true}), nil,
			Options{Profiles: true, ReleaseState: state})
	}
	s := makeSession(state)
	profiles, id, _ := s.ReleaseProfiles()
	if err := s.refresh(t.Context(), s.generation, fact, true, sourceWork{revision: s.workRevision, scope: ReadPRs | ReadTags | ReadChecks}); err != nil {
		t.Fatal(err)
	}
	s.observeDashboardPlan(release.Plan{
		Target: buildTarget, SHA: sha, Evidence: fresh,
		Runs:      []release.Run{{Job: "build", ID: "17", Status: "completed", Outcome: "success", Eligible: true}},
		Proposals: []release.Proposal{{Target: buildTarget, SHA: sha, Payload: "/retest build"}},
	})
	image := release.Image{
		Component: fact.Name, Family: "ci", Name: "fact", Reference: "quay.io/test/fact:" + tag,
		Present: release.Yes, Evidence: fresh,
	}
	inventory := release.Inventory{ReleaseVersion: config.Version, Versions: map[string]string{fact.Name: tag}, Images: []release.Image{image}}
	s.publishImageInventory(inventory, map[string]string{fact.Name: tag}, false, nil, true)
	s.flushDashboards()
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "state.observations.json")); err != nil {
		t.Fatal(err)
	}
	apply(t, s, Action{Kind: "activate-release", ProfileID: profiles[0].ID})

	state, err = OpenReleaseState(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	s = makeSession(state)
	before := len(f.requests)
	paused, found := s.ProfileSnapshot(id)
	if !found || paused.Monitoring || paused.WritesEnabled || len(f.requests) != before {
		t.Fatalf("opening inactive dashboard changed monitoring: %+v", paused)
	}
	if len(paused.Repositories) != 1 || len(paused.Repositories[0].PRs) != 1 || paused.Repositories[0].PRs[0].Title != "Saved PR" {
		t.Fatalf("lost saved PR: %+v", paused.Repositories)
	}
	if pr := paused.Repositories[0].PRs[0]; !pr.Evidence.Historical || !pr.CheckResultsEvidence.At.Equal(fresh.At) || len(pr.Checks) != 1 {
		t.Fatalf("lost saved checks or provenance: %+v", pr)
	}
	if len(paused.Plans) != 1 || len(paused.Plans[0].Proposals) != 0 ||
		len(paused.Plans[0].Runs) != 1 || paused.Plans[0].Runs[0].LastVerified.Outcome != "success" {
		t.Fatalf("lost saved build or restored posting evidence: %+v", paused.Plans)
	}
	assertSavedImage := func(view Snapshot) {
		t.Helper()
		if len(view.Inventory.Images) != 1 {
			t.Fatalf("saved image disappeared: %+v", view.Inventory.Images)
		}
		row := view.Inventory.Images[0]
		if row.Present != release.Yes || row.Evidence.Fresh() || !row.Evidence.At.Equal(fresh.At) {
			t.Fatalf("saved image changed: %+v", row)
		}
	}
	assertSavedImage(paused)
	apply(t, s, Action{Kind: "activate-release", ProfileID: id})
	assertSavedImage(s.Snapshot())
	if len(s.repos) != 0 || len(s.plans) != 0 || len(s.Snapshot().RetestQueue) != 0 {
		t.Fatal("restored dashboard populated live retry state")
	}
	if err := s.Apply(t.Context(), Action{Kind: "grant", ProfileID: id, Generation: s.generation,
		Target: buildTarget, SHA: sha, Enabled: true}); err == nil {
		t.Fatal("granted permission from display history")
	}

	repo.PRs[0].Title, repo.PRs[0].Evidence = "Fresh PR", observed()
	f.repos[fact.Name] = repo
	if err := s.refresh(t.Context(), s.generation, fact, true, sourceWork{revision: s.workRevision, scope: ReadPRs | ReadTags}); err != nil {
		t.Fatal(err)
	}
	refreshed := s.Snapshot()
	assertSavedImage(refreshed)
	if pr := refreshed.Repositories[0].PRs[0]; !pr.Evidence.Fresh() || pr.Title != "Fresh PR" || len(pr.Checks) != 1 {
		t.Fatalf("PR-only refresh lost check history or failed to update the PR: %+v", pr)
	}

	failed := image
	failed.Present = ""
	failed.Evidence = release.Evidence{At: fresh.At.Add(time.Minute), Error: "registry timeout"}
	inventory.Images = []release.Image{failed}
	s.publishImageInventory(inventory, map[string]string{fact.Name: tag}, false, errors.New("registry timeout"), true)
	row := s.Snapshot().Inventory.Images[0]
	if row.Present != release.Yes || !row.Evidence.Historical || !row.Evidence.At.Equal(fresh.At) ||
		!row.Evidence.AttemptAt.Equal(failed.Evidence.At) || row.Evidence.Error == "" {
		t.Fatalf("failed refresh lost verification or attempt time: %+v", row)
	}

	missing := image
	missing.Present = release.No
	missing.Evidence.At = fresh.At.Add(2 * time.Minute)
	inventory.Images = []release.Image{missing}
	s.publishImageInventory(inventory, map[string]string{fact.Name: tag}, false, nil, true)
	row = s.Snapshot().Inventory.Images[0]
	if row.Present != release.No || !row.Evidence.Fresh() || !row.Evidence.At.Equal(missing.Evidence.At) {
		t.Fatalf("confirmed absence failed to replace saved success: %+v", row)
	}
	s.flushDashboards()
	state, err = OpenReleaseState(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	s = makeSession(state)
	if row := s.Snapshot().Inventory.Images[0]; row.Present != release.No || !row.Evidence.At.Equal(missing.Evidence.At) {
		t.Fatalf("confirmed absence failed to survive restart: %+v", row)
	}

	repo.PRs[0].SHA = "new-pr-head"
	repo.Tags[0].SHA = "new-tag-head"
	f.repos[fact.Name] = repo
	if err := s.refresh(t.Context(), s.generation, fact, true, sourceWork{revision: s.workRevision, scope: ReadPRs | ReadTags}); err != nil {
		t.Fatal(err)
	}
	now := s.Snapshot()
	if len(now.Inventory.Images) != 0 || slices.ContainsFunc(now.Plans, func(plan release.Plan) bool { return plan.SHA == sha }) ||
		len(now.Repositories[0].PRs[0].Checks) != 0 {
		t.Fatalf("carried old evidence to changed revisions: %+v", now)
	}
}

func TestDashboardIgnoresUnreadableSidecar(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	state, err := OpenReleaseState(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SetInitialRelease(configuration(), false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "state.observations.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	_, f := newFixture()
	s := New(f, f, f, dispatch.New(f, f, f, dispatch.Options{}), nil, Options{Profiles: true, ReleaseState: state})
	if profiles, active, warnings := s.ReleaseProfiles(); len(profiles) != 1 || active != profiles[0].ID || len(warnings) != 1 {
		t.Fatalf("corrupt observations affected the active profile: %+v %q %v", profiles, active, warnings)
	}
}

func TestSavedRCResultsSurviveFailedComparisonAndYieldToVerifiedAbsence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	state, err := OpenReleaseState(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	config := configuration()
	if err := state.SetInitialRelease(config, false); err != nil {
		t.Fatal(err)
	}
	_, f := newFixture()
	newSession := func(state *ReleaseState) *Session {
		return New(f, f, f, dispatch.New(f, f, f, dispatch.Options{}), nil,
			Options{Profiles: true, ReleaseState: state})
	}
	s := newSession(state)
	fresh := observed()
	candidate := release.Candidate{
		Target: release.Target{Repo: config.ACS().Repo, Tag: "5.0.1-rc.1"},
		SHA:    "rc-sha", Evidence: fresh,
	}
	repo := release.Repository{Source: config.ACS(), Head: candidate.SHA, Evidence: fresh, Candidate: candidate}
	s.repos["acs"] = repo
	s.observeDashboardRepo(repo, true, "")
	s.observeDashboardPlan(release.Plan{
		Target: candidate.Target, SHA: candidate.SHA,
		Validation: release.Validation{SHA: candidate.SHA, Evidence: fresh},
	})
	s.publishImageInventory(release.Inventory{
		ReleaseVersion: config.Version,
		Versions:       map[string]string{"acs": candidate.Target.Tag},
		ReleaseImages: []release.Image{{
			Component: "acs", Name: "main", Family: "ci", Reference: "quay.io/test/main:" + candidate.Target.Tag,
			Present: release.Yes, Evidence: fresh,
		}},
	}, nil, true, nil, true)

	checkpoint := func() *Session {
		t.Helper()
		s.flushDashboards()
		state, err := OpenReleaseState(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		return newSession(state)
	}
	s = checkpoint()
	repo.Head = "advanced-branch-head"
	failed := release.Evidence{At: fresh.At.Add(time.Minute), Error: "comparison timed out"}
	repo.Candidate = release.Candidate{Evidence: failed}
	s.observeDashboardRepo(repo, false, failed.Error)
	view := s.Snapshot()
	if current := view.Candidate(); current.SHA != candidate.SHA || !current.Evidence.Historical ||
		!current.Evidence.At.Equal(fresh.At) || !current.Evidence.AttemptAt.Equal(failed.At) ||
		len(view.Inventory.ReleaseImages) != 1 || len(view.Plans) != 1 {
		t.Fatalf("failed comparison lost previous RC evidence: %+v", view)
	}

	absent := release.Evidence{At: failed.At.Add(time.Minute), Known: true}
	repo.Candidate = release.Candidate{Evidence: absent}
	s.observeDashboardRepo(repo, true, "")
	s = checkpoint()
	view = s.Snapshot()
	if current := view.Candidate(); current.Target.Valid() || !current.Evidence.At.Equal(absent.At) ||
		len(view.Inventory.ReleaseImages) != 0 || len(view.Plans) != 0 {
		t.Fatalf("verified absence revived the old RC: %+v", view)
	}
}

func TestDashboardWriterBatchesUpdatesAndFlushesOnShutdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	state, err := OpenReleaseState(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	config := configuration()
	if err := state.SetInitialRelease(config, false); err != nil {
		t.Fatal(err)
	}
	assertSavedHead := func(t *testing.T, want string) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(filepath.Dir(path), "state.observations.json"))
		if err != nil {
			t.Fatal(err)
		}
		var file dashboardFile
		if err := json.Unmarshal(data, &file); err != nil {
			t.Fatal(err)
		}
		if got := file.Profiles[state.file.ActiveProfile].Repositories["acs"].Head; got != want {
			t.Fatalf("saved head = %q, want %q", got, want)
		}
	}
	synctest.Test(t, func(t *testing.T) {
		_, f := newFixture()
		s := New(f, f, f, dispatch.New(f, f, f, dispatch.Options{}), nil,
			Options{Profiles: true, ReleaseState: state})
		stop, stopped := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(stopped)
			s.runDashboardWriter(stop)
		}()
		defer func() {
			close(stop)
			<-stopped
		}()
		setHead := func(head string) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.observeDashboardRepo(release.Repository{Source: config.ACS(), Head: head, Evidence: observed()}, false, "")
		}
		synctest.Wait()
		setHead("first")
		setHead("second")
		if _, err := os.Stat(filepath.Join(filepath.Dir(path), "state.observations.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("checkpoint was written before the first tick: %v", err)
		}
		time.Sleep(dashboardSaveInterval)
		synctest.Wait()
		assertSavedHead(t, "second")
		setHead("third")
		assertSavedHead(t, "second")
	})
	assertSavedHead(t, "third")
}

func TestDashboardMigratesLegacyFilenameAndPrefersCanonicalFile(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "staging.release.json")
	legacyPath := filepath.Join(dir, "staging.release.json.observations.json")
	canonicalPath := filepath.Join(dir, "staging.release.observations.json")
	state, err := OpenReleaseState(statePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	config := configuration()
	if err := state.SetInitialRelease(config, false); err != nil {
		t.Fatal(err)
	}
	id := state.file.ActiveProfile
	source := config.ACS()
	saved := dashboardFile{
		Version: dashboardStateVersion,
		Profiles: map[string]dashboardState{
			id: {
				Config: config,
				Repositories: map[string]release.Repository{
					source.Name: {Source: source, Head: "legacy-head", Evidence: observed()},
				},
			},
		},
	}
	if err := writeStateJSON(legacyPath, saved, ""); err != nil {
		t.Fatal(err)
	}
	_, f := newFixture()
	restart := func() *Session {
		return New(f, f, f, dispatch.New(f, f, f, dispatch.Options{}), nil,
			Options{Profiles: true, ReleaseState: state})
	}
	assertHead := func(s *Session, want string) {
		t.Helper()
		repos := s.Snapshot().Repositories
		if len(repos) != 1 || repos[0].Head != want {
			t.Fatalf("saved repositories = %+v, want head %q", repos, want)
		}
	}
	s := restart()
	assertHead(s, "legacy-head")
	s.flushDashboards()
	if _, err := os.Stat(canonicalPath); err != nil {
		t.Fatal(err)
	}

	s.observeDashboardRepo(release.Repository{Source: source, Head: "new-head", Evidence: observed()}, false, "")
	s.flushDashboards()
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatal(err)
	}
	assertHead(restart(), "new-head")
}

func TestManualTagObservationsRestoreWithoutDiscovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	config := configuration()
	fact := config.Components[0]
	_, f := newFixture()
	tag := f.repos[fact.Name].Tags[0]
	d := &manualTagDiscovery{fixture: f, names: []string{tag.Name}}
	makeSession := func() *Session {
		t.Helper()
		state, err := OpenReleaseState(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(state.file.Profiles) == 0 {
			if err := state.SetInitialRelease(config, false); err != nil {
				t.Fatal(err)
			}
		}
		return New(d, f, f, dispatch.New(f, f, f, dispatch.Options{}), nil, Options{Profiles: true, ReleaseState: state})
	}
	s := makeSession()
	for _, action := range []string{"fetch-tag-names", "verify-recent-tags"} {
		apply(t, s, Action{Kind: action, Component: fact.Name})
		s.runComponentTagAction(t.Context(), s.generation, s.workRevision, fact, action)
	}
	s.flushDashboards()
	restarted := makeSession().Snapshot()
	names := restarted.ComponentTagNames[fact.Name]
	if !slices.Equal(names.Names, []string{tag.Name}) || !names.Evidence.Historical || names.Evidence.At.IsZero() {
		t.Fatalf("restored tag names = %+v", names)
	}
	if !slices.ContainsFunc(restarted.Repositories, func(repo release.Repository) bool {
		return repo.Source == fact && slices.ContainsFunc(repo.Tags, func(saved release.Tag) bool {
			return saved.Name == tag.Name && saved.SHA == tag.SHA && saved.Evidence.Historical && saved.Evidence.At.Equal(tag.Evidence.At)
		})
	}) {
		t.Fatalf("saved verification missing from %+v", restarted.Repositories)
	}
	if len(d.lists) != 1 || len(f.requests) != 1 {
		t.Fatalf("restart called discovery: lists=%v and requests=%v", d.lists, f.requests)
	}
}

func TestSelectObservedTagAfterRestartWithoutFetchingNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	config := configuration()
	source := config.Components[1]
	_, f := newFixture()
	repo := f.repos[source.Name]
	previous := repo.Tags[0]
	chosen := release.Tag{Name: "3.25.7", SHA: "previous-collector", Belongs: release.Yes, Evidence: observed()}
	repo.Tags = append(repo.Tags, chosen)
	f.repos[source.Name] = repo
	d := &manualTagDiscovery{fixture: f}
	openSession := func() *Session {
		t.Helper()
		state, err := OpenReleaseState(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(state.file.Profiles) == 0 {
			if err := state.SetInitialRelease(config, false); err != nil {
				t.Fatal(err)
			}
			if err := state.saveComponentTag(config.Version, source, previous.Name, previous.SHA); err != nil {
				t.Fatal(err)
			}
		}
		return New(d, f, f, dispatch.New(f, f, f, dispatch.Options{}), nil, Options{Profiles: true, ReleaseState: state})
	}
	initial := openSession()
	initial.observeDashboardRepo(repo, true, "")
	initial.flushDashboards()
	restarted := openSession()
	view := restarted.Snapshot()
	if view.PendingTags[source.Name] != previous.Name || len(view.ComponentTagNames[source.Name].Names) != 0 {
		t.Fatal("expected a saved selection and repository observations without a names catalog")
	}
	const action = "select-fetched-tag"
	apply(t, restarted, Action{Kind: action, Component: source.Name, Tag: chosen.Name})
	if len(f.requests) != 0 || len(d.lists) != 0 {
		t.Fatal("loading saved choices or queuing a selection performed a read")
	}
	restarted.runComponentTagAction(t.Context(), restarted.generation, restarted.workRevision, source, action)
	if len(d.lists) != 0 || len(f.requests) != 1 || f.requests[0].Scope != ReadTags ||
		!slices.Equal(f.requests[0].TagCandidates, []string{chosen.Name}) {
		t.Fatalf("selection issued unexpected reads: names=%v discovery=%+v", d.lists, f.requests)
	}
	if got := restarted.Snapshot().Choices.Tags[source.Name]; got != chosen.Name {
		t.Fatalf("selected tag = %q, want %q", got, chosen.Name)
	}
}

func TestDashboardRetainsPRLinksWhileRCBranchComparisonRefreshes(t *testing.T) {
	source := configuration().ACS()
	old := release.Repository{Source: source, Candidate: release.Candidate{
		Target: release.Target{Repo: source.Repo, Tag: "5.0.1-rc.0"}, SHA: "rc", Evidence: observed(),
		CompareURL: "rc...old", Ahead: 39, PRs: []release.CandidatePR{{Number: 123}},
	}}
	current := old
	current.Candidate.CompareURL, current.Candidate.Ahead = "rc...new", 40
	current.Candidate.PRs, current.Candidate.PRsPending = nil, true

	pending := dashboardRepository(old, current, true, "")
	if rc := pending.Candidate; rc.Ahead != 40 || !rc.PRsPending || !rc.PRsHistorical || len(rc.PRs) != 1 || rc.PRs[0].Number != 123 {
		t.Fatalf("pending comparison lost its display history: %+v", rc)
	}
	current.Candidate.PRsPending, current.Candidate.PRError = false, "GitHub request timed out."
	failed := dashboardRepository(pending, current, true, "").Candidate
	if !failed.PRsHistorical || failed.PRError == "" || len(failed.PRs) != 1 {
		t.Fatalf("failed comparison lost its display history: %+v", failed)
	}

	current.Candidate.PRError = ""
	complete := dashboardRepository(pending, current, true, "").Candidate
	if complete.PRsHistorical || len(complete.PRs) != 0 {
		t.Fatalf("verified empty list did not replace display history: %+v", complete)
	}
	current.Candidate.SHA, current.Candidate.PRsPending = "moved-rc", true
	moved := dashboardRepository(pending, current, true, "").Candidate
	if moved.PRsHistorical || len(moved.PRs) != 0 {
		t.Fatalf("historical PRs leaked across RC commits: %+v", moved)
	}
}
