## Architecture checks

Read [ARCHITECTURE.md](ARCHITECTURE.md) before you edit. Check every change
against each rule before you finish. In your summary, give the number of
each affected rule and the result of its check. State when a check could
not run. If a request conflicts with a rule, give its number and ask the
user to approve the rule change before you implement it.

## External write authorization

Every write to GitHub or Konflux must pass through the authorization boundary
in `internal/dispatch` before an adapter sends it. Enforce this in the Go
code for every caller, including background polling. The presence of UI
controls or write-capable credentials confers no permission.

1. Start read-only unless the user passes `--enable-writes`. Enforce the
   startup flag in `internal/dispatch` for every external write. Keep UI
   actions that send writes or enable automatic posting unavailable in
   Read-only mode.
2. Start in Interactive mode when writes are enabled. Require a **Send**
   click in the Retest Queue for each eligible comment. The click is the
   single-use authorization bound to the exact target, current revision,
   run, and payload. Target grants apply only to automatic posting.
3. Automatic posting requires `--enable-writes`, Interactive mode disabled,
   a current automatic posting grant for the exact target, and verified
   eligibility. Release inclusion confers no automatic posting permission.
   Revalidate authorization and evidence immediately before dispatch in
   every mode.
4. Start with **Require the label for PR comments** unchecked. When enabled,
   require the `allow-release-assistant` label for every PR retry comment,
   including comments the user confirms. Check the label in `internal/dispatch`
   using a fresh GitHub read immediately before dispatch. Deny the write if
   the label is missing or GitHub cannot verify it. The label requirement
   applies only to PRs. A label does not grant write permission or override
   `no-release-assistant`.
5. Show **Read-only** when writes are disabled and **Interactive** when
   confirmation is required. Show **Only labeled** when automatic posting
   requires the PR label and **Full auto** when it does not. Show
   **Interactive · Only labeled** when both requirements apply. The pill opens
   a popup that explains each mode and how to enable it. Show **Allowed by
   label** beside qualifying PRs only while the label requirement is enabled.
6. Keep GitHub and Konflux reads separate from write-capable adapters. The only
   supported external writes are Konflux retry comments posted to GitHub,
   with one exception. An explicit **Create pinning PR** click with
   `--enable-writes` authorizes `internal/dispatch` to use the local `gh`
   CLI to create the StackRox branch, commit the reviewed version-file
   changes, and open the PR. The authorization does not include merging.
   Keep job eligibility in `konflux-retest`. Test authorization with fake
   writers and assert that denied requests never reach the writer.
7. Route every Konflux build retry through the public `konflux-retest` interface.
   Include one command per eligible job. A PR comment may contain multiple
   `/konflux-retest <job>` commands, one per line, when the pipeline triggers
   match exactly the selected jobs. Do not repost the same retry requests in
   consecutive comments for the same target, revision, and runs. On a tagged
   commit, post one `/retest <job> tag:<tag>` command per comment. A failed Actions
   job in `Test Konflux builds`, including `wait-for-images`, does not establish
   build eligibility. Never offer or send an Actions rerun for that workflow,
   even when the Konflux planner offers no comment. This applies to the
   dashboard and the CLI.

## Retest Queue

1. Add every eligible retry for a monitored target to the Retest Queue in
   every mode, including Read-only. Determine queue membership from current
   retry evidence. Keep eligible entries visible when posting is blocked and
   show the reason, such as missing permission, a missing required label, or
   a cooldown.
2. In Interactive mode, the user's **Send** click authorizes that comment.
   In automatic modes, send eligible writes when `internal/dispatch` authorizes
   them. Apply the external write authorization rules above in every mode.
3. Test that eligible retries appear across modes and remain visible when
   posting is blocked. Verify that interactive writes wait for confirmation.

## Repository orientation

This local Go app monitors one ACS patch release. `cmd/release-assistant`
wires the adapters and loopback server. `internal/app` owns polling and
session state, `internal/release` evaluates evidence without network calls,
and `internal/web` owns the Go templates and embedded JavaScript.

Keep verified, stale, and unavailable evidence distinct. Discover the current
RC from StackRox tags and compare its commit with the configured release branch
head. Use the README and current code for the workflow. The RC.0 sections in
`design.md` predate current RC discovery.

## Settings page

1. Use **Settings** as the page heading. Present settings in two-column tables.
   Give each save action its own form and place it directly below its table.
2. Use a full-width header row for each category, such as **Refresh intervals**.
   Put a short explanation below the heading. Use smaller full-width headers for
   groups such as **Components** and **ACS**. State which components or release
   phases the group covers. Keep phase-specific rows together.
3. Give each setting one row. Put its label and any help text in the left cell.
   Align the control in the right column and put its unit beside it. Associate
   the label, help text, and unit with the input.
4. Use the dashboard fonts, muted help text, and horizontal table rules.
   Keep controls aligned and let long descriptions wrap on narrow screens.

## Development

Use the Go version in `go.mod`. Go downloads the pinned
`github.com/vikin91/konflux-retest` module.

Run `go test ./...` and `go vet ./...`. Run `go test -race ./...` for polling
or session changes and `node --test internal/web/app_js_test.js` for browser
JavaScript changes. Use the README's fixture server for manual UI checks.

### Run Go lint

The `golangci-lint` on `PATH` is a StackRox wrapper that looks for
`.gotools/bin/golangci-lint`. From the Release Assistant repository root, use
the standalone v2 binary by absolute path. Put the caches in writable paths.

```sh
GOCACHE=/tmp/release-assistant-go-cache \
GOLANGCI_LINT_CACHE=/tmp/release-assistant-lint-cache \
  /tmp/acs-tools/golangci-lint run ./...
```

Check the build version with `/tmp/acs-tools/golangci-lint version`. If the
binary is missing or its Go version is older than the version in `go.mod`,
build it with a compatible Go toolchain. Then run the lint command above.

```sh
mkdir -p /tmp/acs-tools
GOCACHE=/tmp/release-assistant-go-cache GOBIN=/tmp/acs-tools \
  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0
```

## Local commits

Implementation requests in this repository authorize a local commit by default.
After validation, stage only the files changed for the task and commit them
before replying, unless I explicitly ask to leave the changes uncommitted.
Leave pushes to me.
