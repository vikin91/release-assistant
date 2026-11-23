# Original implementation plan

This document records the planned milestones for the first implementation.
The [current design](design.md) describes the application today. For setup,
examples, and validation commands, see the [README](README.md).

## 1. Scope and working rules

1. Work in `/Users/prygiels/code/release-assistant`. Deliver one Go executable, a local browser UI rendered with Go templates, in-memory session state, and an append-only action log.
2. Import `github.com/vikin91/konflux-retest` as a Go module dependency. The local checkout is `/Users/prygiels/code/konflux-retest`. Do not copy its planner, import its `internal` packages, or execute its CLI to obtain retry decisions. Keep its existing CLI usable.
3. Keep modules replaceable through small interfaces at external-service and application boundaries. Replacement means changing constructor wiring and rebuilding. Runtime plugins and dynamically loaded modules are out of scope.
4. Use the standard library for HTTP serving, HTML templates, flags, logging, and synchronization where it suffices. Do not introduce a frontend framework, dependency-injection framework, database, event bus, generic workflow engine, or persistent outgoing-write queue.
5. The only external writes are authorized Konflux retest comments. Never merge, enable automerge, create tags or PRs, push branches, add labels, or trigger ACS RC.0. Release commands are text for the user to copy.
6. Do not create worktrees. Do not initialize Git, create commits, or publish changes merely because this plan exists. The directory was not a Git repository when the plan was written. If a later implementation task authorizes branch work, inspect the repository first, use the user's git-spice workflow when applicable, and create `piotr/` branches from `master` unless instructed otherwise. Leave pushes and submissions to the user.
7. Preserve unrelated work in this directory and the dependency checkout. Do not modify the dependency as an incidental workaround. If the assigned implementation scope does not authorize a required dependency change, report the concrete gap and obtain authorization for that change while continuing independent application work.
8. Do not treat a milestone's partial implementation as the finished application. Record remaining work and failed checks explicitly. Do not invent image names, API capabilities, or successful validation to complete a checklist.

## 2. Package boundaries

Start with the following layout. Create files when their milestone needs them, rather than generating empty scaffolding for every package.

| Package or directory | Owns | Must not own |
| --- | --- | --- |
| `cmd/release-assistant` | Flag parsing, credential setup, constructors, server lifecycle | Release rules, retry planning, HTML |
| `internal/release` | Application-owned data types, pure readiness assessment, tag suitability, explanatory recommendations | HTTP clients, credentials, session mutation, library-specific types |
| `internal/app` | Session choices, polling, merging observations, invoking assessment and dispatch, action logging | Konflux dependency rules, registry protocol details, HTML |
| `internal/github` | GitHub discovery and freshness reads; separate comment-writing client | Inclusion decisions, permissions, automatic retry loops |
| `internal/konflux` | Adapter around the imported retry library; conversion to application types | Its own job catalog, dependency graph, or retry eligibility rules |
| `internal/images` | Component and ACS RC.0 image resolution and registry existence observations | Konflux retry planning, exhaustive architecture or attestation audits |
| `internal/dispatch` | Write grants, approvals, rejection state, rate limits, failure counters, final dispatch checks | Release readiness, polling schedules, discovery of eligible retry jobs |
| `internal/web` | Handlers, embedded templates/assets, display models, local request protection | External API calls or independent decisions about readiness and writes |

Keep action-log formatting in a focused file in `internal/app`; a separate logging framework is unnecessary.

Dependency direction is explicit:

1. `internal/release` depends on ordinary Go value types and pure helpers. External adapters translate their responses into these types.
2. `internal/app` consumes discovery, build, image, and dispatch interfaces and calls release assessment. Pass the pure assessment function through the constructor if replacement is needed; do not wrap it in an artificial service hierarchy.
3. `internal/dispatch` consumes a fresh-target reader, a retry planner, and a comment writer. It does not import the concrete GitHub or Konflux adapter.
4. `internal/web` consumes an application interface for snapshots and user actions. It does not import the external adapters or the retry library.
5. `cmd/release-assistant` imports concrete implementations and wires them together. Only `internal/konflux` imports `github.com/vikin91/konflux-retest` in application code.

Define each interface in its consuming package. Prefer one to three operations per interface and separate GitHub reads from comment writes. Pass dependencies explicitly through constructors. Do not create an interface for every struct or expose vendor types in application contracts.

### Boundary contracts

These are responsibilities, not prewritten Go signatures. Implement typed inputs and outputs after checking the dependency API in milestone 1.

