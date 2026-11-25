package images

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"release-assistant/internal/release"
)

const (
	maxRegistryBody     = 1 << 20
	registryConcurrency = 4
)

type (
	registryClient struct {
		baseURL  string
		http     *http.Client
		username string
		password string

		confirmedPresence sync.Map // registryProbe to confirmed registryObservation
		probes            chan struct{}
		retryAfter        atomic.Int64
	}

	registryProbe struct {
		reference      string
		componentImage bool
	}

	registryObservation struct {
		present          release.Truth
		at, created      time.Time
		digest, platform string
	}

	manifestDescriptor struct {
		MediaType string          `json:"mediaType"`
		Digest    string          `json:"digest"`
		Size      *int64          `json:"size"`
		Platform  json.RawMessage `json:"platform"`
	}

	imageManifest struct {
		SchemaVersion int                  `json:"schemaVersion"`
		MediaType     string               `json:"mediaType"`
		Config        manifestDescriptor   `json:"config"`
		Layers        []manifestDescriptor `json:"layers"`
		Manifests     []manifestDescriptor `json:"manifests"`
	}
)

var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// newRegistryClient keeps credentials scoped to the Quay token request and disables redirects.
func newRegistryClient(baseURL string, client *http.Client, username, password string) *registryClient {
	if client == nil {
		client = http.DefaultClient
	}
	copy := *client
	copy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &registryClient{
		baseURL: strings.TrimRight(baseURL, "/"), http: &copy, username: username, password: password,
		probes: make(chan struct{}, registryConcurrency),
	}
}

// present reuses confirmed presence for the life of the client because release images rarely disappear.
func (c *registryClient) present(ctx context.Context, reference string) (release.Truth, time.Time, error) {
	observation, err := c.observe(ctx, registryProbe{reference: reference}, false)
	return observation.present, observation.at, err
}

func (c *registryClient) confirmed(probe registryProbe) (registryObservation, bool) {
	if observation, ok := c.confirmedPresence.Load(probe); ok {
		return observation.(registryObservation), true
	}
	return registryObservation{}, false
}

func (c *registryClient) observe(ctx context.Context, probe registryProbe, force bool) (registryObservation, error) {
	if !force && !probe.componentImage {
		if observation, ok := c.confirmed(probe); ok {
			return observation, nil
		}
	}
	select {
	case c.probes <- struct{}{}:
		defer func() { <-c.probes }()
	case <-ctx.Done():
		return registryObservation{at: time.Now()}, ctx.Err()
	}
	observation, err := c.checkManifest(ctx, probe)
	if observation.at.IsZero() {
		observation.at = time.Now()
	}
	if probe.componentImage {
		return observation, err
	}
	if force {
		if err == nil && observation.present == release.Yes {
			c.confirmedPresence.Store(probe, observation)
		} else {
			c.confirmedPresence.Delete(probe)
		}
	} else if err == nil && observation.present == release.Yes {
		confirmed, _ := c.confirmedPresence.LoadOrStore(probe, observation)
		observation = confirmed.(registryObservation)
	}
	return observation, err
}

func (c *registryClient) checkManifest(ctx context.Context, probe registryProbe) (registryObservation, error) {
	var observation registryObservation
	repository, tag, ok := splitReference(probe.reference)
	if !ok {
		return observation, errors.New("expected image reference is invalid")
	}
	address := c.baseURL + "/v2/" + repository + "/manifests/" + url.PathEscape(tag)
	response, err := c.request(ctx, address, "", manifestAccept)
	if err != nil {
		return observation, err
	}
	authorization := ""
	if response.StatusCode == http.StatusUnauthorized {
		challenge := response.Header.Get("WWW-Authenticate")
		_ = response.Body.Close()
		anonymous := probe.componentImage && strings.HasPrefix(repository, "stackrox-io/")
		token, err := c.registryToken(ctx, challenge, repository, anonymous)
		if err != nil {
			return observation, err
		}
		authorization = "Bearer " + token
		response, err = c.request(ctx, address, authorization, manifestAccept)
		if err != nil {
			return observation, err
		}
	}
	defer func() { _ = response.Body.Close() }()

	data, err := readBody(response.Body)
	if err != nil {
		return observation, err
	}
	switch response.StatusCode {
	case http.StatusOK:
		var manifest imageManifest
		if err := json.Unmarshal(data, &manifest); err != nil || !validManifest(manifest) {
			return observation, errors.New("registry returned a malformed or unsupported image manifest")
		}
		observation.present = release.Yes
		observation.at = time.Now()
		if probe.componentImage {
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			observation.digest = fmt.Sprintf("sha256:%x", sha256.Sum256(data))
			observation.created, observation.platform = c.imageCreation(ctx, repository, authorization, data, manifest)
		}
		return observation, nil
	case http.StatusNotFound:
		if hasMissingCode(data) {
			observation.present = release.No
			return observation, nil
		}
		return observation, errors.New("registry returned an unclassified not-found response; image availability is unknown")
	case http.StatusUnauthorized, http.StatusForbidden:
		return observation, fmt.Errorf("registry denied access (HTTP %d); image availability is unknown", response.StatusCode)
	default:
		return observation, fmt.Errorf("registry returned HTTP %d; image availability is unknown", response.StatusCode)
	}
}

