package github

import (
	"encoding/json"
	"strings"
)

type (
	discoveryCycleKey struct{}
	discoveryCycle    struct {
		reads map[string]cachedRead
	}
	cachedRead struct {
		data json.RawMessage
		err  error
	}
)

// cacheableObservation excludes mutable PR identities and tag references.
func cacheableObservation(endpoint string) bool {
	return strings.Contains(endpoint, "/commits/") ||
		strings.Contains(endpoint, "/contents/") ||
		strings.HasSuffix(endpoint, "/protection/required_status_checks")
}