| Boundary | Input | Output and required semantics |
| --- | --- | --- |
| Release discovery | Release configuration and current tracking choices | Branch heads, PR summaries, tags, ancestry facts, actual branch pins, supplied pinning-PR pins; retain partial results and identify incomplete observations |
| Fresh target read | Repository and PR number or tag name | Current SHA, PR open/closed state and opt-out label where applicable; errors mean authorization cannot be verified |
| Retry planning | PR or tag identity | Resolved SHA, eligible jobs, display status, advisories, uncertainty, and evidence needed for run-specific controls; errors may accompany useful results |
| Image inspection | ACS release version and confirmed component versions | Locally computed references from saved verified patterns, checked through registries only; missing tag selections and failed requests remain unknown |
| Release assessment | Observations and user choices | Overall readiness, individual condition statuses, reasons, and proposed next steps; no network calls or state mutation |
| Comment writing | Exact destination and one approved comment payload | Sent, failed, or uncertain delivery, with a comment identifier/link when available; no automatic HTTP write retry |
| Web application access | Snapshot request or typed user action | A copied display snapshot or an action result; handlers never mutate shared maps directly |

The real adapters and small test fakes must satisfy the same consumer interfaces. Demonstrate replacement in tests; do not implement a second production integration merely to prove modularity.

## 3. Data and state rules

Use explicit structs and enums. Zero values must mean unknown or unconfirmed, never ready or write-authorized.

1. **Release configuration:** ACS patch version, explicit ACS target branch, component repositories, their release branches and major/minor streams, optional pinning PR, port, polling intervals, retry limits, and log location. Require the component branch and stream information needed for discovery; do not infer ACS-to-component mappings. Scanner applies only to ACS 4.x.
2. **Target identity:** repository plus PR number, or repository plus tag name. Bind every observation, grant, and approval to a resolved SHA. Two tags pointing to one commit remain different permission targets.
3. **PR choices:** undecided, included, excluded, or ignored. Keep RW permission separate. Ignore suppresses detailed reads and all writes; repository-wide discovery may still encounter that PR.
4. **Session choices:** confirmed PR selection, confirmed component tags, pinning PR number, Conforma acknowledgements, and interactive mode. Keep a confirmed tag separate from a proposed newer tag. Browser reload preserves session state; server restart clears it and resets interactive mode on and grants off.
5. **Observations:** store source identity, observed time, value, and current availability/error separately. A failed refresh may preserve the last value for display but cannot make it fresh evidence for a write or a satisfied readiness condition.
6. **Readiness:** represent each condition as satisfied, unsatisfied, or unknown, with a reason and evidence references. The overall recommendation is derived from these conditions; do not maintain an independent mutable `ready` flag.
7. **Retry proposals:** identify target, SHA, job, relevant run identity, exact payload, and observation time. Approval is single-use and tied to this proposal. Rejecting one proposal suppresses that run, not every future run of the job.
8. **Dispatch state:** keep grants, approvals, per-target last-attempt times, per-job failure counts, and rejection state in memory. Failure counts use distinct runs for the same job/target/SHA. Repeated polls and repeated comments do not increase them. Success or a different SHA resets the count.

Keep session ownership explicit: `app` owns release choices and observations; `dispatch` owns write authority and retry-control state. Pass changes to the dispatcher synchronously when a target is ignored, its head changes, or its opt-out label is detected. Do not maintain independent copies of RW permission in the UI and the dispatcher.

## 4. Milestone 1: establish the dependency contract

Deliver a buildable module and a tested Konflux adapter before implementing a retry scheduler.

The inspected dependency declares Go `1.26.0` in `go.mod`. Its current public API is in [check.go](/Users/prygiels/code/konflux-retest/check.go). It exposes `CheckPR`, `CheckTag`, and a `Result` with `URL`, `Tag`, `SHA`, `Action`, `Message`, `Jobs`, and `Warnings`. It accepts credentials explicitly. It can return an error with useful partial results. Re-read the API and `go.mod` at implementation time because the local checkout may change.

Follow these steps:

