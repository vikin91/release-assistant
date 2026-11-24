package web

import (
	"regexp"
	"slices"
	"strings"
)

type retryDiagnostic struct {
	Label, Message string
	External       bool
}

var externalRetryFailure = regexp.MustCompile(
	`^(?:GitHub (?:GET|POST) .* rejected \(HTTP [45][0-9]{2}\): |` +
		`(?:required checks for [^:]+: )?(?:Authentication failed; GitHub rejected the token|` +
		`Access denied by token permissions or repository policy|Not found or inaccessible with the current GitHub token|` +
		`GitHub server error|GitHub rejected the request) \(HTTP [45][0-9]{2}\))`,
)

func retryDiagnostics(pending, incomplete bool, observationError string, warnings []string) []retryDiagnostic {
	var diagnostics []retryDiagnostic
	for i, message := range slices.Concat([]string{observationError}, warnings) {
		for line := range strings.SplitSeq(message, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			item := retryDiagnostic{Label: "Warning", Message: line}
			switch {
			case strings.Contains(line, "GitHub reported a rate limit (HTTP "),
				strings.Contains(line, "GitHub rate limit reached"),
				strings.HasPrefix(line, "GitHub GET ") && strings.Contains(line, " rejected (HTTP 429)"),
				strings.HasPrefix(line, "GitHub reads deferred until ") && strings.Contains(line, " after rate limit:"):
				item = retryDiagnostic{Label: "Error", Message: "Rate limited by GitHub", External: true}
			case strings.Contains(line, "Release Assistant deferred this request before contacting GitHub"),
				strings.Contains(line, "internal discovery request budget limit reached"):
				item = retryDiagnostic{Label: "Error", Message: "Rate limited internally by Release Assistant"}
			case externalRetryFailure.MatchString(line):
				_, reason, rejection := strings.Cut(line, "): ")
				if rejection {
					line = "GitHub: " + reason
				}
				item = retryDiagnostic{Label: "Error", Message: line, External: true}
			case i == 0 || incomplete:
				item.Label = "Error"
			}
			if !slices.Contains(diagnostics, item) {
				diagnostics = append(diagnostics, item)
			}
		}
	}
	if len(diagnostics) == 0 {
		switch {
		case pending:
			return []retryDiagnostic{{Label: "Pending", Message: "Loading GitHub Actions and Prow results"}}
		case incomplete:
			return []retryDiagnostic{{Label: "Pending", Message: "Retry results are incomplete"}}
		}
	}
	return diagnostics
}
