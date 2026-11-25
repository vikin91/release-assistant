package integration

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"release-assistant/internal/app"
	"release-assistant/internal/github"
	"release-assistant/internal/images"
	"release-assistant/internal/release"
)

// TestLiveReadAdapters is opt-in and never constructs a comment writer.
func TestLiveReadAdapters(t *testing.T) {
	if os.Getenv("ACS_LIVE_READ_SMOKE") == "" {
		t.Skip("set ACS_LIVE_READ_SMOKE to check real read adapters")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	token := os.Getenv("GH_TOKEN")
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}
	if token == "" {
		authCtx, authCancel := context.WithTimeout(ctx, 5*time.Second)
		out, err := exec.CommandContext(authCtx, "gh", "auth", "token", "--hostname", "github.com").Output()
		authCancel()
		if err == nil {
			token = strings.TrimSpace(string(out))
		}
	}
	client := &http.Client{Timeout: 20 * time.Second}
	reader := github.New("", token, client)
	var repository release.Repository
	var err error
	if sha := os.Getenv("ACS_LIVE_SOURCE_SHA"); sha != "" {
		repository.Head = sha
		repository.Pins = release.Pins{Versions: map[string]string{}, Evidence: observation()}
		for _, name := range []string{"fact", "collector", "scanner"} {
			data, readErr := reader.File(ctx, "stackrox/stackrox", sha, strings.ToUpper(name)+"_VERSION")
			if readErr != nil {
				t.Fatal("live pin read unavailable")
			}
			repository.Pins.Versions[name] = strings.TrimSpace(string(data))
		}
	} else {
		repository, err = reader.Discover(ctx, app.DiscoveryRequest{Source: release.Source{Name: "acs", Repo: "stackrox/stackrox", Branch: "release-4.10"}, Version: "4.10.1"})
	}
	if repository.Head == "" || !repository.Pins.Evidence.Fresh() {
		t.Fatalf("live GitHub branch/pins unavailable: %v", err)
	}
	if err != nil {
		t.Logf("GitHub retained partial observations: %v", err)
	}
	config := release.Config{Version: "4.10.1", Branch: "release-4.10"}
	tags := map[string]string{}
	for _, name := range []string{"fact", "collector", "scanner"} {
		version := repository.Pins.Versions[name]
		major, minor, _, ok := release.Version(version)
		if !ok {
			t.Fatalf("live %s pin is not a final version", name)
		}
		stream := strings.Join([]string{strconv.Itoa(major), strconv.Itoa(minor)}, ".")
		config.Components = append(config.Components, release.Source{Name: name, Repo: "stackrox/" + name, Branch: "release-" + stream, Stream: stream})
		tags[name] = version
	}
	// This smoke test checks saved image patterns against the registry.
	user, password := os.Getenv("QUAY_USERNAME"), os.Getenv("QUAY_PASSWORD")
	if user == "" && password == "" {
		user = os.Getenv("QUAY_RHACS_ENG_RO_USERNAME")
		password = os.Getenv("QUAY_RHACS_ENG_RO_PASSWORD")
	}
	inventory, inspectErr := images.New(client, user, password).Inspect(ctx, config, tags)
	if !inventory.Evidence.Fresh() {
		t.Fatalf("live image pattern set is unresolved: %v", inspectErr)
	}
	if len(inventory.Images) != 18 {
		t.Fatalf("expected 18 real image references, got %d", len(inventory.Images))
	}
	present, missing, unknown := 0, 0, 0
	for _, image := range inventory.Images {
		if image.Reference == "" {
			t.Fatal("live inventory contains an unresolved reference")
		}
		switch {
		case !image.Evidence.Fresh():
			unknown++
		case image.Present == release.Yes:
			present++
		case image.Present == release.No:
			missing++
		default:
			unknown++
		}
	}
	t.Logf("Resolved ACS revision %s: images present=%d missing=%d unknown=%d", repository.Head, present, missing, unknown)
	if inspectErr != nil {
		t.Logf("Registry observations remain partial: %v", inspectErr)
	}
}

// TestLiveImagePatterns checks registry reads without constructing a GitHub client.
func TestLiveImagePatterns(t *testing.T) {
	if os.Getenv("ACS_LIVE_IMAGE_SMOKE") == "" {
		t.Skip("set ACS_LIVE_IMAGE_SMOKE for read-only registry validation")
	}
	config := release.Config{Version: "5.0.1", Branch: "release-5.0", Components: []release.Source{
		{Name: "fact", Repo: "stackrox/fact", Branch: "release-0.4", Stream: "0.4"},
		{Name: "collector", Repo: "stackrox/collector", Branch: "release-3.26", Stream: "3.26"},
	}}
	user, password := os.Getenv("QUAY_USERNAME"), os.Getenv("QUAY_PASSWORD")
	if user == "" && password == "" {
		user = os.Getenv("QUAY_RHACS_ENG_RO_USERNAME")
		password = os.Getenv("QUAY_RHACS_ENG_RO_PASSWORD")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	inventory, err := images.New(&http.Client{Timeout: 15 * time.Second}, user, password).Inspect(ctx, config, map[string]string{"fact": "0.4.7", "collector": "3.26.5"})
	if inventory.ReleaseVersion != "5.0.1" || len(inventory.Images) != 6 || len(inventory.ReleaseImages) != 6 {
		t.Fatalf("saved patterns incomplete: %+v", inventory)
	}
	unknown := 0
	for _, image := range append(inventory.Images, inventory.ReleaseImages...) {
		t.Logf("%s: present=%s verified=%t %s", image.Reference, image.Present, image.Evidence.Fresh(), image.Evidence.Error)
		if !image.Evidence.Fresh() {
			unknown++
		}
	}
	if unknown > 0 {
		t.Fatalf("%d registry reads could not be verified: %v", unknown, err)
	}
	if err != nil {
		t.Fatal(err)
	}
}
