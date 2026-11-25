// Package images resolves and inspects the component images consumed by ACS CI.
package images

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"release-assistant/internal/release"
)

var (
	imageTagPattern = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$`)
	quayRepoPattern = regexp.MustCompile(`^quay\.io/[a-z0-9_-]+/[a-z0-9._/-]+$`)
	bearerParamRE   = regexp.MustCompile(`([a-z]+)="([^"\r\n]*)"`)
)

// Inspector resolves and checks the CI, Konflux, and ACS release candidate image references.
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
	return i.inspectImages(ctx, config, confirmedTags, includeRC, false, nil)
}

// InspectImages publishes pending rows and each completed registry observation in catalog order.
func (i *Inspector) InspectImages(ctx context.Context, config release.Config, tags map[string]string, force bool, progress func(release.Inventory)) (release.Inventory, error) {
	return i.inspectImages(ctx, config, tags, true, force, progress)
}

func (i *Inspector) inspectImages(ctx context.Context, config release.Config, confirmedTags map[string]string, includeRC, force bool, progress func(release.Inventory)) (release.Inventory, error) {
	now := time.Now()
	major, minor, patch, versionOK := release.Version(config.Version)
	releaseVersion := ""
	if versionOK {
		releaseVersion = fmt.Sprintf("%d.%d.%d", major, minor, patch)
	}
	inventory := release.Inventory{
		ReleaseVersion: releaseVersion,
		Versions:       make(map[string]string, len(config.Components)),
		Evidence:       release.Evidence{At: now},
	}
	if err := config.Validate(); err != nil {
		inventory.Evidence.Error = "invalid release configuration: " + err.Error()
		return unknownInventory(inventory, config, inventory.Evidence.Error), fmt.Errorf("invalid release configuration: %w", err)
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
			inventory.Images = append(inventory.Images, unresolvedImage(pattern, now, reason))
			continue
		}
		reference := pattern.repo + ":" + version + pattern.suffix
		inventory.Images = append(inventory.Images, release.Image{
			Component: pattern.component, Family: pattern.family, Name: pattern.name,
			Reference: reference,
			Evidence:  release.Evidence{URL: sourceURL(reference)},
		})
	}
	if tag := strings.TrimPrefix(confirmedTags["acs"], "v"); includeRC && tag != "" {
		inventory.ReleaseImages = ExpectedRCImages(major, tag)
		for index := range inventory.ReleaseImages {
			row := &inventory.ReleaseImages[index]
			row.Evidence.URL = sourceURL(row.Reference)
		}
		inventory.Versions["acs"] = tag
	}
	inventory.Evidence = release.Evidence{At: time.Now(), Known: len(metadataIssues) == 0 && !unselected}
	if len(metadataIssues) > 0 {
		inventory.Evidence.Error = errors.Join(metadataIssues...).Error()
	}
	if len(inventory.Images) == 0 {
		err := errors.New("image inventory contains no expected component images")
		inventory.Evidence.Known = false
		issues = append(issues, err)
	}
	publish := func() {
		if progress != nil {
			partial := inventory
			partial.Versions = maps.Clone(inventory.Versions)
			partial.Images = slices.Clone(inventory.Images)
			partial.ReleaseImages = slices.Clone(inventory.ReleaseImages)
			progress(partial)
		}
	}
	type result struct {
		row   *release.Image
		image release.Image
		err   error
	}
	results := make(chan result, len(inventory.Images)+len(inventory.ReleaseImages))
	pending := 0
	for _, rows := range [][]release.Image{inventory.Images, inventory.ReleaseImages} {
		for index, image := range rows {
			if image.Reference == "" {
				continue
			}
			row := &rows[index]
			if !force && i != nil && i.registry != nil {
				if observation, ok := i.registry.confirmed(imageProbe(image)); ok {
					observation.apply(row)
					row.Evidence.Known = true
					continue
				}
			}
			pending++
			go func() {
				observed, err := i.observeImage(ctx, image, force)
				results <- result{row: row, image: observed, err: err}
			}()
		}
	}
	publish()
	for range pending {
		result := <-results
		*result.row = result.image
		if image := result.image; result.err != nil && image.Component != "acs" {
			issues = append(issues, fmt.Errorf("inspect %s %s image: %w", image.Component, image.Family, result.err))
		}
		publish()
	}
	return inventory, errors.Join(issues...)
}

// observeImage keeps a failed registry read distinct from confirmed absence.
func (i *Inspector) observeImage(ctx context.Context, image release.Image, force bool) (release.Image, error) {
	var err error
	if i == nil || i.registry == nil {
		image.Evidence.At = time.Now()
		err = errors.New("registry client is unavailable")
	} else {
		var observation registryObservation
		observation, err = i.registry.observe(ctx, imageProbe(image), force)
		observation.apply(&image)
	}
	image.Evidence.Known = err == nil
	if err != nil {
		image.Evidence.Error = "registry availability is unknown: " + err.Error()
	}
	return image, err
}

func imageProbe(image release.Image) registryProbe {
	return registryProbe{reference: image.Reference, includeCreation: image.Component != "acs"}
}

func (observation registryObservation) apply(image *release.Image) {
	image.Present, image.Evidence.At = observation.present, observation.at
	image.CreatedAt, image.CreatedPlatform = observation.created, observation.platform
}

func componentPatterns(config release.Config) []imagePattern {
	var patterns []imagePattern
	for _, component := range config.Components {
		names, ok := componentImageNames[component.Name]
		if !ok {
			names = []string{component.Name}
		}
		for _, name := range names {
			for _, family := range imageFamilies {
				patterns = append(patterns, imagePattern{
					component: component.Name, name: name, family: family.family,
					repo: family.repositoryPrefix + name, suffix: family.tagSuffix,
				})
			}
		}
	}
	return patterns
}

func normalizeTag(source release.Source, tag string) (string, bool) {
	major, minor, patch, ok := release.Version(tag)
	version := fmt.Sprintf("%d.%d.%d", major, minor, patch)
	return version, ok && fmt.Sprintf("%d.%d", major, minor) == source.Stream
}

func unknownInventory(inventory release.Inventory, config release.Config, reason string) release.Inventory {
	now := inventory.Evidence.At
	for _, pattern := range componentPatterns(config) {
		inventory.Images = append(inventory.Images, unresolvedImage(pattern, now, reason))
	}
	return inventory
}

func unresolvedImage(pattern imagePattern, now time.Time, reason string) release.Image {
	return release.Image{
		Component: pattern.component, Family: pattern.family, Name: pattern.name,
		Evidence: release.Evidence{At: now, Error: reason},
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
