package app

import (
	"io"
	"strconv"
	"testing"
)

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
