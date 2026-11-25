package web

import (
	"errors"
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"release-assistant/internal/app"
	"release-assistant/internal/release"
)

func TestPinningFailurePageShowsManualCommandAndPermissions(t *testing.T) {
	cfg := validACS5Config()
	req := release.PinningRequest{Repo: cfg.ACS().Repo, Branch: cfg.Branch, Version: cfg.Version, Head: "reviewed-head",
		Changes: []release.PinChange{{Path: "FACT_VERSION", Before: "0.3.76\r\n", After: "0.3.77\r\n"}}}
	fake := &fakeApplication{
		snapshot: app.Snapshot{Config: cfg, PinningPreview: app.PinningPreview{Request: req, ID: "reviewed"}},
		err:      errors.New("could not create GitHub branch: gh: Not Found (HTTP 404) (GitHub request ID A012:BC34)"),
	}
	b := newBrowser(t, fake)
	w := b.post(url.Values{"kind": {"create-pinning-pr"}, "tab": {"create-pinning-pr"}, "id": {"reviewed"}, "generation": {"0"}})
	body := html.UnescapeString(w.Body.String())
	if w.Code != http.StatusConflict || len(fake.actions) != 1 {
		t.Fatalf("status = %d, actions = %v", w.Code, fake.actions)
	}
	for _, want := range []string{
		fake.err.Error(), "Manual steps to create a pinning PR", "Contents: write", "Pull requests: write",
		"data-copy-text>Copy command", "branch='" + req.HeadBranch() + "'", "git fetch origin 'release-5.0'",
		"test \"$(git rev-parse FETCH_HEAD)\" = 'reviewed-head'", "printf '%s\\r\\n' '0.3.77' > 'FACT_VERSION'",
		"git commit -m '" + req.Title() + "' -- 'FACT_VERSION'", "git push origin \"HEAD:refs/heads/$branch\"",
		"gh pr create --repo 'stackrox/stackrox' --base 'release-5.0' --head \"$branch\"",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("pinning page missing %q", want)
		}
	}
}
