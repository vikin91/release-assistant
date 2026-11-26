package app

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

type vulnerabilityPollFixture struct {
	*fixture
	read func(release.Candidate, release.RCVulnerabilities) release.RCVulnerabilities
}

func (f vulnerabilityPollFixture) ReadRCVulnerabilities(_ context.Context, rc release.Candidate, _ string, prior release.RCVulnerabilities, _ release.Validation) release.RCVulnerabilities {
	return f.read(rc, prior)
}

type waitingConformaPlanner struct {
	*fixture
	resume <-chan struct{}
}

func (p waitingConformaPlanner) ReadConformaReport(ctx context.Context, _ int64, _ string, _ string, digest string) release.ConformaReport {
	select {
	case <-p.resume:
		return release.ConformaReport{Digest: digest}
	case <-ctx.Done():
		return release.ConformaReport{}
	}
}

func TestVulnerabilityScanPublishesWhileConformaLoads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f, _ := rcFixture(t)
		rc := f.repos["acs"].Candidate
		s.repos["acs"] = f.repos["acs"]
		s.inventory.Versions = map[string]string{"acs": rc.Target.Tag}
		s.inventory.ReleaseImages = []release.Image{{
			Family: "konflux", Reference: "quay.io/rhacs-eng/release-main:" + rc.Target.Tag, Present: release.Yes,
			ManifestDigest: "sha256:verified", Evidence: observed(),
		}}
		s.discovery = vulnerabilityPollFixture{f, func(candidate release.Candidate, _ release.RCVulnerabilities) release.RCVulnerabilities {
			return release.RCVulnerabilities{Target: candidate.Target, SHA: candidate.SHA, Images: []release.ImageVulnerabilities{
				{Image: "release-main", Status: "Available", ConformaReported: true, ConformaDetails: "https://github.com/check"},
			}}
		}}
		resume := make(chan struct{})
		s.planner = waitingConformaPlanner{fixture: f, resume: resume}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go s.refreshRCVulnerabilities(ctx, s.generation, s.workRevision)
		synctest.Wait()
		reviews := s.Snapshot().RCVulnerabilities
		if len(reviews) != 1 || !reviews[0].ScansComplete() || reviews[0].Images[0].ConformaReport.Digest != "" {
			t.Fatalf("expected scan results while Conforma was blocked, got %+v", reviews)
		}
		close(resume)
		synctest.Wait()
		if got := s.Snapshot().RCVulnerabilities[0].Images[0].ConformaReport.Digest; got != "sha256:verified" {
			t.Fatalf("detailed Conforma digest = %q", got)
		}
	})
}

func TestVulnerabilityPollPublishesWithoutRestartingMonitor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f, _ := rcFixture(t)
		acs := s.config.ACS()
		repo := f.repos[acs.Name]
		s.repos[acs.Name] = repo
		select {
		case <-s.changed:
		default:
		}

		const artifactID = 42
		var reads int
		s.discovery = vulnerabilityPollFixture{f, func(rc release.Candidate, prior release.RCVulnerabilities) release.RCVulnerabilities {
			reads++
			if reads > 1 {
				if prior.Target != rc.Target || prior.SHA != rc.SHA || len(prior.Images) != 1 || prior.Images[0].ArtifactID != artifactID {
					t.Fatalf("refresh did not reuse prior report evidence: %+v", prior)
				}
				return prior
			}
			return release.RCVulnerabilities{Target: rc.Target, SHA: rc.SHA, Images: []release.ImageVulnerabilities{
				{Image: "release-main", Status: "Available", ArtifactID: artifactID},
			}}
		}}
		s.options.RefreshIntervals[ACSTagChecks] = 5 * time.Minute
		jobs := s.pollJobs(acs)
		i := slices.IndexFunc(jobs, func(job pollJob) bool { return job.kind == pollRCVulnerabilities })
		if i < 0 {
			t.Fatal("vulnerability poll is missing")
		}
		builds := slices.IndexFunc(jobs, func(job pollJob) bool { return job.kind == pollBuilds })
		if builds < i {
			t.Fatal("routine build polling precedes the vulnerability review")
		}
		job := jobs[i]
		poll := func() {
			s.runPoll(t.Context(), s.generation, s.workRevision, acs, job)
		}

		poll()
		if reads != 1 || s.polls[job.id(acs)].finished.IsZero() {
			t.Fatal("first poll did not record completion")
		}
		select {
		case <-s.changed:
			t.Fatal("publishing scan evidence requested a monitor restart")
		default:
		}
		snapshot := s.Snapshot()
		if len(snapshot.RCVulnerabilities) == 0 || !snapshot.RCVulnerabilities[0].ScansComplete() {
			t.Fatal("downloaded scan was not visible in the dashboard snapshot")
		}
		time.Sleep(5*time.Minute - time.Nanosecond)
		poll()
		if reads != 1 {
			t.Fatal("vulnerability poll ran before the configured interval")
		}
		time.Sleep(time.Nanosecond)
		poll()
		if reads != 2 {
			t.Fatal("vulnerability poll did not run at the configured interval")
		}
	})
}

