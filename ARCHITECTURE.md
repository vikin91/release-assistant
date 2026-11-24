# Architecture rules

## Retry decisions

1. **PLAN-01 — The retry tools decide.** Konflux Retest decides which Konflux
   jobs to retry. CI Retest decides which GitHub Actions and Prow jobs to retry.
   The tools supply the retry action and the text of any retry command.
   Release Assistant uses those decisions. It must not make its own retry rules.
   **Check.** Give the dashboard and the standalone tools the same target and
   settings. Compare their retry actions and commands.
2. **PLAN-02 — Retry every check that qualifies.** The purpose of retries is to
   get checks passing so PRs can merge and releases can proceed. Send failed
   and long-running checks to the responsible tool for a decision. Retry each
   check the tool says can run again as soon as posting rules permit.
   **Check.** Test a failed check and a stuck check. Check that each retry the
   tools approve reaches the queue.
3. **PLAN-03 — Do not add local barriers.** Do not block a retry because the
   release is unready, old run data is missing, or an unrelated read fails.
   If the retry tool cannot decide, show the reason. Ask the tool again
   when new data arrives.
   **Check.** Offer an eligible retry while the release is unready.
   Check that the app can send it when posting rules permit.
4. **PLAN-04 — One tool owns each check.** A failed Actions check in
   **Test Konflux builds** cannot authorize a Konflux retry. Only Konflux Retest
   can decide if the build qualifies. CI Retest must never offer an Actions
   rerun for that workflow.
   **Check.** Fail **wait-for-images** without an eligible Konflux job.
   Check that neither tool offers a retry for that failure.

## Queue and posting

5. **QUEUE-01 — Put every eligible retry in one queue.** Show all retries
   that either tool offers for monitored targets. Show them in every mode.
   Keep blocked retries visible with the reason. Show manual actions as manual.
   **Check.** Use offers from both tools in Read-only, Interactive, and automatic modes.
6. **QUEUE-02 — Let users act in parallel.** The user can select **Send** on
   any number of eligible rows without waiting for an earlier send to finish.
   The app must send the same request only once while it is in progress.
   **Check.** Pause one send. Send a different row and click it twice.
   Check that the second request starts and reaches GitHub only once.
7. **QUEUE-03 — Show the result of every send.** Record each GitHub posting
   call separately. Show the target, retry, and result. Distinguish success,
   failure, and unknown delivery.
   **Check.** Simulate success, failure, and unknown delivery. Return the
   results out of order. Check that the log matches each result to its send.
8. **WRITE-01 — Use one path to post.** All app posts to GitHub pass through
   `internal/dispatch`. The app posts only Konflux retry comments. CI Retest
   supplies manual actions. Keep the command text from the tool.
   **Check.** Trace sends from clicks and polling. Check that manual CI retries
   never reach the writer.
9. **WRITE-02 — Check permission at send time.** Start read-only unless the
   user enables writes. Start in Interactive mode when writes are enabled.
   Check current permission, the target revision, and the retry before each send.
   Apply the label rules in [AGENTS.md](AGENTS.md).
   **Check.** Remove permission or change the revision before posting.
   Check that the writer receives zero calls.
10. **WRITE-06 — Identify the app in every comment.** Add a footer to each
    comment the app posts on the user's behalf. Include the Release Assistant
    name and version or build identifier.
    **Check.** Inspect a PR comment and a tag comment. Check both footers.

## Shared UI and API

11. **UI-01 — Reuse the same UI for the same task.** Use the same components
    for PR lists across components and the ACS release branch. Supply the
    target data to the shared components.
    **Check.** Trace both lists to the same rendering code and action controls.
12. **API-01 — Reuse the same API for the same action.** Pass the repository,
    target, and revision as data. Use the same validation and action path
    for each view that performs that action.
    **Check.** Submit the same action from component and ACS views.
    Check that both requests use the same handler and validation.
13. **DATA-01 — Assess the data without reading it again.** Keep network
    reads outside release assessment. Give the assessment code the results
    of those reads. Keep a failed read distinct from a verified negative result.
    **Check.** Run release assessment with network access disabled.
    Compare an unavailable image check with a verified missing image.
