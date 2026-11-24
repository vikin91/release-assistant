package github

import (
	"context"
	"testing"

	"release-assistant/internal/apilog"
	"release-assistant/internal/app"
)

func TestTagBatchesResumeWithoutRepeatingResolvedTags(t *testing.T) {
	client, request, counts := discoveryFixture(t, 100, 500, 20)
	request.Scope = app.ReadTags
	batches := 0
	for {
		batches++
		repo, err := client.Discover(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		if repo.TagsPending && repo.Evidence.Fresh() {
			t.Fatal("unfinished inventory confirmed complete")
		}
		if !repo.TagsPending {
			if len(repo.Tags) != 100 {
				t.Fatalf("got %d tags", len(repo.Tags))
			}
			break
		}
		if batches > 20 {
			t.Fatal("tag discovery did not progress")
		}
	}
	if counts["tag_ref"] != 100 || counts["tags"] != 2 || counts["pr_detail"] != 0 {
		t.Fatalf("unexpected repeated work: %v", counts)
	}
	t.Logf("100 tags completed in %d bounded batches; 100 ref reads, 2 pages, 0 PR reads", batches)
}

func TestResumableTagsReportPaginationLimit(t *testing.T) {
	client, request, _ := discoveryFixture(t, 2000, 0, 0)
	// No matching tags: exercise the page cap without resolving 2,000 objects.
	request.Source.Stream = "9.9"
	request.Scope = app.ReadTags
	repo, err := client.Discover(t.Context(), request)
	if err == nil || repo.TagsPending || repo.Evidence.Fresh() {
		t.Fatalf("pagination truncated silently: pending=%v error=%v evidence=%+v", repo.TagsPending, err, repo.Evidence)
	}
}

func TestReleaseSwitchReusesRecentRepositoryTagPage(t *testing.T) {
	client, request, counts := discoveryFixture(t, 1, 0, 0)
	a := apilog.WithRelease(t.Context(), "5.0.1")
	b := apilog.WithRelease(t.Context(), "4.11.1")
	for _, ctx := range []context.Context{a, b} {
		page, err := client.tagListPage(ctx, request.Source.Repo, 1)
		if err != nil || len(page) != 1 {
			t.Fatalf("tag page = %v, %v", page, err)
		}
	}
	if counts["tags"] != 1 {
		t.Fatalf("expected one GitHub request, got %v", counts)
	}
}
