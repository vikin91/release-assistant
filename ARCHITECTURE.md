# Architecture and invariants

## What the app does

1. **RELEASE-01 — The user starts the release.** The app monitors one ACS patch
   release. It reports readiness. The user starts the release.
   **Check.** Make the release ready. Check that the app reports readiness
   and starts no release.
2. **CHOICE-01 — Keep the user's choices.** A refresh must keep a valid tag
   that the user selects. A refresh must also keep the user's choice to clear a tag.
   **Check.** Select a tag and refresh. Then clear the tag and refresh.
   Check the choice after each refresh.
3. **EVIDENCE-01 — Check what exists.** A successful build cannot prove that
   an image is available. A proposed version change cannot prove that the
   release branch uses that version.
   **Check.** Use a successful build with a missing image. Use an open pull
   request with the correct versions and a branch with the wrong versions.
   Check that both requirements remain unmet.
4. **EVIDENCE-02 — Use current evidence.** Stale or unavailable evidence cannot
   establish a current result.
   **Check.** After a successful read, make its source unavailable. Check that
   the old result no longer counts as current.
5. **RC-01 — Find the current release candidate.** Use the highest-numbered
   StackRox release candidate tag on the configured release branch. Compare
   its commit with the branch head.
   **Check.** Supply tags on and off the release branch. Check the selected tag
   and the number of commits after it.

## What the app can send

6. **POST-01 — The user controls posting.** The app must have the user's
   permission before it posts a retry comment.
   **Check.** Include a change with an eligible retry. Check that the app posts
   nothing without permission.
7. **WRITE-02 — Read-only by default.** The app must start in Read-only mode
   unless the user starts it with permission to post.
   **Check.** Start the app without giving it permission to post. Check that
   the app cannot send an eligible retry comment.
8. **WRITE-03 — One approval per comment.** With permission to post, the app
   starts in Interactive mode. Each approval applies once to one target,
   revision, run, and comment text. **Send all** approves each visible eligible comment once.
   **Check.** Try to send without approval. Try to use the same approval twice.
   Try to use an approval after the revision changes. Check that each invalid send fails.
9. **WRITE-04 — Grant per target.** Automatic posting requires the user to
   disable Interactive mode and grant permission for the exact target.
   Release inclusion gives no grant.
   **Check.** Enable automatic posting for one target. Check that a second
   target with an eligible retry receives no comment without its own grant.
10. **WRITE-05 — Check again before sending.** Before each send, the app checks
    permission, target identity, and retry eligibility again. The label
    `no-release-assistant` blocks pull request comments. If the user requires
    the `allow-release-assistant` label, a fresh GitHub read must show it.
    **Check.** Test a new revision, an opt-out label, a missing required label,
    and a failed required label read. Check that the app sends nothing in each case.
11. **QUEUE-01 — Show eligible retries.** The Retest Queue shows each eligible
    retry for a monitored target in every mode. If the app cannot post a
    comment, the queue shows the reason.
    **Check.** Use the same eligible retry in all modes. Block posting.
    Check that the retry and the reason remain visible.

## Code boundaries

12. **WRITE-01 — One write path.** Send only Konflux retry comments to GitHub.
    Route every external write through `internal/dispatch`. Require
    `--enable-writes` at startup.
    **Check.** Trace all calls to the writer, including calls from background polling.
13. **RETRY-01 — Konflux decides eligibility.** Use the public `konflux-retest`
    API. Send one comment per eligible job. The dashboard and CLI must never
    offer or send an Actions rerun for `Test Konflux builds`.
    **Check.** Fail `wait-for-images` with no eligible Konflux job. Check that
    neither interface offers a retry command for that workflow.
14. **APP-01 — Local server.** `cmd/release-assistant` starts the server on the
    loopback address. `internal/app` owns polling and session state.
    `internal/web` renders the UI.
    **Check.** Check the server address. Close the browser. Check that polling continues.
15. **DATA-01 — No network calls in release assessment.** `internal/release`
    assesses evidence without network calls.
    **Check.** Check that release assessment uses inputs from its caller.
