## External write authorization

Every write to GitHub or Konflux must pass through the authorization boundary
in `internal/dispatch` before an adapter sends it. Enforce this in the Go
code for every caller, including background polling. UI controls and the
presence of write-capable credentials confer no permission.

1. Start read-only. Deny every external write unless the user explicitly
   enables writes with `--enable-writes` and grants access to the exact target.
   Release inclusion confers no write permission.
2. Start in interactive mode. While it is enabled, require a separate,
   single-use user confirmation for each write. Bind it to the exact target,
   current revision, run, and payload. A grant alone cannot send a write.
3. With interactive mode disabled, automatic writes still require global
   write enablement, a current per-target grant, and verified eligibility.
   Revalidate before sending and deny the write if authorization or evidence
   is missing, stale, or revoked.
4. Keep GitHub and Konflux reads separate from write-capable adapters. The only
   supported external writes are Konflux retry comments posted to GitHub.
   Keep job eligibility in `konflux-retest`. Test authorization with fake
   writers and assert that denied requests never reach the writer.
5. Route every Konflux build retry through the public `konflux-retest` interface.
   Post one comment per eligible job. Use `/konflux-retest <job>` on a PR
   or `/retest <job> tag:<tag>` on the tagged commit. A failed Actions
   job in `Test Konflux builds`, including `wait-for-images`, does not establish
   build eligibility. Never offer or send an Actions rerun for that workflow,
   even when the Konflux planner offers no comment. This applies to the
   dashboard and the CLI.

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

## Local commits

Implementation requests in this repository authorize a local commit by default.
After validation, stage only the files changed for the task and commit them
before replying, unless I explicitly ask to leave the changes uncommitted.
Leave pushes to me.