1. Initialize the application as a local Go module named `release-assistant`; this does not create a Git repository or claim a remote repository name. Target Go 1.26.0 unless the dependency's actual minimum has changed. Use no features newer than the application's declared version.
2. Add an explicit module requirement for `github.com/vikin91/konflux-retest` and a local replacement pointing to the existing checkout. For unpublished local development, `v0.0.0` can be the deliberate requirement placeholder; it is not a published release claim. Document the local checkout requirement. Do not fetch an arbitrary latest version or copy source files.
3. Implement adapter methods that call `CheckPR` and `CheckTag` directly and convert results into application-owned types. Preserve partial errors and warnings. Do not discard all jobs solely because an unrelated input check failed, and do not interpret `ActionNone` as release readiness.
4. Audit the public result against run-specific requirements. At plan-writing time it does not expose check-run IDs, per-job outcomes, or typed Conforma advisories. Stable run evidence is needed for failure counts, resets after success, and rejection of a particular run. Identify the smallest additive public API change needed in the dependency. Do not parse warning prose as a control protocol or count one failure per poll.
5. Verify that tag versus branch run identity and partial-error eligibility are supported by the library for this use. Resolve eligibility gaps in the dependency. Do not create another job catalog or use a second GitHub planner in the application. Additive dependency changes must preserve the CLI and its tests.
6. Keep public API extensions small: expose the observations the library already uses and any missing evidence it needs. Do not turn the dependency into the release assistant or add comment posting to it. Until required run metadata is available, continue read-only work and report the incomplete dispatch prerequisite.
7. The current API returns job names, not complete comments. Format only library-approved jobs using its documented command syntax: `/konflux-retest <job>` for PRs and `/retest <job> tag:<tag>` for tags. Check the dependency's `cmd/konflux-retest/report.go` when implementing this conversion. Post tag commands on the resolved commit. Use one command per comment for both target kinds to keep dispatch uniform.

**Acceptance:** the application builds with the imported dependency; only the adapter imports its types; tests cover PR/tag conversion, partial errors, no-job results, and exact comment formatting. Document unresolved library gaps rather than marking this milestone complete with invented evidence.

## 5. Milestone 2: implement read-only observations

Deliver real release discovery, build observations, and component image checks, usable without the web UI.

1. Implement GitHub reads for configured branch heads, PRs targeting those branches, merged PRs relevant to the current tag, Git tags, commit ancestry, version-pin files, and an optional manually supplied pinning PR. Resolve annotated and lightweight tags to commits. Use semantic version ordering, restrict tags to the configured major/minor stream, and exclude prereleases.
2. Paginate relevant API results. Do not silently treat a truncated or failed query as a complete inventory. Discover enough history to establish the configured branch/tag relationship and selected PR coverage; do not scan every historical PR in every repository. If no valid baseline tag is available, show that fact rather than assuming an empty change set.
3. Read actual ACS branch pins even if no pinning PR was supplied. Prefer manual pinning PR input for version one; do not build fuzzy automatic PR detection. Validate that the supplied PR targets the configured ACS branch. Preserve the distinction between current branch pins and proposed PR pins. Read checks for a supplied open pinning PR at its current head and include it once in active monitoring even when ordinary discovery omits it. Display it in pinning details and retry views without a separate inclusion decision. Convert verified PipelineRun image outputs into application-owned observations; inspect only the configured pinning PR’s exact reported digests, preserving current-head binding and unknown results. Keep output checks independent of the retry planner and component image prerequisites.
4. Call the Konflux adapter for active PRs and confirmed tags. Keep required GitHub check status useful for display without filtering the library's suggestions. Preserve library warnings and render Conforma failures as separately acknowledgeable advisories; acknowledgements do not make failed checks pass.
5. Identify real image patterns from upstream CI and pipeline definitions once during implementation and save them with immutable source provenance. Compute image names and tags locally at runtime. Reading actual ACS version-pin files remains part of GitHub discovery; it is independent of registry checks.
6. Implement a small built-in image pattern table for CI and Konflux component images and the ACS 4.x/5.x RC.0 image sets. Preserve verified `release-` repositories and `-fast` suffixes. Test exact references, selected-tag validation, and profiles verified against ACS 4.10, 4.11, and 5.0. Source repositories must not be consulted to compute image names at runtime. Keep later ACS RC.0 observations independent of component prerequisites. Missing tag selections remain unknown.
7. Check each expected image's registry existence. Separate confirmed absence from authentication errors, rate limits, timeouts, and malformed responses. Do not add an independent architecture/attestation audit or treat a build-success check as image existence. Existing library validation of retry inputs remains intact.
8. Resolve credentials outside the library: `GH_TOKEN`, then `GITHUB_TOKEN`, then an existing `gh` login; use available Quay credentials and an existing authenticated `oc` session where needed. Do not silently switch accounts after an invalid explicit token. Keep credentials out of snapshots, browser forms, and logs. Load credentials at startup; require restart after changing them in version one and explain this in the README.

