package app

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"release-assistant/internal/release"
)

func TestComponentTagSpanChoosesPreviousRelease(t *testing.T) {
	work := newReleaseWork()
	work.choices.Tags["collector"] = "3.24.19"
	work.confirmedSHAs["collector"] = "selected"
	repo := release.Repository{
		Source: release.Source{Name: "collector", Repo: "acme/collector", Branch: "release-3.24", Stream: "3.24"},
		Tags: []release.Tag{
			{Name: "3.24.19", SHA: "selected", Belongs: release.Yes},
			{Name: "3.24.20", SHA: "later", Belongs: release.Yes},
			{Name: "3.24.18", SHA: "off-branch", Belongs: release.No},
			{Name: "v3.24.17", SHA: "previous", Belongs: release.Yes},
			{Name: "3.23.18", SHA: "other-stream", Belongs: release.Yes},
			{Name: "3.24.16", SHA: "older", Belongs: release.Yes},
		},
	}
	want := componentTagRange{tag: "3.24.19", previousSHA: "previous", selectedSHA: "selected"}
	if got := work.componentTagSpan(repo); got != want {
		t.Fatalf("range = %+v, want %+v", got, want)
	}
	repo.Tags[2].Belongs = ""
	if got := work.componentTagSpan(repo); got.tag != "" {
		t.Fatalf("unresolved predecessor yielded range %+v", got)
	}
	repo.Tags[2].Belongs = release.No
	repo.TagsPending = true
	if got := work.componentTagSpan(repo); got.tag != "" {
		t.Fatalf("incomplete inventory yielded range %+v", got)
	}
}

type tagHistoryFixture struct {
	*fixture
	read func(release.Source, string, string) ([]release.CandidatePR, error)
}

func (f tagHistoryFixture) ReadComponentTagPRs(_ context.Context, source release.Source, previous, selected string) ([]release.CandidatePR, error) {
	return f.read(source, previous, selected)
}

func TestComponentTagHistoryPollPublishesRetryAndIgnoresPRChoices(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f, _ := rcFixture(t)
		source := s.config.Components[1]
		repo := s.repos[source.Name]
		repo.Tags = append(slices.Clone(repo.Tags), release.Tag{Name: "3.25.7", SHA: "previous", Belongs: release.Yes, Evidence: observed()})
		s.repos[source.Name] = repo
		later := release.Target{Repo: source.Repo, PR: 22}
		s.choices.PRs[later] = release.Included
		historical := release.Target{Repo: source.Repo, PR: 11}
		s.choices.PRs[historical] = release.Excluded
		attempts := 0
		s.discovery = tagHistoryFixture{f, func(got release.Source, previous, selected string) ([]release.CandidatePR, error) {
			attempts++
			if got != source || previous != "previous" || selected != repo.Tags[0].SHA {
				t.Fatalf("unexpected range %v %s...%s", got, previous, selected)
			}
			if attempts == 1 {
				return nil, errors.New("GitHub unavailable")
			}
			return []release.CandidatePR{{Number: historical.PR}}, nil
		}}
		jobs := s.pollJobs(source)
		index := slices.IndexFunc(jobs, func(job pollJob) bool { return job.kind == pollComponentTagHistory })
		if index < 0 {
			t.Fatal("component tag history poll missing")
		}
		job := jobs[index]
		s.runPoll(t.Context(), s.generation, s.workRevision, source, job)
		details := s.Snapshot().ComponentTagDetails[source.Name]
		if details.PRsKnown || details.PRError == "" || !details.NextRetry.After(time.Now()) {
			t.Fatalf("failed poll details = %+v", details)
		}
		time.Sleep(time.Until(details.NextRetry))
		s.runPoll(t.Context(), s.generation, s.workRevision, source, job)
		details = s.Snapshot().ComponentTagDetails[source.Name]
		if !details.PRsKnown || len(details.PRs) != 1 || details.PRs[0].Number != historical.PR || details.PRError != "" || !details.NextRetry.IsZero() {
			t.Fatalf("successful poll details = %+v", details)
		}
		s.runPoll(t.Context(), s.generation, s.workRevision, source, job)
		if attempts != 2 {
			t.Fatalf("successful range was read again, attempts = %d", attempts)
		}
	})
}
