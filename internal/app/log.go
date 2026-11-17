package app

import (
	"encoding/json"
	"io"
	"os"
	"slices"
	"sync"
	"time"

	"release-assistant/internal/release"
)

const maxRecentRecords = 200

type (
	// Record contains application-owned audit fields only, never raw HTTP errors.
	Record struct {
		At       time.Time      `json:"at"`
		Release  string         `json:"release"`
		Target   release.Target `json:"target"`
		SHA      string         `json:"sha,omitempty"`
		Decision string         `json:"decision"`
		Payload  string         `json:"payload,omitempty"`
		Result   string         `json:"result"`
		Origin   string         `json:"origin"`
	}
	// Log appends JSON decisions and exposes write failures to the UI.
	Log struct {
		mu      sync.Mutex
		output  io.Writer
		closer  io.Closer
		failure string
		recent  []Record
	}
)

// OpenLog opens an append-only private log without replaying it.
func OpenLog(path string) (*Log, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	return &Log{output: f, closer: f}, nil
}

// NewLog accepts an audit sink for tests or embedding.
func NewLog(w io.Writer) *Log { return &Log{output: w} }

// Append records a decision; failures remain visible until a successful write.
func (l *Log) Append(r Record) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.recent) == maxRecentRecords {
		l.recent = slices.Delete(l.recent, 0, 1)
	}
	l.recent = append(l.recent, r)
	if err := json.NewEncoder(l.output).Encode(r); err != nil {
		l.failure = "Action log write failed"
	} else {
		l.failure = ""
	}
}

// Recent returns copied session activity, including records whose file write failed.
func (l *Log) Recent() []Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.recent)
}

// Error reports audit availability without exposing filesystem or credential details.
func (l *Log) Error() string { l.mu.Lock(); defer l.mu.Unlock(); return l.failure }

// Close releases the file after monitoring has stopped.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closer != nil {
		return l.closer.Close()
	}
	return nil
}