**Acceptance:** HTTP fixture tests cover pagination, annotated tags, ancestry failure, merged/closed PR discovery, missing pin files, partial responses, both image families, and registry access uncertainty. All production paths in this milestone are read-only. Tests need no live credentials.

## 6. Milestone 3: implement pure release assessment

Deliver deterministic release recommendations from supplied observations and choices.

1. Require merged included PRs on their target branches, confirmed suitable component tags, all expected component images, and matching actual ACS release-branch pins before recommending Ready for ACS RC.0. Each PR choice takes effect immediately without session-wide confirmation. An empty selection is valid. Undecided PRs remain visible and do not block. ACS RC.0 image checks are informational for the later build stage.
2. Apply the tag cases in the table below. Determine membership through branch ancestry and selected PR merge positions. Do not add patch-equivalence or historical content audits. Do not silently change a confirmed tag when a newer one appears.
3. Reassess pin suitability immediately after an inclusion change. Mark outdated only when selected changes are absent; preserve pins that already cover them. Report unknown when evidence is insufficient. Assess an open pinning PR separately so an already-correct proposal is not needlessly updated.
4. Recommend creating/updating the pinning PR only when chosen component versions and both image families are ready. Otherwise explain the missing or unknown prerequisite. If the correct pins are already on the ACS branch, recognize completion without requiring another PR.
5. For a supplied pinning PR merged into the configured release branch, read its component versions at the merge commit and initialize untouched tag choices after verifying final version, stream, branch ancestry, and resolved commit. Preserve manual choices and do not reapply defaults after tag identity invalidation. Missing evidence leaves the affected choice unselected; never grant write permission. Test both discovery orders, partial observations, ACS 4.x Scanner, manual switches, and refreshes.
6. Describe the pinning goal using the configured release branch or existing pinning PR number, the verified version-file paths, and the confirmed versions for pins that differ.
7. Provide component-release documentation or verified commands when another tag is needed. Continue monitoring after readiness. If the previous ACS patch is published and no remaining PRs are found, present the design's tentative advice that a patch may be unnecessary; do not infer this from an empty/incomplete query or turn it into a prohibition.
8. Attach reasons, source links, and observation times to recommendations. An advisory failure that is outside the readiness predicate stays visible without secretly adding another release gate.

| Observed situation | Expected assessment |
| --- | --- |
| Tag equals component branch tip; no selected open PR | Current tag is suitable without a historical content audit |
| Branch advanced after tag; none of the later PRs are selected | Current tag remains suitable for the selected scope |
| A PR merged after the tag is selected | Need a tag covering that merge; reuse a suitable newer tag if available |
| An open component PR is selected | Wait for merge, then a tag covering its changes |
| Same-stream tag is not an ancestor of the configured branch head | Do not offer it as an eligible tag |
| Branch/tag ancestry cannot be established | Suitability is unknown |
| Newly included PR is already covered by pinned tag | Preserve valid pins |
| Pinning PR has correct versions but has not merged | PR pins are correct; actual branch pins still await the merge |
| Selected component image is missing or inaccessible | Do not recommend the pin update; distinguish missing from unknown |
| Server restarted | Reset PR choices and write grants; initialize verified component tags from a supplied merged pinning PR, otherwise require explicit tag choices; no PR-selection confirmation |

**Acceptance:** map-driven unit tests cover every row and explain the reason for each result. Assessment tests use values only: no HTTP, filesystem, clock sleeps, or credentials.

## 7. Milestone 4: implement session coordination and polling

Deliver continuous monitoring that survives browser disconnects and recovers from external outages.

