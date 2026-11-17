package app

import (
	"fmt"
	"io"
	"slices"
	"strings"
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
	// Log writes readable decisions and exposes write failures to the UI.
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
	var text strings.Builder
	fmt.Fprintf(&text, "%s [%s]", r.At.Format(time.RFC3339Nano), r.Decision)
	target := r.Target.Repo
	if r.Target.PR > 0 || r.Target.Tag != "" {
		target = r.Target.String()
	}
	if target != "" {
		fmt.Fprintf(&text, " %s", target)
	}
	if r.Release != "" {
		fmt.Fprintf(&text, " release=%s", r.Release)
	}
	if r.SHA != "" {
		fmt.Fprintf(&text, " sha=%s", r.SHA)
	}
	if r.Origin != "" {
		fmt.Fprintf(&text, " origin=%s", r.Origin)
	}
	fmt.Fprintf(&text, ": %s\n", strings.ReplaceAll(r.Result, "\n", "\n    "))
	if r.Payload != "" {
		fmt.Fprintf(&text, "    Payload: %s\n", strings.ReplaceAll(r.Payload, "\n", "\n    "))
	}
	if _, err := io.WriteString(l.output, text.String()); err != nil {
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
