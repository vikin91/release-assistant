package release

import "fmt"

// DefaultsFor returns the component inputs known for an ACS minor release.
// Returned sources omit Branch so callers derive it from the final stream.
// A nil result means no mapping is known; explicit configuration still works.
func DefaultsFor(acsVersion string) []Source {
	major, minor, _, ok := Version(acsVersion)
	if !ok {
		return nil
	}
	streams, ok := streamsFor(major, minor)
	if !ok {
		return nil
	}
	sources := []Source{
		{Name: "fact", Repo: "stackrox/fact", Stream: streams.fact},
		{Name: "collector", Repo: "stackrox/collector", Stream: streams.collector},
	}
	if streams.scanner != "" {
		sources = append(sources, Source{Name: "scanner", Repo: "stackrox/scanner", Stream: streams.scanner})
	}
	return sources
}

type componentStreams struct{ fact, collector, scanner string }

// 4.x mappings are explicit because the component streams do not follow one rule.
var acs4Streams = map[string]componentStreams{
	"4.10": {fact: "0.2", collector: "3.24", scanner: "2.39"},
	"4.11": {fact: "0.3", collector: "3.25", scanner: "2.40"},
}

// 5.x follows the observed pattern: 5.0 uses 0.4/3.26 and each minor adds one.
func streamsFor(major, minor int) (componentStreams, bool) {
	switch major {
	case 4:
		streams, ok := acs4Streams[fmt.Sprintf("%d.%d", major, minor)]
		return streams, ok
	case 5:
		return componentStreams{
			fact:      fmt.Sprintf("0.%d", minor+4),
			collector: fmt.Sprintf("3.%d", minor+26),
		}, true
	default:
		return componentStreams{}, false
	}
}
