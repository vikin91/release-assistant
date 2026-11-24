# Architecture and invariants

## Rules for external writes

1. **WRITE-01 — One write path.** Every external write must pass through
   `internal/dispatch`. The only external write the app can send is a Konflux
   retry comment to GitHub.
   **Check.** Trace every call to the writer, including calls from background polling.
2. **WRITE-02 — Read-only by default.** The app must start in Read-only mode
   unless the user supplies `--enable-writes`.
   **Check.** Attempt a send without the flag. The fake writer must receive zero calls.
3. **WRITE-03 — One approval per comment.** With writes enabled, the app must
   start in Interactive mode. Each approval applies once to one target, revision,
   run, and comment text. **Send all** approves each visible eligible comment once.
   **Check.** Test an unapproved send, a repeated send, and a send after the revision changes.
4. **WRITE-04 — Grant per target.** The app can post automatically only when
   the user disables Interactive mode. It must also have a current grant for
   the exact target. Release inclusion gives no grant.
   **Check.** Include a target without a grant. Check that background polling sends nothing.
5. **WRITE-05 — Check again before sending.** The dispatcher must recheck permission,
   target identity, and retry eligibility before each send. The label
   `no-release-assistant` blocks PR comments. If the user requires the PR label,
   a fresh GitHub read must show `allow-release-assistant`.
   **Check.** Test a new revision, a missing required label, a failed label read,
   and the `no-release-assistant` label. The fake writer must receive zero calls in each case.
6. **RETRY-01 — Konflux decides eligibility.** Use the public `konflux-retest` API
   to decide which Konflux jobs qualify. Post one comment per eligible job.
   The dashboard and CLI must never offer or send an Actions rerun for
   `Test Konflux builds`. **Check.** Fail `wait-for-images` without an eligible
   Konflux job. Expect zero retry commands for that workflow.
7. **QUEUE-01 — Show eligible retries.** The Retest Queue must show each eligible
   retry for a monitored target in every mode. If posting is blocked, show the reason.
   **Check.** Use the same eligible job in Read-only, Interactive, and automatic modes.
   Check that the row stays visible when posting is blocked.

## Release data and package boundaries

8. **APP-01 — One local release.** Each process monitors one ACS patch release.
   `cmd/release-assistant` starts the server on the loopback address.
   `internal/app` owns polling and session state. `internal/web` renders the UI.
   **Check.** Check the server address at startup. Close the browser. Check that polling continues.
9. **DATA-01 — Evidence stays distinct.** `internal/release` must assess evidence
   without network calls. Keep verified, stale, and unavailable evidence distinct.
   **Check.** Check that `internal/release` makes no network calls. Fail a read after
   a successful read. The old value must never count as current evidence.
10. **RC-01 — Find the current release candidate.** Use the highest-numbered
    StackRox release candidate (RC) tag on the configured release branch.
    Compare its commit with the branch head.
    **Check.** Supply multiple RC tags, including one outside the branch.
    Check the selected tag and the number of commits after it.
