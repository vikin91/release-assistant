package images

import (
	"net/http"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"release-assistant/internal/release"
)

func TestImageChecksPublishBeforeTheSlowestProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		unblock := make(chan struct{})
		defer close(unblock)
		proceed := make(chan struct{})
		client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls.Add(1)
			select {
			case <-proceed:
			case <-unblock:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
			return response(http.StatusOK, validImageManifest, nil), nil
		})}
		inspector := New(client, "", "")
		tags := map[string]string{"fact": "0.3.77", "collector": "0.4.20", "acs": "5.0.1-rc.0"}
		updates := make(chan release.Inventory, 64)
		done := make(chan error, 1)
		go func() {
			_, err := inspector.InspectImages(t.Context(), configFor("5.0.1"), tags, false, func(partial release.Inventory) {
				updates <- partial
			})
			done <- err
		}()
		synctest.Wait()
		if got := calls.Load(); got != registryConcurrency {
			t.Fatalf("got %d concurrent requests, want %d", got, registryConcurrency)
		}
		initial := <-updates
		if len(initial.Images) == 0 || len(initial.ReleaseImages) == 0 {
			t.Fatal("the first update should publish the complete catalog")
		}
		proceed <- struct{}{}
		synctest.Wait()
		partial := <-updates
		verified := 0
		for _, rows := range [][]release.Image{partial.Images, partial.ReleaseImages} {
			for _, row := range rows {
				if row.Evidence.Fresh() && row.Present == release.Yes {
					verified++
				}
			}
		}
		if verified != 1 || len(done) != 0 {
			t.Fatalf("got %d verified observations while other requests were in flight, want 1", verified)
		}
	})
}

func TestComponentImagePollsRecheckTheCurrentTag(t *testing.T) {
	var calls atomic.Int32
	var missing atomic.Bool
	inspector := New(testHTTPClient(func(*http.Request) *http.Response {
		calls.Add(1)
		if missing.Load() {
			return response(http.StatusNotFound, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`, nil)
		}
		return response(http.StatusOK, validImageManifest, nil)
	}), "", "")
	config := configFor("5.0.1")
	tags := map[string]string{"fact": "0.3.77", "collector": "0.4.20"}
	for _, step := range []struct {
		force, missing bool
		calls          int32
		presence       release.Truth
	}{
		{calls: 6, presence: release.Yes},
		{calls: 6, presence: release.Yes},
		{force: true, missing: true, calls: 6, presence: release.No},
		{missing: true, calls: 6, presence: release.No},
	} {
		missing.Store(step.missing)
		before := calls.Load()
		got, err := inspector.InspectImages(t.Context(), config, tags, step.force, nil)
		if err != nil {
			t.Fatal(err)
		}
		if n := calls.Load() - before; n != step.calls || got.Images[0].Present != step.presence {
			t.Fatalf("force=%t got %d probes and presence %q, want %d probes and %q",
				step.force, n, got.Images[0].Present, step.calls, step.presence)
		}
	}
}

func TestQuayThrottlingDefersUncachedProbes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		inspector := New(testHTTPClient(func(*http.Request) *http.Response {
			calls++
			if calls == 1 {
				return response(http.StatusTooManyRequests, "", http.Header{"Retry-After": {"5"}})
			}
			return response(http.StatusOK, validImageManifest, nil)
		}), "", "")
		reference := "quay.io/test/release-main:5.0.1"
		for range 2 {
			image, err := inspector.InspectReference(t.Context(), reference)
			if err == nil || image.Evidence.Fresh() || calls != 1 {
				t.Fatalf("throttled check = %+v, err = %v, calls = %d", image, err, calls)
			}
		}
		time.Sleep(5 * time.Second)
		image, err := inspector.InspectReference(t.Context(), reference)
		if err != nil || image.Present != release.Yes || calls != 2 {
			t.Fatalf("resumed check = %+v, err = %v, calls = %d", image, err, calls)
		}
	})
}
