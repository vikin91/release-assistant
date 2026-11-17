package app

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strconv"
	"testing"
	"time"

	"release-assistant/internal/release"
)

func TestLogPrintsFullHistoryToConsole(t *testing.T) {
	var console bytes.Buffer
	log := NewLog(&console)
	var want []Record
	for index := range maxRecentRecords + 3 {
		record := Record{
			At:      time.Date(2026, time.November, 17, 12, 0, index, 0, time.UTC),
			Release: "5.0.1", Target: release.Target{Repo: "stackrox/stackrox"},
			SHA: "abc123", Decision: "proposal", Payload: "/konflux-retest job\nfull payload",
			Result: strconv.Itoa(index), Origin: "system",
		}
		log.Append(record)
		want = append(want, record)
	}
	decoder := json.NewDecoder(bytes.NewReader(console.Bytes()))
	for index, record := range want {
		var got Record
		if err := decoder.Decode(&got); err != nil {
			t.Fatalf("console record %d: %v", index, err)
		}
		if !reflect.DeepEqual(got, record) {
			t.Fatalf("console record %d = %+v, want %+v", index, got, record)
		}
	}
	var extra Record
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("unexpected trailing console output: %v", err)
	}
	if log.Error() != "" || len(log.Recent()) != maxRecentRecords {
		t.Fatal("console output changed recent history or log status")
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
