package app

import (
	"fmt"
	"io"
	"path"
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
		// message supplies CLI prose independently of the UI's audit fields.
		message string
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
	stamp := r.At.Format(time.RFC3339)
	fmt.Fprintf(&text, "%s %s: %s\n", stamp, recordLevel(r), strings.ReplaceAll(recordMessage(r), "\n", "\n    "))
	var details []string
	if r.Release != "" {
		details = append(details, "release "+r.Release)
	}
	if r.Target.Repo != "" {
		details = append(details, "repository "+r.Target.Repo)
	}
	if r.SHA != "" {
		details = append(details, "commit "+r.SHA)
	}
	if r.Origin != "" {
		details = append(details, "reported by "+r.Origin)
	}
	if len(details) > 0 {
		fmt.Fprintf(&text, "%s Debug: Context for %s: %s.\n", stamp, consoleTarget(r.Target), strings.Join(details, "; "))
	}
	if r.Payload != "" && r.Decision != "image" {
		fmt.Fprintf(&text, "%s Debug: Payload for %s:\n    %s\n", stamp, consoleTarget(r.Target), strings.ReplaceAll(r.Payload, "\n", "\n    "))
	}
	if _, err := io.WriteString(l.output, text.String()); err != nil {
		l.failure = "Action log write failed"
	} else {
		l.failure = ""
	}
}

// counted keeps counts grammatical in CLI sentences.
func counted(count int, noun string) string {
	if count != 1 {
		noun += "s"
	}
	return fmt.Sprintf("%d %s", count, noun)
}

// retryDelay expresses polling backoff in seconds or minutes for the CLI.
func retryDelay(delay time.Duration) string {
	count, unit := delay.Seconds(), "second"
	if delay >= time.Minute {
		count, unit = delay.Minutes(), "minute"
	}
	if count != 1 {
		unit += "s"
	}
	return fmt.Sprintf("%g %s", count, unit)
}

// consoleTarget keeps repository and ref names readable without omitting the ref.
func consoleTarget(target release.Target) string {
	if target.Repo == "" {
		return "the release"
	}
	repo := path.Base(target.Repo)
	if target.PR > 0 {
		return fmt.Sprintf("%s#%d", repo, target.PR)
	}
	if target.Tag != "" {
		return repo + "/" + target.Tag
	}
	return repo
}

// recordMessage translates audit decisions into sentences for the CLI.
func recordMessage(r Record) string {
	if r.message != "" {
		return r.message
	}
	target := consoleTarget(r.Target)
	for operation, subject := range map[string]string{
		"tags": "release tags", "github": "pull requests and release tags",
		"checks": "build results", "images": "image availability", "pinning-images": "build images",
	} {
		if r.Decision == operation+"-error" {
			return fmt.Sprintf("Could not read %s in %s. %s", subject, target, r.Result)
		}
		if r.Decision == operation+"-recovered" {
			return fmt.Sprintf("Reading %s in %s is working again.", subject, target)
		}
	}
	switch r.Decision {
	case "configure":
		return fmt.Sprintf("Preparing release %s.", r.Release)
	case "inclusion":
		if r.Result == "" {
			return fmt.Sprintf("Cleared the selection for %s.", target)
		}
		return fmt.Sprintf("Marked %s as %s.", target, r.Result)
	case "tag-confirmed":
		return fmt.Sprintf("Selected release tag %s.", target)
	case "tag-invalidated":
		return fmt.Sprintf("The commit for %s changed. Select the tag again before continuing.", target)
	case "selection-complete":
		return "Pull request selection is complete. Monitoring selected builds and release pins."
	case "conforma-acknowledged":
		if r.Result == "true" {
			return fmt.Sprintf("Marked the Conforma advisory in %s as reviewed.", target)
		}
		return fmt.Sprintf("Removed the Conforma review acknowledgement in %s.", target)
	case "RW":
		if r.Result == "true" {
			return fmt.Sprintf("Enabled permission to post retry comments in %s.", target)
		}
		return fmt.Sprintf("Disabled permission to post retry comments in %s.", target)
	case "ignore":
		if r.Result == "true" {
			return fmt.Sprintf("Ignoring retry suggestions in %s.", target)
		}
		return fmt.Sprintf("Considering retry suggestions in %s.", target)
	case "interactive":
		if r.Result == "true" {
			return "Retry comments require confirmation."
		}
		return "Retry comments no longer require confirmation. Permission is still required for each target."
	case "revoked":
		return fmt.Sprintf("Revoked permission to post retry comments in %s because the target changed or no longer allows writes.", target)
	case "reject":
		if r.Result == "true" {
			return fmt.Sprintf("Rejected the proposed retry in %s.", target)
		}
		return fmt.Sprintf("Cleared the rejection of the proposed retry in %s.", target)
	case "approve":
		return fmt.Sprintf("Approved one retry comment in %s.", target)
	case "withheld":
		return fmt.Sprintf("Did not post a retry comment in %s because its approval is out of date.", target)
	case "dispatch":
		return fmt.Sprintf("Posting a retry comment in %s.", target)
	case "delivery":
		switch r.Result {
		case "sent":
			return fmt.Sprintf("Posted a retry comment in %s.", target)
		case "failed":
			return fmt.Sprintf("Could not post a retry comment in %s.", target)
		default:
			return fmt.Sprintf("Could not confirm whether a retry comment was posted in %s. Check GitHub before retrying.", target)
		}
	default:
		return r.Result
	}
}

// recordLevel assigns console severity without changing the UI's audit records.
func recordLevel(r Record) string {
	switch {
	case strings.HasSuffix(r.Decision, "-error"), r.Decision == "delivery" && r.Result == "failed":
		return "Error"
	case r.Decision == "retry-wait", r.Decision == "tag-invalidated", r.Decision == "revoked", r.Decision == "withheld", r.Decision == "delivery" && r.Result == "uncertain":
		return "Warn"
	case r.Decision == "work-complete", r.Decision == "observation", r.Decision == "checks", r.Decision == "image":
		return "Debug"
	default:
		return "Info"
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
