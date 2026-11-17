package app

import (
	"bytes"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"release-assistant/internal/release"
)

func TestLogPrintsFullHistoryToConsole(t *testing.T) {
	var console bytes.Buffer
	log := NewLog(&console)
	for index := range maxRecentRecords + 3 {
		record := Record{
			At:      time.Date(2026, time.November, 17, 12, 0, index, 0, time.UTC),
			Release: "5.0.1", Target: release.Target{Repo: "stackrox/stackrox"},
			SHA: "abc123", Decision: "proposal", Payload: "/konflux-retest job\nfull payload",
			Result: strconv.Itoa(index), Origin: "system",
		}
		log.Append(record)
	}
	if got := strings.Count(console.String(), "[proposal]"); got != maxRecentRecords+3 {
		t.Fatalf("console printed %d events, want %d", got, maxRecentRecords+3)
	}
	want := "2026-11-17T12:00:00Z [proposal] stackrox/stackrox release=5.0.1 sha=abc123 origin=system: 0\n    Payload: /konflux-retest job\n    full payload\n"
	if !strings.HasPrefix(console.String(), want) {
		t.Fatalf("console output does not start with %q: %s", want, console.String())
	}
	if log.Error() != "" || len(log.Recent()) != maxRecentRecords {
		t.Fatal("console output changed recent history or log status")
	}
}

func TestLogFormatsTargetsAndMultilineResults(t *testing.T) {
	for name, tc := range map[string]struct {
		target release.Target
		want   string
	}{
		"should show PR number":    {release.Target{Repo: "stackrox/fact", PR: 7}, " stackrox/fact#7"},
		"should show tag":          {release.Target{Repo: "stackrox/fact", Tag: "v0.2.1"}, " stackrox/fact@v0.2.1"},
		"should omit empty target": {},
	} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			log := NewLog(&output)
			log.Append(Record{At: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC), Target: tc.target, Decision: "checks-error", Result: "Read failed\nRetry later"})
			want := "2026-11-17T12:00:00Z [checks-error]" + tc.want + ": Read failed\n    Retry later\n"
			if output.String() != want {
				t.Fatalf("output = %q, want %q", output.String(), want)
			}
		})
	}
}

func TestRecentLogIsBoundedAndCopied(t *testing.T) {
	log := NewLog(io.Discard)
	for index := range maxRecentRecords + 3 {
		log.Append(Record{Result: strconv.Itoa(index)})
	}
	entries := log.Recent()
	if len(entries) != maxRecentRecords || entries[0].Result != "3" || entries[len(entries)-1].Result != strconv.Itoa(maxRecentRecords+2) {
		t.Fatal("recent log did not preserve the latest bounded history")
	}
	entries[0].Result = "modified"
	if log.Recent()[0].Result == "modified" {
		t.Fatal("recent log shares its backing slice")
	}
}

func TestFailedLogWriteRetainsConsoleEntry(t *testing.T) {
	log := NewLog(badLog{})
	log.Append(Record{Decision: "github-error", Result: "GitHub rejected the request (HTTP 401)"})
	if len(log.Recent()) != 1 || log.Error() == "" {
		t.Fatal("log failure hid the diagnostic event")
	}
}
