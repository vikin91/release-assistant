package web

import (
	"fmt"
	"strings"

	"release-assistant/internal/release"
)

func manualPinningCommand(req release.PinningRequest) string {
	if req.Head == "" || len(req.Changes) == 0 {
		return ""
	}
	var command strings.Builder
	fmt.Fprintf(&command, "(\n  set -e\n  branch=%s\n\n", pinningShellQuote(req.HeadBranch()))
	fmt.Fprintf(&command, "  git fetch origin %s\n", pinningShellQuote(req.Branch))
	fmt.Fprintf(&command, "  test \"$(git rev-parse FETCH_HEAD)\" = %s || {\n", pinningShellQuote(req.Head))
	command.WriteString("    echo 'The release branch changed. Refresh Release Assistant.' >&2\n    exit 1\n  }\n")
	command.WriteString("  git switch -c \"$branch\" FETCH_HEAD\n")
	var paths, updates []string
	for _, change := range req.Changes {
		value, format := change.After, "%s"
		if trimmed, ok := strings.CutSuffix(value, "\r\n"); ok {
			value, format = trimmed, "%s\\r\\n"
		} else if trimmed, ok := strings.CutSuffix(value, "\n"); ok {
			value, format = trimmed, "%s\\n"
		}
		path := pinningShellQuote(change.Path)
		fmt.Fprintf(&command, "  printf %s %s > %s\n", pinningShellQuote(format), pinningShellQuote(value), path)
		paths = append(paths, path)
		updates = append(updates, fmt.Sprintf("%s from %s to %s", change.Path, strings.TrimSpace(change.Before), strings.TrimSpace(change.After)))
	}
	fmt.Fprintf(&command, "  git commit -m %s -- %s\n", pinningShellQuote(req.Title()), strings.Join(paths, " "))
	command.WriteString("  git push origin \"HEAD:refs/heads/$branch\"\n")
	fmt.Fprintf(&command, "  gh pr create --repo %s --base %s --head \"$branch\" \\\n", pinningShellQuote(req.Repo), pinningShellQuote(req.Branch))
	fmt.Fprintf(&command, "    --title %s \\\n    --body %s\n)", pinningShellQuote(req.Title()), pinningShellQuote("Update "+strings.Join(updates, ", ")+"."))
	return command.String()
}

func pinningShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