func validManifest(manifest imageManifest) bool {
	if manifest.SchemaVersion != 2 {
		return false
	}
	switch manifest.MediaType {
	case "application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.list.v2+json":
		if len(manifest.Manifests) == 0 {
			return false
		}
		for _, descriptor := range manifest.Manifests {
			if !validDescriptor(descriptor) || !validImageManifestType(descriptor.MediaType) {
				return false
			}
		}
		return true
	case "application/vnd.oci.image.manifest.v1+json", "application/vnd.docker.distribution.manifest.v2+json":
		if !validDescriptor(manifest.Config) || !validImageConfigType(manifest.Config.MediaType) || manifest.Layers == nil {
			return false
		}
		for _, descriptor := range manifest.Layers {
			if !validDescriptor(descriptor) || !validLayerType(descriptor.MediaType) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func validDescriptor(descriptor manifestDescriptor) bool {
	return descriptor.MediaType != "" && digestPattern.MatchString(descriptor.Digest) && descriptor.Size != nil && *descriptor.Size >= 0
}

func validImageManifestType(mediaType string) bool {
	return mediaType == "application/vnd.oci.image.manifest.v1+json" || mediaType == "application/vnd.docker.distribution.manifest.v2+json"
}

func validImageConfigType(mediaType string) bool {
	return mediaType == "application/vnd.oci.image.config.v1+json" || mediaType == "application/vnd.docker.container.image.v1+json"
}

func validLayerType(mediaType string) bool {
	switch mediaType {
	case "application/vnd.oci.image.layer.v1.tar", "application/vnd.oci.image.layer.v1.tar+gzip", "application/vnd.oci.image.layer.v1.tar+zstd",
		"application/vnd.oci.image.layer.nondistributable.v1.tar", "application/vnd.oci.image.layer.nondistributable.v1.tar+gzip", "application/vnd.oci.image.layer.nondistributable.v1.tar+zstd",
		"application/vnd.docker.image.rootfs.diff.tar", "application/vnd.docker.image.rootfs.diff.tar.gzip", "application/vnd.docker.image.rootfs.foreign.diff.tar.gzip":
		return true
	default:
		return false
	}
}

const manifestAccept = "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json"

func splitReference(reference string) (repository, tag string, ok bool) {
	if !strings.HasPrefix(reference, "quay.io/") {
		return "", "", false
	}
	remainder := strings.TrimPrefix(reference, "quay.io/")
	if repository, digest, found := strings.Cut(remainder, "@"); found {
		return repository, digest, validQuayRepo("quay.io/"+repository) && digestPattern.MatchString(digest)
	}
	separator := strings.LastIndexByte(remainder, ':')
	if separator <= 0 || separator == len(remainder)-1 {
		return "", "", false
	}
	repository, tag = remainder[:separator], remainder[separator+1:]
	return repository, tag, validQuayRepo("quay.io/"+repository) && imageTagPattern.MatchString(tag)
}

func (c *registryClient) request(ctx context.Context, address, authorization, accept string) (*http.Response, error) {
	if until := time.Unix(0, c.retryAfter.Load()); time.Now().Before(until) {
		return nil, fmt.Errorf("registry rate limited requests (HTTP 429) until %s", until.Format(time.RFC3339))
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, errors.New("invalid registry request")
	}
	request.Header.Set("Accept", accept)
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("registry network/TLS error or timeout; image availability is unknown")
	}
	if response.StatusCode == http.StatusTooManyRequests {
		_ = response.Body.Close()
		now := time.Now()
		until := now.Add(time.Minute)
		if value := response.Header.Get("Retry-After"); value != "" {
			if seconds, err := strconv.ParseUint(value, 10, 32); err == nil {
				until = now.Add(time.Duration(seconds) * time.Second)
			} else if date, err := http.ParseTime(value); err == nil {
				until = date
			}
		}
		for previous := c.retryAfter.Load(); until.UnixNano() > previous; previous = c.retryAfter.Load() {
			if c.retryAfter.CompareAndSwap(previous, until.UnixNano()) {
				break
			}
		}
		return nil, fmt.Errorf("registry rate limited requests (HTTP 429) until %s", until.Format(time.RFC3339))
	}
	return response, nil
}

func (c *registryClient) registryToken(ctx context.Context, challenge, repository string, anonymous bool) (string, error) {
	params, ok := parseBearerChallenge(challenge)
	if !ok {
		return "", errors.New("quay requires unsupported registry authentication; image availability is unknown")
	}
	if params["service"] != "quay.io" || params["scope"] != "repository:"+repository+":pull" {
		return "", errors.New("quay requested an unsupported token scope")
	}
	realm, err := url.Parse(params["realm"])
	base, baseErr := url.Parse(c.baseURL)
	if err != nil || baseErr != nil || realm.Scheme != base.Scheme || realm.Host != base.Host || realm.User != nil || realm.Fragment != "" {
		return "", errors.New("quay requested an unsupported token endpoint")
	}
	if base.Hostname() == "quay.io" && (realm.Scheme != "https" || realm.Path != "/v2/auth") {
		return "", errors.New("quay requested an unsupported token endpoint")
	}
	if !anonymous && (c.username == "") != (c.password == "") {
		return "", errors.New("quay token authentication requires both username and password")
	}
	query := realm.Query()
	query.Set("service", params["service"])
	query.Set("scope", params["scope"])
	realm.RawQuery = query.Encode()
	authorization := ""
	if !anonymous && c.username != "" {
		authorization = "Basic " + base64.StdEncoding.EncodeToString([]byte(c.username+":"+c.password))
	}
	response, err := c.request(ctx, realm.String(), authorization, "application/json")
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("quay token endpoint returned HTTP %d; image availability is unknown", response.StatusCode)
	}
	body, err := readBody(response.Body)
	if err != nil {
		return "", err
	}
	var tokenResponse struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tokenResponse); err != nil {
		return "", errors.New("quay token response is malformed")
	}
	if tokenResponse.Token != "" {
		return tokenResponse.Token, nil
	}
	if tokenResponse.AccessToken != "" {
		return tokenResponse.AccessToken, nil
	}
	return "", errors.New("quay token response contains no bearer token")
}

func parseBearerChallenge(challenge string) (map[string]string, bool) {
	if !strings.HasPrefix(challenge, "Bearer ") {
		return nil, false
	}
	params := make(map[string]string)
	for _, match := range bearerParamRE.FindAllStringSubmatch(strings.TrimPrefix(challenge, "Bearer "), -1) {
		params[match[1]] = match[2]
	}
	if params["realm"] == "" || params["service"] == "" || params["scope"] == "" {
		return nil, false
	}
	return params, true
}

func readBody(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxRegistryBody+1))
	if err != nil {
		return nil, errors.New("could not read registry response; image availability is unknown")
	}
	if len(data) > maxRegistryBody {
		return nil, errors.New("registry response exceeds the metadata size limit")
	}
	return data, nil
}

func hasMissingCode(data []byte) bool {
	var response struct {
		Errors []struct {
			Code string `json:"code"`
		} `json:"errors"`
	}
	if json.Unmarshal(data, &response) != nil {
		return false
	}
	for _, detail := range response.Errors {
		if detail.Code == "MANIFEST_UNKNOWN" || detail.Code == "NAME_UNKNOWN" {
			return true
		}
	}
	return false
}
