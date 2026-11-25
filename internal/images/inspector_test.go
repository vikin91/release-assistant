package images

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"release-assistant/internal/release"
)

const (
	validImageManifest = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","size":1},"layers":[]}`
	validImageIndex    = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","size":1}]}`
)

func TestInspectUsesSavedComponentPatternsForSupportedStreams(t *testing.T) {
	for _, tt := range []struct {
		name, version, scannerTag string
		wantCount                 int
	}{
		{name: "4.10", version: "4.10.0", scannerTag: "4.10.1", wantCount: 18},
		{name: "4.11", version: "4.11.2", scannerTag: "4.11.3", wantCount: 18},
		{name: "5.0", version: "5.0.1", wantCount: 6},
	} {
		t.Run(tt.name, func(t *testing.T) {
			requests := 0
			inspector := New(testHTTPClient(func(request *http.Request) *http.Response {
				requests++
				manifest := strings.HasPrefix(request.URL.Path, "/v2/rhacs-eng/")
				publicManifest := strings.HasPrefix(request.URL.Path, "/v2/stackrox-io/")
				if request.URL.Host != "quay.io" || !manifest && !publicManifest {
					t.Errorf("unexpected non-Quay registry request: %s", request.URL)
				}
				return response(http.StatusOK, validImageManifest, nil)
			}), "", "")
			config := configFor(tt.version)
			tags := map[string]string{"fact": "0.3.77", "collector": "v0.4.20"}
			if tt.name != "5.0" {
				tags["scanner"] = tt.scannerTag
			}
			inventory, err := inspector.InspectComponents(t.Context(), config, tags)
			if err != nil {
				t.Fatalf("Inspect() error = %v", err)
			}
			wantRequests := tt.wantCount
			if inventory.ReleaseVersion != tt.version || len(inventory.Images) != tt.wantCount || requests != wantRequests {
				t.Fatalf("release=%s images=%d requests=%d, want release=%s images=%d requests=%d", inventory.ReleaseVersion, len(inventory.Images), requests, tt.version, tt.wantCount, wantRequests)
			}
			if !inventory.Evidence.Fresh() {
				t.Fatalf("pattern catalog completeness was not recorded: %#v", inventory.Evidence)
			}
			want := componentReferences(tags)
			for _, image := range inventory.Images {
				if _, ok := want[image.Reference]; !ok {
					t.Errorf("unexpected image reference %q", image.Reference)
				}
				delete(want, image.Reference)
				if image.Present != release.Yes || !image.Evidence.Fresh() || image.Name == "" ||
					image.Evidence.URL != sourceURL(image.Reference) {
					t.Errorf("image result is incomplete: %#v", image)
				}
			}
			for reference := range want {
				t.Errorf("expected reference was not checked: %s", reference)
			}
		})
	}
}

func TestInspectValidatesTagsBeforeRegistryAccess(t *testing.T) {
	for _, tt := range []struct {
		name, version, tag string
	}{
		{name: "missing tag", version: "4.10.0"},
		{name: "wrong component stream", version: "4.10.0", tag: "v0.4.1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			requests := 0
			inspector := New(testHTTPClient(func(*http.Request) *http.Response {
				requests++
				return response(http.StatusOK, validImageManifest, nil)
			}), "", "")
			inventory, err := inspector.Inspect(t.Context(), configFor(tt.version), map[string]string{"fact": tt.tag, "collector": "0.4.20", "scanner": "4.10.1"})
			wantError := tt.tag != ""
			wantRequests := 15 + len(inventory.ReleaseImages)
			if (err != nil) != wantError || requests != wantRequests {
				t.Fatalf("Inspect() error=%v requests=%d, want error=%t and %d requests", err, requests, wantError, wantRequests)
			}
			if len(inventory.Images) != 18 {
				t.Fatalf("got %d rows, want all 18 expected image rows", len(inventory.Images))
			}
			if inventory.Evidence.Known || (inventory.Evidence.Error != "") != wantError {
				t.Fatalf("incomplete expected references were reported complete: %#v", inventory.Evidence)
			}
			for _, image := range inventory.Images {
				if image.Component == "fact" && (image.Reference != "" || !strings.Contains(image.Evidence.Error, "select a fact tag")) {
					t.Errorf("invalid selection produced a reference or wrong error: %#v", image)
				}
			}
		})
	}
}