1. Start one monitoring session per process. Support CLI configuration and a browser setup flow through the same validation function. CLI values initialize the session; explicit browser submissions update it. Incomplete configuration displays setup and starts no external monitoring.
2. Treat a change of release identity, target branches, or streams as a new selection context: clear affected choices, tag confirmations, write grants, and pending approvals before monitoring it. Do not carry authority into a newly configured target. Discard in-flight responses from the old configuration using a small session generation counter.
3. Poll active checks approximately every minute and PR/tag discovery approximately every five minutes. Make intervals configurable. Use bounded, cancellable work and per-source backoff with a configurable five-minute default cap. One unavailable component must not stop the others from updating. Registry checks run alongside GitHub discovery, so a blocked source read cannot prevent image results.
4. Avoid overlapping refreshes for the same target and unbounded goroutines. Keep network calls outside the session-state lock. Apply source results under a short lock only if their configuration generation and target SHA still match. Apply image results only if configuration generation and selected versions still match. Return copied snapshots to the UI.
5. Propagate head changes and opt-out-label observations to the dispatcher immediately. Preserve inclusion when a head changes. An ignored target receives no detail/check/log reads or writes. Removing an opt-out label does not restore a grant.
6. Append decisions, permission changes, approvals/rejections, dispatch attempts/results, and meaningful status transitions as JSON records to a configurable local log. Include release identity, target, SHA, timestamp, and origin. Do not log unchanged polls, tokens, arbitrary authorization headers, or credential-bearing error bodies. Report log-write failures visibly; never hide them as successful records.
7. Start read-only unless `-enable-writes` was supplied. Keep this immutable process setting in the dispatcher, reject grants and sends when disabled, and expose it in the snapshot so RW and send controls are disabled and greyed out. Keep read-only decisions and copyable comments available; startup opt-in never grants target permission.
8. On shutdown, cancel polling and outstanding reads, stop accepting new writes, and close the server and log. On restart, rediscover external work from scratch without replaying logs. Session choices and counters may reset; grants must be off and interactive mode on.

**Acceptance:** tests with controllable time verify polling intervals, backoff/recovery, independent component progress, ignored targets, discarded stale responses, browser-independent monitoring, and restart defaults. Run the race detector on concurrent session tests.

## 8. Milestone 5: implement the local browser UI

Deliver a usable read-only workflow and all session controls before connecting the real comment writer.

1. Render with `html/template` and embedded assets. Use ordinary form submissions and a small periodic refresh script if needed. Do not introduce a JavaScript rendering engine, WebSockets, or a frontend build step solely for updates.
2. Show ACS release identity in the page and browser tab title. Display compact selected-tag and pin pills, per-image numbered results, current/proposed pins in expandable details, overall readiness, and reasons for next actions. Every main content section must be foldable. Preserve each user-selected fold state, including nested evidence, across reloads and form submissions using stable identities and tab-local presentation state. Tag confirmation or new observations must not change visibility. Saving valid release configuration may close only its own panel; navigation explicitly reopens its destination and a saved closed state overrides retained URL fragments on reload. Separate component images before pinning from ACS RC.0 images after pinning and RC.0 creation; start the later section collapsed and let navigation reopen either section. Do not show configuration-valid or session-selection status cards. Keep unknown and stale observations visibly distinct from success.
3. Provide Included and Allow posting retry comments controls independently. Show undecided/excluded state explicitly; clearing inclusion is not Ignore. Provide Ignore, tag confirmation/switch, pinning PR input, interactive-mode setting, and Conforma acknowledgement actions.
4. Show exact copyable retest comments without authorization. When writes are eligible, show the target, SHA, payload, and run evidence for confirmation. Display rate-limit waiting, rejected/stale proposals, permission revocation, and opt-out-label state. Never turn off interactive mode merely because no browser is connected.
5. Bind to loopback. Protect state-changing requests against cross-origin submission using conventional host/origin checks and CSRF protection; never mutate state on GET. Escape externally supplied titles, messages, and log excerpts. Keep the browser free of service credentials.
6. Keep UI handlers thin. Convert requests into typed application actions and render returned state. Readiness and authorization must remain enforced by backend modules even if a client crafts requests manually.

**Acceptance:** handler tests cover controls, independent inclusion/RW state, escaped external text, local request protection, and rendering of unknown evidence. Manually exercise the browser with fixture-backed services, including reload and disconnect. Verify that every recommendation has a visible explanation.

## 9. Milestone 6: implement authorized retest dispatch

Deliver the only external write path. Finish the library contract needed for run-specific behavior before enabling this milestone's production writer.

Use this dispatch sequence:

