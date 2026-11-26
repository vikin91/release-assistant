package app

import (
	"context"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"release-assistant/internal/release"
)

type vulnerabilityPollFixture struct {
	*fixture
	read func(release.Candidate, release.RCVulnerabilities) release.RCVulnerabilities
}

func (f vulnerabilityPollFixture) ReadRCVulnerabilities(_ context.Context, rc release.Candidate, _ string, prior release.RCVulnerabilities, _ release.Validation) release.RCVulnerabilities {
	return f.read(rc, prior)
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
