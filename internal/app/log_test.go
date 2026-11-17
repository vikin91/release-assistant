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
	if got := strings.Count(console.String(), " Info: "); got != maxRecentRecords+3 {
		t.Fatalf("console printed %d events, want %d", got, maxRecentRecords+3)
	}
	want := "2026-11-17T12:00:00Z Info: 0\n2026-11-17T12:00:00Z Debug: Retry comment for stackrox:\n    /konflux-retest job\n    full payload\n"
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
		"should show PR number":    {release.Target{Repo: "stackrox/fact", PR: 7}, "fact#7"},
		"should show tag":          {release.Target{Repo: "stackrox/fact", Tag: "v0.2.1"}, "fact/v0.2.1"},
		"should omit empty target": {want: "the release"},
	} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			log := NewLog(&output)
			log.Append(Record{At: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC), Target: tc.target, Decision: "checks-error", Result: "Read failed\nRetry later"})
			want := "2026-11-17T12:00:00Z Error: Could not read build results in " + tc.want + ". Read failed\n    Retry later\n"
			if !strings.HasPrefix(output.String(), want) {
				t.Fatalf("output = %q, want prefix %q", output.String(), want)
			}
		})
	}
}

func TestLogOmitsRoutineConsoleNoiseWithoutChangingUIHistory(t *testing.T) {
	for name, record := range map[string]Record{
		"should omit request count summaries":   {Decision: "work-complete", Result: "Phase=prs; GitHub reads=9."},
		"should omit generic freshness reports": {Decision: "observation", Result: "current=true"},
	} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			log := NewLog(&output)
			log.Append(record)
			if output.Len() != 0 {
				t.Fatalf("unexpected console noise: %s", output.String())
			}
			if got := log.Recent(); len(got) != 1 || got[0] != record {
				t.Fatal("console filtering changed UI history")
			}
		})
	}
	var output bytes.Buffer
	NewLog(&output).Append(Record{Decision: "configure", Release: "4.10.12", SHA: "ambiguous-commit", Origin: "system"})
	if strings.Contains(output.String(), "Context for") || strings.Contains(output.String(), "ambiguous-commit") {
		t.Fatalf("unexplained context leaked into the log: %s", output.String())
	}
}

func TestLogLabelsCommitRoles(t *testing.T) {
	for name, tc := range map[string]struct {
		target release.Target
		want   string
	}{
		"should identify PR head commits": {release.Target{Repo: "stackrox/fact", PR: 7}, "fact#7 (pull request head commit abc123)"},
		"should identify tag commits":     {release.Target{Repo: "stackrox/fact", Tag: "v0.2.1"}, "fact/v0.2.1 (tag commit abc123)"},
	} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			NewLog(&output).Append(Record{Decision: "dispatch", Target: tc.target, SHA: "abc123"})
			if !strings.Contains(output.String(), "Posting a retry comment in "+tc.want+".\n") {
				t.Fatalf("unexplained commit in log: %s", output.String())
			}
		})
	}
}