func TestInspectRejectsUnverifiedStreamWithoutNetwork(t *testing.T) {
	requests := 0
	config := configFor("6.0.0")
	inventory, err := New(testHTTPClient(func(*http.Request) *http.Response {
		requests++
		return response(http.StatusOK, validImageManifest, nil)
	}), "", "").Inspect(t.Context(), config, map[string]string{"fact": "0.3.1", "collector": "0.4.1"})
	if err == nil || requests != 0 {
		t.Fatalf("Inspect() err=%v requests=%d, want rejected unsupported stream before network", err, requests)
	}
	if len(inventory.Images) != 6 {
		t.Fatalf("got %d unknown rows, want six named rows", len(inventory.Images))
	}
}

func TestInspectAcceptsConfiguredComponentStreamsIndependentOfACS(t *testing.T) {
	for _, tt := range []struct {
		version string
		streams map[string]string
		tags    map[string]string
	}{
		{
			version: "4.11.1",
			streams: map[string]string{"fact": "1.2", "collector": "3.26", "scanner": "2.40"},
			tags:    map[string]string{"fact": "1.2.3", "collector": "3.26.5", "scanner": "2.40.1"},
		},
		{
			version: "5.0.1",
			streams: map[string]string{"fact": "0.4", "collector": "3.26"},
			tags:    map[string]string{"fact": "0.4.7", "collector": "3.26.5"},
		},
	} {
		t.Run(tt.version, func(t *testing.T) {
			config := configFor(tt.version)
			for index := range config.Components {
				config.Components[index].Stream = tt.streams[config.Components[index].Name]
			}
			inspector := New(testHTTPClient(func(*http.Request) *http.Response {
				return response(http.StatusOK, validImageManifest, nil)
			}), "", "")
			inventory, err := inspector.Inspect(t.Context(), config, tt.tags)
			if err != nil {
				t.Fatalf("Inspect() error = %v", err)
			}
			if len(inventory.Images) != len(componentReferences(tt.tags)) {
				t.Fatalf("got %d rows, want %d", len(inventory.Images), len(componentReferences(tt.tags)))
			}
			for _, image := range inventory.Images {
				if image.Present != release.Yes {
					t.Errorf("configured component stream did not resolve: %#v", image)
				}
			}
		})
	}
}

func TestInspectKeepsIndependentRegistryResults(t *testing.T) {
	inspector := New(testHTTPClient(func(request *http.Request) *http.Response {
		if strings.HasSuffix(request.URL.Path, "/manifests/0.3.77") {
			return response(http.StatusForbidden, "denied", nil)
		}
		return response(http.StatusOK, validImageManifest, nil)
	}), "", "")
	inventory, err := inspector.Inspect(t.Context(), configFor("5.0.0"), map[string]string{"fact": "0.3.77", "collector": "0.4.20"})
	if err == nil || !inventory.Evidence.Fresh() || len(inventory.Images) != 6 {
		t.Fatalf("partial inventory=%#v err=%v", inventory, err)
	}
	var factUnknown, collectorKnown bool
	for _, image := range inventory.Images {
		if image.Component == "fact" && image.Present == "" && strings.Contains(image.Evidence.Error, "availability is unknown") {
			factUnknown = true
		}
		if image.Component == "collector" && image.Present == release.Yes {
			collectorKnown = true
		}
	}
	if !factUnknown || !collectorKnown {
		t.Fatalf("failed row hid independent result: factUnknown=%t collectorKnown=%t", factUnknown, collectorKnown)
	}
}

