// Package images resolves and inspects the component images consumed by ACS CI.
package images

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"release-assistant/internal/release"
)

const (
	ciFamily      = "ci"
	konfluxFamily = "konflux"
	acsRepository = "stackrox/stackrox"
)

var (
	imageTagPattern = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$`)
	quayRepoPattern = regexp.MustCompile(`^quay\.io/[a-z0-9_-]+/[a-z0-9._/-]+$`)
	bearerParamRE   = regexp.MustCompile(`([a-z]+)="([^"\r\n]*)"`)
	sourceRevisions = map[string]string{
		"4.10": "de310ded87bdec2cbe7f2750c6f9625632a39260",
		"4.11": "8b36e9ad94e885ab7d7756fe88de74d9655c8716",
		"5.0":  "32ef302fd73a452b292add4a6a117ce753bcf72a",
	}
)

// Inspector resolves and checks the CI, Konflux, and ACS RC.0 image references.
type Inspector struct {
	registry *registryClient
}

// New constructs an image inspector that uses Quay for registry observations.
func New(httpClient *http.Client, username, password string) *Inspector {
	return &Inspector{registry: newRegistryClient("https://quay.io", httpClient, username, password)}
}

type imagePattern struct {
	component string
	name      string
	family    string
	repo      string
	suffix    string
	file      string
}

// Inspect resolves built-in image patterns and checks their registry manifests.
func (i *Inspector) Inspect(ctx context.Context, config release.Config, confirmedTags map[string]string) (release.Inventory, error) {
	return i.inspect(ctx, config, confirmedTags, true)
}

// InspectComponents checks component prerequisites without querying ACS release candidate images.
func (i *Inspector) InspectComponents(ctx context.Context, config release.Config, confirmedTags map[string]string) (release.Inventory, error) {
	return i.inspect(ctx, config, confirmedTags, false)
}

func (i *Inspector) inspect(ctx context.Context, config release.Config, confirmedTags map[string]string, includeRC bool) (release.Inventory, error) {
	now := time.Now()
	major, minor, patch, versionOK := release.Version(config.Version)
	releaseVersion := ""
	stream := ""
	if versionOK {
		releaseVersion = fmt.Sprintf("%d.%d.%d", major, minor, patch)
		stream = fmt.Sprintf("%d.%d", major, minor)
	}
	revision := sourceRevisions[stream]
	if revision == "" && major == 4 {
		revision = sourceRevisions["4.10"]
	}
	if revision == "" && major == 5 {
		revision = sourceRevisions["5.0"]
	}
	inventory := release.Inventory{
		ReleaseVersion: releaseVersion,
		Versions:       make(map[string]string, len(config.Components)),
		Evidence:       release.Evidence{At: now, URL: revisionURL(revision)},
	}
	if err := config.Validate(); err != nil {
		inventory.Evidence.Error = "invalid release configuration: " + err.Error()
		return unknownInventory(inventory, config, revision, inventory.Evidence.Error), fmt.Errorf("invalid release configuration: %w", err)
	}
	patterns := componentPatterns(config)
	var issues []error
	var metadataIssues []error
	unselected := false
	for _, component := range config.Components {
		version, ok := normalizeTag(component, confirmedTags[component.Name])
		if !ok {
			reason := fmt.Sprintf("select a %s tag in stream %s", component.Name, component.Stream)
			if confirmedTags[component.Name] == "" {
				unselected = true
			} else {
				issues = append(issues, errors.New(reason))
				metadataIssues = append(metadataIssues, errors.New(reason))
			}
			continue
		}
		inventory.Versions[component.Name] = version
	}
	for _, pattern := range patterns {
		version, ok := inventory.Versions[pattern.component]
		if !ok {
			reason := fmt.Sprintf("select a %s tag in stream %s", pattern.component, componentStream(config, pattern.component))
			inventory.Images = append(inventory.Images, unresolvedImage(pattern, revision, now, reason))
			continue
		}
		reference := pattern.repo + ":" + version + pattern.suffix
		definition := definitionURL(revision, pattern.file)
		image := release.Image{
			Component: pattern.component, Family: pattern.family, Name: pattern.name,
			Reference: reference, DefinitionURL: definition,
			Evidence: release.Evidence{At: now, URL: definition},
		}
		if i == nil || i.registry == nil {
			image.Evidence.Error = "registry client is unavailable; image availability is unknown"
			issues = append(issues, fmt.Errorf("inspect %s %s image: registry client is unavailable", pattern.component, pattern.family))
		} else {
			present, err := i.registry.present(ctx, reference)
			image.Evidence = release.Evidence{At: time.Now(), URL: sourceURL(reference), Known: err == nil}
			if err != nil {
				image.Evidence.Error = "registry availability is unknown: " + err.Error()
				issues = append(issues, fmt.Errorf("inspect %s %s image: %w", pattern.component, pattern.family, err))
			} else {
				image.Present = present
			}
		}
		inventory.Images = append(inventory.Images, image)
	}
	if includeRC {
		inventory.ReleaseImages = inspectRC0(ctx, i, config, revision, now)
		inventory.Versions["acs"] = releaseVersion + "-rc.0"
	}
	inventory.Evidence = release.Evidence{At: time.Now(), URL: revisionURL(revision), Known: len(metadataIssues) == 0 && !unselected}
	if len(metadataIssues) > 0 {
		inventory.Evidence.Error = errors.Join(metadataIssues...).Error()
	}
	if len(inventory.Images) == 0 {
		err := errors.New("image inventory contains no expected component images")
		inventory.Evidence.Known = false
		issues = append(issues, err)
	}
	return inventory, errors.Join(issues...)
}