1. Select a current library proposal. Do not synthesize eligibility from a failed check, required-check status, an elapsed timeout, or a missing run.
2. Check ignore state, target-scoped grant, interactive mode, rejection state, and cooldown. If interactive mode is on, request confirmation only when dispatch can proceed without queuing approval.
3. Refresh the target identity and opt-out label and obtain a fresh library result. Verify that the approved SHA, job, relevant run, destination, and exact payload still match an eligible proposal. Preserve unaffected eligible jobs in partial results, but withhold anything whose required identity or authority is unknown.
4. Recheck the current local grant and approval immediately at the write boundary. Serialize local revocation and the decision to enter the writer so a revocation received before dispatch prevents the send. Do not hold a global session lock across remote reads. Limit each target to one in-flight comment attempt.
5. Consume the approval once and reserve the target's minimum comment interval as dispatch starts. Post exactly one command to the correct PR or tag commit. Treat uncertain delivery as an attempt for rate limiting; never use an HTTP client's automatic write retry to bypass these controls.
6. Record sent, failed, or uncertain delivery. Subsequent retry eligibility comes from fresh library results and the same permission/rate-limit rules. Do not add application-level comment-history recovery, durable deduplication, or an indefinite wait for a new run.

Enforce the one-minute configurable minimum gap across all jobs on a target. After ten distinct consecutive unsuccessful eligible runs for a job/target/SHA, show a warning and permit that job at most one comment per configurable hour. Other jobs retain the normal target gap. Use run identities and success observations, not poll counts. Do not invent unobserved failures if evidence is incomplete.

Preserve the design's revocation boundary: already dispatched requests may complete. A fresh external check cannot prevent a remote head or label changing after that read; do not claim an atomic transaction with GitHub. Local permission revocation must still obey the serialized dispatch boundary.

**Acceptance:** fake-writer tests prove that no comment is sent after a pre-dispatch revocation, head change, opt-out label, ignore action, stale approval, rejected run, unknown authority, or library withdrawal of eligibility. Test concurrent duplicate submissions, single-use approval, per-target rate limiting, per-job slowdown, success/SHA resets, and partial results. Pending/queued/running suppression must be tested through the library adapter contract. Tests must never post live comments.

## 10. Milestone 7: validate the complete application and hand it over

1. Run a fixture-backed end-to-end scenario: start empty, configure a release, select PRs, observe merges, confirm tags, observe both image families, suggest pinning, observe the pinning merge, and reach readiness with explanations. Then include another PR and verify the appropriate statuses change while monitoring continues.
2. Run the same application flow with real read adapters and a disabled/fake writer when credentials are available. Read-only smoke testing is sufficient for handoff; do not post comments merely to validate integration. Report if credentials or service access prevented this check.
3. Verify fresh startup against already completed external work. No repeated tag creation or pinning PR should be recommended solely because local state was lost. Individual PR choices apply immediately. A supplied merged pinning PR initializes untouched tag choices from its merge commit after tag eligibility is verified; other tags require explicit selection. Refreshes preserve manual changes, and grants remain off.
4. Verify dependency isolation by inspecting imports and running the application against fake implementations of discovery, image inspection, retry planning, and writing. Replacing an adapter must not require changing release rules or templates.
5. Format every edited Go file with `gofmt -w`; use `goimports` when reorganizing imports. Follow the applicable Go instructions and document exported declarations. Run targeted tests during development, then the final commands below. If a project lint configuration exists by then, run its configured checks too. Check changed packages for unused code; do not add routine deadcode scans or speculative benchmarks.
6. Write a README with local dependency setup, build/run examples, CLI/browser configuration, credential precedence and restart behavior, permission controls, log location, supported release scope, and known integration limitations. Explain that the application never merges or creates release artifacts.
7. Hand over the files changed, validations performed, unresolved gaps, and exact local run command. Leave any push/submission commands to the user. Do not claim completion while a required library contract or image inventory remains unresolved.

Run these commands from `/Users/prygiels/code/release-assistant` once the implementation exists:

```sh
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/release-assistant
```

If an authorized dependency change was necessary, run its relevant tests and required checks in `/Users/prygiels/code/konflux-retest` too. Report the application and dependency results separately. No coverage improvement or performance claim is required; report measurements only if actually collected.

## 11. Completion criteria

The implementation is complete when all seven milestones pass and the following properties hold:

1. The retry library is an imported dependency behind one adapter, with no copied planner or CLI subprocess integration.
2. Real external integrations can be replaced through constructor wiring and narrow consumer interfaces; the release assessment runs without external services.
3. The user can understand every recommendation from visible statuses and evidence.
4. Branch/tag selection follows the agreed rules, both component image families are verified before pinning, and already completed external work is recognized.
5. Writes obey target permissions, exact confirmation when interactive, fresh library eligibility, opt-out labels, and rate limits.
6. Restart, outages, partial data, and stale responses preserve uncertainty and reset write authority as specified.
7. There is one executable, one in-memory session per process, and a local action log. No deferred feature has been added to compensate for an unclear implementation choice.