func TestInspectKeepsRegistryAbsenceDistinctFromUnknown(t *testing.T) {
	for _, tt := range []struct {
		name    string
		status  int
		body    string
		want    release.Truth
		wantErr bool
	}{
		{name: "manifest missing", status: http.StatusNotFound, body: `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`, want: release.No},
		{name: "repository missing", status: http.StatusNotFound, body: `{"errors":[{"code":"NAME_UNKNOWN"}]}`, want: release.No},
		{name: "unclassified not found", status: http.StatusNotFound, body: "not found", wantErr: true},
		{name: "unauthorized", status: http.StatusUnauthorized, wantErr: true},
		{name: "forbidden", status: http.StatusForbidden, wantErr: true},
		{name: "rate limited", status: http.StatusTooManyRequests, wantErr: true},
		{name: "server error", status: http.StatusBadGateway, wantErr: true},
		{name: "malformed manifest", status: http.StatusOK, body: "not json", wantErr: true},
		{name: "object without image schema", status: http.StatusOK, body: `{}`, wantErr: true},
		{name: "valid OCI index", status: http.StatusOK, body: validImageIndex, want: release.Yes},
		{name: "empty OCI index", status: http.StatusOK, body: `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`, wantErr: true},
		{name: "missing config descriptor", status: http.StatusOK, body: `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[]}`, wantErr: true},
		{name: "missing layers field", status: http.StatusOK, body: `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","size":1}}`, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			inspector := New(testHTTPClient(func(*http.Request) *http.Response {
				return response(tt.status, tt.body, nil)
			}), "", "")
			inventory, err := inspector.Inspect(t.Context(), configFor("5.0.0"), map[string]string{"fact": "0.3.77", "collector": "0.4.20"})
			if (err != nil) != tt.wantErr {
				t.Fatalf("Inspect() error = %v, wantErr=%t", err, tt.wantErr)
			}
			for _, image := range inventory.Images {
				if image.Present != tt.want {
					t.Errorf("image availability = %q, want %q: %#v", image.Present, tt.want, image)
				}
			}
		})
	}
}

func TestRegistryPreservesAuthenticationAndRedirectProtections(t *testing.T) {
	var manifestRequests int
	var basicSeen, redirectedRequest bool
	client := newRegistryClient("https://quay.io", &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/v2/rhacs-eng/fact/manifests/0.3.77":
			manifestRequests++
			if request.Header.Get("Authorization") == "Bearer fixture-token" {
				return responseFor(request, http.StatusTemporaryRedirect, "", http.Header{"Location": []string{"https://outside.example/capture"}}), nil
			}
			return responseFor(request, http.StatusUnauthorized, "", http.Header{"Www-Authenticate": []string{`Bearer realm="https://quay.io/v2/auth",service="quay.io",scope="repository:rhacs-eng/fact:pull"`}}), nil
		case "/v2/auth":
			user, password, ok := request.BasicAuth()
			basicSeen = ok && user == "user" && password == "password"
			return responseFor(request, http.StatusOK, `{"token":"fixture-token"}`, nil), nil
		default:
			redirectedRequest = request.URL.Host == "outside.example"
			return responseFor(request, http.StatusNotFound, "", nil), nil
		}
	})}, "user", "password")
	_, _, err := client.present(t.Context(), "quay.io/rhacs-eng/fact:0.3.77")
	if err == nil || !basicSeen || manifestRequests != 2 || redirectedRequest {
		t.Fatalf("redirect/auth protection failed: err=%v basic=%t manifests=%d followed=%t", err, basicSeen, manifestRequests, redirectedRequest)
	}
}