func componentPatterns(config release.Config) []imagePattern {
	var patterns []imagePattern
	for _, component := range config.Components {
		names := []string{component.Name}
		if component.Name == "scanner" {
			names = []string{"scanner", "scanner-db", "scanner-slim", "scanner-db-slim"}
		}
		for _, name := range names {
			patterns = append(patterns,
				imagePattern{component: component.Name, name: name, family: ciFamily, repo: "quay.io/rhacs-eng/" + name, file: "scripts/ci/lib.sh"},
				imagePattern{component: component.Name, name: name, family: konfluxFamily, repo: "quay.io/rhacs-eng/release-" + name, suffix: "-fast", file: ".tekton/retag-" + name + ".yaml"},
			)
		}
	}
	return patterns
}

func normalizeTag(source release.Source, tag string) (string, bool) {
	major, minor, patch, ok := release.Version(tag)
	version := fmt.Sprintf("%d.%d.%d", major, minor, patch)
	return version, ok && fmt.Sprintf("%d.%d", major, minor) == source.Stream
}

func unknownInventory(inventory release.Inventory, config release.Config, revision, reason string) release.Inventory {
	now := inventory.Evidence.At
	for _, pattern := range componentPatterns(config) {
		inventory.Images = append(inventory.Images, unresolvedImage(pattern, revision, now, reason))
	}
	return inventory
}

func unresolvedImage(pattern imagePattern, revision string, now time.Time, reason string) release.Image {
	definition := definitionURL(revision, pattern.file)
	return release.Image{
		Component: pattern.component, Family: pattern.family, Name: pattern.name, DefinitionURL: definition,
		Evidence: release.Evidence{At: now, URL: definition, Error: reason},
	}
}

func componentStream(config release.Config, name string) string {
	for _, component := range config.Components {
		if component.Name == name {
			return component.Stream
		}
	}
	return "unknown"
}

func revisionURL(sha string) string {
	if sha == "" {
		return "https://github.com/" + acsRepository
	}
	return "https://github.com/" + acsRepository + "/tree/" + url.PathEscape(sha)
}

func definitionURL(sha, file string) string {
	if sha == "" || file == "" {
		return ""
	}
	parts := strings.Split(file, "/")
	for index := range parts {
		parts[index] = url.PathEscape(parts[index])
	}
	return "https://github.com/" + acsRepository + "/blob/" + url.PathEscape(sha) + "/" + strings.Join(parts, "/")
}

func sourceURL(reference string) string {
	if reference == "" {
		return ""
	}
	parts := strings.SplitN(strings.TrimPrefix(reference, "quay.io/"), ":", 2)
	if len(parts) != 2 {
		return ""
	}
	repoParts := strings.Split(parts[0], "/")
	for index := range repoParts {
		repoParts[index] = url.PathEscape(repoParts[index])
	}
	return "https://quay.io/repository/" + strings.Join(repoParts, "/") + "?tag=" + url.QueryEscape(parts[1])
}

func validQuayRepo(repo string) bool {
	return quayRepoPattern.MatchString(repo) && !strings.Contains(repo, "..") && path.Clean(repo) == repo
}
