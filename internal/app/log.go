package app

import (
	"encoding/json"
	"io"
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
		failure string
		recent  []Record
	}
)

// NewLog writes audit records to w and retains recent activity for the UI.
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

// Recent returns copied session activity, including records whose output write failed.
func (l *Log) Recent() []Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.recent)
}

// Error reports audit availability without exposing filesystem or credential details.
func (l *Log) Error() string { l.mu.Lock(); defer l.mu.Unlock(); return l.failure }