func TestDiscoverySummaryNamesBranchAndRequestedData(t *testing.T) {
	for name, tc := range map[string]struct {
		scope DiscoveryScope
		want  string
	}{
		"should report only requested PRs":  {ReadPRs, "Found 4 pull requests while checking fact/release-0.2. Branch head of fact/release-0.2 is branch-head."},
		"should report only requested tags": {ReadTags, "Found 0 release tags while checking fact/release-0.2. Branch head of fact/release-0.2 is branch-head."},
		"should report both when requested": {ReadPRs | ReadTags, "Found 4 pull requests and 0 release tags while checking fact/release-0.2. Branch head of fact/release-0.2 is branch-head."},
	} {
		t.Run(name, func(t *testing.T) {
			request := DiscoveryRequest{Source: release.Source{Repo: "stackrox/fact", Branch: "release-0.2"}, Scope: tc.scope}
			repo := release.Repository{Head: "branch-head", PRs: make([]release.PR, 4)}
			if got := discoverySummary(request, repo, false); got != tc.want {
				t.Fatalf("summary = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLogPrintsSeverityLevels(t *testing.T) {
	for name, tc := range map[string]struct {
		decision, result, level string
	}{
		"should report read failures as errors":           {"github-error", "Read failed", "Error"},
		"should report image failures as errors":          {"pinning-images-error", "Registry unavailable", "Error"},
		"should report failed deliveries as errors":       {"delivery", "failed", "Error"},
		"should warn about uncertain deliveries":          {"delivery", "uncertain", "Warn"},
		"should warn about retry waits":                   {"retry-wait", "Retry later", "Warn"},
		"should warn about invalidated tags":              {"tag-invalidated", "Commit changed", "Warn"},
		"should warn about revoked authority":             {"revoked", "Identity changed", "Warn"},
		"should warn about withheld dispatch":             {"withheld", "stale", "Warn"},
		"should report normal activity as info":           {"configure", "New selection", "Info"},
		"should report recovery as info":                  {"github-recovered", "Reads recovered", "Info"},
		"should report successful deliveries as info":     {"delivery", "sent", "Info"},
		"should report completed user selections as info": {"selection-complete", "Selection confirmed", "Info"},
		"should report discovery starts as info":          {"github-start", "Reading GitHub", "Info"},
		"should report check diagnostics as debug":        {"checks", "No actions", "Debug"},
		"should report available images as info":          {"image", "yes", "Info"},
		"should report missing images as warnings":        {"image", "no", "Warn"},
		"should report unknown image status as debug":     {"image", "", "Debug"},
	} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			log := NewLog(&output)
			record := Record{At: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC), Decision: tc.decision, Result: tc.result}
			log.Append(record)
			want := "2026-11-17T12:00:00Z " + tc.level + ": "
			if !strings.HasPrefix(output.String(), want) {
				t.Fatalf("output = %q, want prefix %q", output.String(), want)
			}
			if got := log.Recent(); len(got) != 1 || got[0] != record {
				t.Fatalf("console severity changed the UI record: %+v", got)
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

func TestLogExplainsUserActionsAndDelivery(t *testing.T) {
	for name, tc := range map[string]struct {
		decision, result, want string
	}{
		"should explain selection":              {"inclusion", "included", "Marked fact#7 as included."},
		"should explain clearing selection":     {"inclusion", "", "Cleared the selection for fact#7."},
		"should explain permission grants":      {"RW", "true", "Enabled permission to post retry comments in fact#7."},
		"should explain permission removal":     {"RW", "false", "Disabled permission to post retry comments in fact#7."},
		"should explain ignoring retries":       {"ignore", "true", "Ignoring retry suggestions in fact#7."},
		"should explain considering retries":    {"ignore", "false", "Considering retry suggestions in fact#7."},
		"should explain review acknowledgement": {"conforma-acknowledged", "true", "Marked the Conforma advisory in fact#7 as reviewed."},
		"should explain undoing review":         {"conforma-acknowledged", "false", "Removed the Conforma review acknowledgement in fact#7."},
		"should explain confirmation":           {"interactive", "true", "Retry comments require confirmation."},
		"should explain unattended mode":        {"interactive", "false", "Retry comments no longer require confirmation. Permission is still required for each target."},
		"should explain retry rejection":        {"reject", "true", "Rejected the proposed retry in fact#7."},
		"should explain clearing rejection":     {"reject", "false", "Cleared the rejection of the proposed retry in fact#7."},
		"should explain approval":               {"approve", "single use", "Approved one retry comment in fact#7."},
		"should explain dispatch":               {"dispatch", "attempt", "Posting a retry comment in fact#7."},
		"should explain successful delivery":    {"delivery", "sent", "Posted a retry comment in fact#7."},
		"should explain failed delivery":        {"delivery", "failed", "Could not post a retry comment in fact#7."},
		"should explain uncertain delivery":     {"delivery", "uncertain", "Could not confirm whether a retry comment was posted in fact#7. Check GitHub before retrying."},
		"should explain stale approval":         {"withheld", "stale", "Did not post a retry comment in fact#7 because its approval is out of date."},
		"should explain recovery":               {"checks-recovered", "Reads recovered.", "Reading build results in fact#7 is working again."},
		"should retain safe error details":      {"checks-error", "GitHub returned HTTP 403: access denied.", "Could not read build results in fact#7. GitHub returned HTTP 403: access denied."},
	} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			log := NewLog(&output)
			record := Record{At: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC), Target: release.Target{Repo: "stackrox/fact", PR: 7}, Decision: tc.decision, Result: tc.result}
			log.Append(record)
			first, _, _ := strings.Cut(output.String(), "\n")
			if _, message, ok := strings.Cut(first, ": "); !ok || message != tc.want {
				t.Fatalf("output = %q, want message %q", first, tc.want)
			}
			if log.Recent()[0] != record {
				t.Fatal("CLI prose changed the UI audit record")
			}
		})
	}
}

func TestRetryDelay(t *testing.T) {
	for name, tc := range map[string]struct {
		delay time.Duration
		want  string
	}{
		"one second":         {time.Second, "1 second"},
		"several seconds":    {5 * time.Second, "5 seconds"},
		"one minute":         {time.Minute, "1 minute"},
		"fractional minutes": {90 * time.Second, "1.5 minutes"},
		"several minutes":    {5 * time.Minute, "5 minutes"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := retryDelay(tc.delay); got != tc.want {
				t.Fatalf("delay = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFailedLogWriteRetainsConsoleEntry(t *testing.T) {
	log := NewLog(badLog{})
	log.Append(Record{Decision: "github-error", Result: "GitHub rejected the request (HTTP 401)"})
	if len(log.Recent()) != 1 || log.Error() == "" {
		t.Fatal("log failure hid the diagnostic event")
	}
}
