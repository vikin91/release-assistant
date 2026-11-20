package images

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"release-assistant/internal/release"
)

func TestInspectReferenceUsesDigestManifestEndpoint(t *testing.T) {
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	reference := "quay.io/rhacs-eng/release-fact@" + digest
	wantPath := "/v2/rhacs-eng/release-fact/manifests/sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	requests := 0
	inspector := New(testHTTPClient(func(request *http.Request) *http.Response {
		requests++
		if request.Method != http.MethodGet || request.URL.Host != "quay.io" || request.URL.EscapedPath() != wantPath {
			t.Errorf("manifest request = %s %s, want GET https://quay.io%s", request.Method, request.URL, wantPath)
		}
		return response(http.StatusOK, validImageManifest, nil)
	}), "", "")

	image, err := inspector.InspectReference(t.Context(), reference)
	if err != nil {
		t.Fatalf("InspectReference() error = %v", err)
	}
	if requests != 1 {
		t.Fatalf("registry requests = %d, want 1", requests)
	}
	if image.Component != "acs" || image.Family != konfluxFamily || image.Name != "fact" || image.Reference != reference || image.Present != release.Yes {
		t.Errorf("InspectReference() image = %#v, want observed ACS Konflux image", image)
	}
	if image.Evidence.URL != "https://quay.io"+wantPath || !image.Evidence.Known || image.Evidence.Error != "" || image.Evidence.At.IsZero() {
		t.Errorf("InspectReference() evidence = %#v, want known digest observation at exact manifest URL", image.Evidence)
	}
}

func TestInspectReferenceReportsConfirmedManifestAbsence(t *testing.T) {
	requests := 0
	inspector := New(testHTTPClient(func(request *http.Request) *http.Response {
		requests++
		return response(http.StatusNotFound, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`, nil)
	}), "", "")

	image, err := inspector.InspectReference(t.Context(), "quay.io/rhacs-eng/fact:0.3.77")
	if err != nil {
		t.Fatalf("InspectReference() error = %v, want confirmed absence without error", err)
	}
	if requests != 1 || image.Present != release.No || !image.Evidence.Known || image.Evidence.Error != "" {
		t.Fatalf("requests=%d image=%#v, want one known absent observation", requests, image)
	}
}

func TestInspectReferenceCachesOnlyConfirmedPresence(t *testing.T) {
	for name, tc := range map[string]struct {
		reference string
		status    int
		body      string
		presence  release.Truth
		wantErr   bool
	}{
		"should recheck a missing tag": {
			reference: "quay.io/rhacs-eng/fact:0.3.77",
			status:    http.StatusNotFound,
			body:      `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`,
			presence:  release.No,
		},
		"should recheck an unverified digest": {
			reference: "quay.io/rhacs-eng/release-fact@sha256:" + strings.Repeat("a", 64),
			status:    http.StatusOK,
			body:      `{}`,
			wantErr:   true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			requests := 0
			inspector := New(testHTTPClient(func(*http.Request) *http.Response {
				requests++
				if requests == 1 {
					return response(tc.status, tc.body, nil)
				}
				return response(http.StatusOK, validImageManifest, nil)
			}), "", "")

			first, err := inspector.InspectReference(t.Context(), tc.reference)
			if (err != nil) != tc.wantErr || first.Present != tc.presence {
				t.Fatalf("first inspection = %#v, err = %v, want presence %q and error %t", first, err, tc.presence, tc.wantErr)
			}
			verified, err := inspector.InspectReference(t.Context(), tc.reference)
			if err != nil || verified.Present != release.Yes || !verified.Evidence.Fresh() {
				t.Fatalf("second inspection = %#v, err = %v, want verified presence", verified, err)
			}
			cached, err := inspector.InspectReference(t.Context(), tc.reference)
			if err != nil || cached != verified || requests != 2 {
				t.Fatalf("third inspection = %#v, err = %v, requests = %d, want the verified result with two requests total", cached, err, requests)
			}
		})
	}
}

func TestInspectReferenceKeepsRegistryFailuresUnknown(t *testing.T) {
	for _, tt := range []struct {
		name    string
		status  int
		body    string
		request func(*http.Request) (*http.Response, error)
	}{
		{name: "forbidden", status: http.StatusForbidden, body: "denied"},
		{name: "unauthorized", status: http.StatusUnauthorized, body: "login required"},
		{name: "malformed manifest", status: http.StatusOK, body: "not json"},
		{name: "unsupported manifest", status: http.StatusOK, body: `{}`},
		{name: "transport failure", request: func(*http.Request) (*http.Response, error) {
			return nil, errors.New("connection refused")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			requests := 0
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				requests++
				if tt.request != nil {
					return tt.request(request)
				}
				return responseFor(request, tt.status, tt.body, nil), nil
			})}
			image, err := New(client, "", "").InspectReference(t.Context(), "quay.io/rhacs-eng/fact:0.3.77")
			if err == nil {
				t.Fatal("InspectReference() error = nil, want registry observation error")
			}
			if requests != 1 || image.Present != "" || image.Evidence.Known || image.Evidence.Error == "" {
				t.Fatalf("requests=%d image=%#v, want one unknown observation with an error", requests, image)
			}
		})
	}
}

func TestInspectReferenceRejectsInvalidReferencesBeforeRegistryAccess(t *testing.T) {
	for _, tt := range []struct {
		name      string
		reference string
	}{
		{name: "other registry", reference: "registry.example/rhacs-eng/fact:0.3.77"},
		{name: "missing tag or digest", reference: "quay.io/rhacs-eng/fact"},
		{name: "malformed digest", reference: "quay.io/rhacs-eng/fact@sha256:abcd"},
		{name: "uppercase digest", reference: "quay.io/rhacs-eng/fact@sha256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
		{name: "invalid tag path", reference: "quay.io/rhacs-eng/fact:0.3.77/extra"},
		{name: "traversal path", reference: "quay.io/rhacs-eng/../fact:0.3.77"},
		{name: "empty path segment", reference: "quay.io/rhacs-eng//fact:0.3.77"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			requests := 0
			inspector := New(testHTTPClient(func(*http.Request) *http.Response {
				requests++
				return response(http.StatusOK, validImageManifest, nil)
			}), "", "")
			image, err := inspector.InspectReference(t.Context(), tt.reference)
			if err == nil || requests != 0 {
				t.Fatalf("InspectReference() error=%v requests=%d, want rejected reference without network", err, requests)
			}
			if image.Reference != tt.reference || image.Present != "" || image.Evidence.Known || image.Evidence.Error == "" {
				t.Errorf("invalid reference result = %#v, want preserved reference and unknown evidence", image)
			}
			if strings.Contains(image.Evidence.URL, "/manifests/") {
				t.Errorf("invalid reference produced manifest evidence URL %q", image.Evidence.URL)
			}
		})
	}
}