func TestRegistryRejectsUnexpectedTokenScope(t *testing.T) {
	requests := 0
	client := newRegistryClient("https://quay.io", &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		return responseFor(request, http.StatusUnauthorized, "", http.Header{"Www-Authenticate": []string{`Bearer realm="https://quay.io/v2/auth",service="quay.io",scope="repository:other/image:pull"`}}), nil
	})}, "username", "password")
	_, _, err := client.present(t.Context(), "quay.io/rhacs-eng/fact:0.3.77")
	if err == nil || requests != 1 {
		t.Fatalf("present() error=%v requests=%d, want unsupported scope and one request", err, requests)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func testHTTPClient(handler func(*http.Request) *http.Response) *http.Client {
	var mu sync.Mutex
	return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		result := handler(request)
		result.Request = request
		return result, nil
	})}
}

func response(status int, body string, headers http.Header) *http.Response {
	if headers == nil {
		headers = make(http.Header)
	}
	return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body))}
}

func responseFor(request *http.Request, status int, body string, headers http.Header) *http.Response {
	result := response(status, body, headers)
	result.Request = request
	return result
}

func configFor(version string) release.Config {
	major, minor, _, _ := release.Version(version)
	stream := strings.Join([]string{strconv.Itoa(major), strconv.Itoa(minor)}, ".")
	components := []release.Source{
		{Name: "fact", Repo: "some/override", Branch: "release-override", Stream: "0.3"},
		{Name: "collector", Repo: "some/override", Branch: "release-override", Stream: "0.4"},
	}
	if major == 4 {
		components = append(components, release.Source{Name: "scanner", Repo: "some/override", Branch: "release-override", Stream: stream})
	}
	return release.Config{Version: version, Branch: "release-override", Components: components}
}

func componentReferences(tags map[string]string) map[string]bool {
	want := make(map[string]bool)
	for component, tag := range tags {
		major, componentMinor, patch, ok := release.Version(tag)
		if !ok {
			continue
		}
		version := fmt.Sprintf("%d.%d.%d", major, componentMinor, patch)
		nameSet := []string{component}
		if component == "scanner" {
			nameSet = []string{"scanner", "scanner-db", "scanner-slim", "scanner-db-slim"}
		}
		for _, name := range nameSet {
			want["quay.io/rhacs-eng/"+name+":"+version] = true
			want["quay.io/stackrox-io/"+name+":"+version] = true
			want["quay.io/rhacs-eng/release-"+name+":"+version+"-fast"] = true
		}
	}
	return want
}

func TestInspectComponentsDefersEveryRCRequest(t *testing.T) {
	requests, rcRequests := 0, 0
	inspector := New(testHTTPClient(func(r *http.Request) *http.Response {
		requests++
		if strings.Contains(r.URL.Path, "-rc.0") {
			rcRequests++
		}
		return response(http.StatusOK, validImageManifest, nil)
	}), "", "")
	inventory, err := inspector.InspectComponents(t.Context(), configFor("4.10.12"), map[string]string{"fact": "0.3.77", "collector": "0.4.20", "scanner": "4.10.1"})
	if err != nil || requests != 18 || rcRequests != 0 || len(inventory.ReleaseImages) != 0 {
		t.Fatalf("requests=%d rc=%d error=%v", requests, rcRequests, err)
	}
	t.Log("18 component requests; 0 release candidate requests")
}

func BenchmarkImagePhaseRequests(b *testing.B) {
	for name, includeRC := range map[string]bool{"components": false, "components-and-RC": true} {
		b.Run(name, func(b *testing.B) {
			requests := 0
			inspector := New(testHTTPClient(func(*http.Request) *http.Response {
				requests++
				return response(http.StatusOK, validImageManifest, nil)
			}), "", "")
			config := configFor("4.10.12")
			tags := map[string]string{"fact": "0.3.77", "collector": "0.4.20", "scanner": "4.10.1"}
			for b.Loop() {
				_, err := inspector.inspect(b.Context(), config, tags, includeRC)
				if err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(requests)/float64(b.N), "requests/op")
		})
	}
}
