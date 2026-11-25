package images

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"release-assistant/internal/release"
)

func (c *registryClient) withTagMapping(ctx context.Context, manifest registryObservation, repository, tag, digest string) registryObservation {
	if c.apiToken == "" {
		manifest.tagError = "Set QUAY_API_TOKEN with repo:read access to check when this tag was updated."
		return manifest
	}
	mapping, err := c.activeTag(ctx, repository, tag, "Bearer "+c.apiToken)
	switch {
	case err != nil:
		manifest.tagError = err.Error()
	case mapping.present != release.Yes || mapping.tagDigest != digest:
		manifest.tagError = "Quay returned different tag and manifest results. Check again for the current tag age."
	default:
		manifest.tagDigest, manifest.tagUpdatedAt = mapping.tagDigest, mapping.tagUpdatedAt
	}
	return manifest
}

func (c *registryClient) activeTag(ctx context.Context, repository, tag, authorization string) (registryObservation, error) {
	var observation registryObservation
	query := url.Values{"specificTag": {tag}, "onlyActiveTags": {"true"}}
	address := c.baseURL + "/api/v1/repository/" + repository + "/tag/?" + query.Encode()
	response, err := c.request(ctx, address, authorization, "application/json")
	if err != nil {
		return observation, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return observation, fmt.Errorf("quay tag lookup returned HTTP %d", response.StatusCode)
	}
	body, err := readBody(response.Body)
	if err != nil {
		return observation, err
	}
	var result struct {
		Tags []struct {
			Name   string `json:"name"`
			Digest string `json:"manifest_digest"`
			Start  int64  `json:"start_ts"`
			End    *int64 `json:"end_ts"`
		} `json:"tags"`
		HasAdditional bool `json:"has_additional"`
	}
	if err := json.Unmarshal(body, &result); err != nil || result.Tags == nil || result.HasAdditional {
		return observation, errors.New("quay returned an incomplete active tag list")
	}
	now := time.Now()
	for _, record := range result.Tags {
		if record.Name != tag || (record.End != nil && *record.End <= now.Unix()) {
			continue
		}
		if observation.tagDigest != "" || record.Start <= 0 || !digestPattern.MatchString(record.Digest) {
			return registryObservation{}, errors.New("quay did not return a unique, valid active tag mapping")
		}
		observation.tagDigest = record.Digest
		observation.tagUpdatedAt = time.Unix(record.Start, 0)
	}
	observation.at = now
	observation.present = release.No
	if observation.tagDigest != "" {
		observation.present = release.Yes
	}
	return observation, nil
}