func TestVulnerabilityReportsRestoreAndRefreshAfterRestart(t *testing.T) {
	_, f, _ := rcFixture(t)
	repo := f.repos["acs"]
	rc := repo.Candidate
	previous := release.Tag{Name: configuration().Version + "-rc.1", SHA: "previous-commit", Evidence: observed()}
	repo.Tags = append(repo.Tags, previous)
	path := filepath.Join(t.TempDir(), "state.json")
	state, err := OpenReleaseState(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SetInitialRelease(configuration(), false); err != nil {
		t.Fatal(err)
	}
	makeReader := func(artifactID int64) vulnerabilityPollFixture {
		return vulnerabilityPollFixture{f, func(candidate release.Candidate, _ release.RCVulnerabilities) release.RCVulnerabilities {
			review := release.RCVulnerabilities{Target: candidate.Target, SHA: candidate.SHA}
			if artifactID == 0 {
				review.Problem = "GitHub request timed out."
				return review
			}
			if candidate.Target != rc.Target {
				review.DescendantSHA = rc.SHA
			}
			review.Images = []release.ImageVulnerabilities{{Image: "release-main", Status: "Available", ArtifactID: artifactID}}
			return review
		}}
	}
	makeSession := func(state *ReleaseState, artifactID int64) *Session {
		return New(makeReader(artifactID), f, f, dispatch.New(f, f, f, dispatch.Options{}), nil, Options{Profiles: true, ReleaseState: state})
	}
	s := makeSession(state, 42)
	s.repos["acs"] = repo
	s.observeDashboardRepo(repo, true, "")
	s.refreshRCVulnerabilities(t.Context(), s.generation, s.workRevision)
	s.flushDashboards()

	state, err = OpenReleaseState(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	s = makeSession(state, 0)
	assertReviews := func(count int, historical bool, artifactID int64) {
		t.Helper()
		reviews := s.Snapshot().RCVulnerabilities
		if len(reviews) != count || reviews[0].Target != rc.Target {
			t.Fatalf("unexpected vulnerability reviews: %+v", reviews)
		}
		for _, review := range reviews {
			if review.Historical != historical || review.ObservedAt.IsZero() || len(review.Images) != 1 || review.Images[0].ArtifactID != artifactID {
				t.Fatalf("unexpected vulnerability review: %+v", review)
			}
		}
	}
	assertReviews(2, true, 42)
	if len(s.rcVulnerabilities) != 0 {
		t.Fatal("saved reviews populated live observations")
	}

	s.repos["acs"] = repo
	s.refreshRCVulnerabilities(t.Context(), s.generation, s.workRevision)
	assertReviews(2, true, 42)
	if got := s.Snapshot().RCVulnerabilities[0].Problem; got == "" {
		t.Fatal("saved results hid the current read failure")
	}

	s.discovery = makeReader(43)
	s.refreshRCVulnerabilities(t.Context(), s.generation, s.workRevision)
	assertReviews(2, false, 43)

	repo.Candidate.SHA = "moved-rc"
	s.repos["acs"] = repo
	reviews := s.Snapshot().RCVulnerabilities
	if len(reviews) != 1 || reviews[0].SHA != "moved-rc" || len(reviews[0].Images) != 0 {
		t.Fatalf("saved reports survived a change of RC revision: %+v", reviews)
	}
}
