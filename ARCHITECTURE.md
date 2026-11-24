# Architecture rules

## Retry decisions

1. **The retry tools decide.** Konflux Retest decides which Konflux
   jobs to retry. CI Retest decides which GitHub Actions and Prow jobs to retry.
   The tools supply the retry action and the text of any retry command.
   Release Assistant uses those decisions. It must not make its own retry rules.
2. **Retry every check that qualifies.** The purpose of retries is to
   get checks passing so PRs can merge and releases can proceed. Send failed
   and long-running checks to the responsible tool for a decision. Retry each
   check the tool says can run again as soon as posting rules permit.
3. **Do not add local barriers.** Do not block a retry because the
   release is unready, old run data is missing, or an unrelated read fails.
   If the retry tool cannot decide, show the reason. Ask the tool again
   when new data arrives.
4. **One tool owns each check.** A failed Actions check in
   **Test Konflux builds** cannot authorize a Konflux retry. Only Konflux Retest
   can decide if the build qualifies. CI Retest must never offer an Actions
   rerun for that workflow. CI-retest and Konflux-Retest target two different 
   systems and one does not emit retest comments for the other and vice versa.

## Queue and posting

5. **Put every eligible retry in one queue.** Show all retries
   that either tool offers for monitored targets. Show them in every mode.
   Keep blocked retries visible with the reason. Show manual actions as manual.
   The user looking at the Queue view must see all things that are actionable
   in terms of retries.
6. **Let users act in parallel.** The user can select **Send** on
   any number of eligible rows in the queue without waiting for an earlier 
   send to finish. The app must do it's best to send the same request only once 
   so that a retest is not triggered while the check run is in progress.
7. **Show the result of every send.** Record each GitHub posting
   call separately. Show the target, retry, and result. Distinguish success,
   failure, and unknown delivery. Base that on API reply.
8. **Use one path to post.** All app posts to GitHub pass through
   `internal/dispatch`. There must be a central place that monitors and 
   controlls posting of comments.
9. **Check permission at send time.** Start read-only unless the
   user enables writes. Start in Interactive mode when writes are enabled.
   Check current permission, the target revision, and the retry before each send.
   Apply the label rules in [AGENTS.md](AGENTS.md).
10. **Identify the app in every comment.** Add a footer to each
    comment the app posts on the user's behalf. Include the Release Assistant
    name and version or build identifier.

## Shared UI and API

11. **Reuse the same UI component for the same task.** Use the same components
    for PR lists across components and the ACS release branch. Supply the
    target data to the shared components.
12. **Reuse the same API for the same action.** Pass the repository,
    target, and revision as data. Use the same validation and action path
    for each view that performs that action.
13. **Assess the data without reading it again.** Keep network
    reads outside release assessment. Give the assessment code the results
    of those reads. Keep a failed read distinct from a verified negative result.
14. CI-retest and Konflux-retest call GitHub APIs on their own. Try to leverage their
    results without issuing duplicate API requests.
