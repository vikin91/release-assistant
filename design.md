# ACS release assistant design

The assistant runs one ACS patch release session in a local Go process. The
session combines observations from GitHub, Konflux, and Quay with the user's
release choices. It evaluates readiness continuously and serves a browser UI
with Go templates and embedded JavaScript. See the [README](README.md) for
setup and the release workflow.

## Package responsibilities

| Package | Responsibility |
| --- | --- |
| `cmd/release-assistant` | Parse flags, load credentials, wire adapters, and run the loopback server. |
| `internal/release` | Define release data and evaluate tag suitability, readiness, and next steps from observations and choices. |
| `internal/app` | Own the session, schedule work, merge observations, request dispatch, and log activity. |
| `internal/github` | Discover branch, PR, tag, pin, and check state. Post comments through a separate writer. |
| `internal/githubread` | Schedule GitHub reads and enforce request budgets and backoff. |
| `internal/konflux` | Adapt the public `konflux-retest` library, verify build identity, and back off failed Konflux endpoint reads. |
| `internal/images` | Resolve configured image references and check their availability in Quay. |
| `internal/dispatch` | Own write grants, confirmations, rejection state, failure counters, and the final checks before sending. |
| `internal/web` | Render session snapshots and submit user actions. Protect local forms with Host and Origin checks and CSRF tokens. |

The CLI wires concrete adapters into consumer-defined interfaces. Release
assessment uses Go values and makes no network requests. The retry library
owns Konflux job eligibility, dependencies, and sequencing. The dispatcher
uses the library's recommendations and independently checks authority and
timing. GitHub required-check settings inform the UI.

## Discovery and evidence

1. A valid configuration specifies an ACS version and target branch, component repositories, release branches and streams, and an optional pinning PR. The CLI seeds component streams for supported versions. The browser derives branches from streams and retains CLI branch overrides while the corresponding component input stays the same.
2. PR discovery and active checks run independently of tag selection. The user can select open PRs throughout the release. Included PRs that close without merging remain actionable. For a merged component PR, the session retains any inclusion choice and checks the merge's containment in the selected tag.
3. Discovery uses Git tags ordered by semantic version. Tag eligibility requires a final version in the configured stream and verified ancestry on the release branch. A newer tag is an option until the user selects it. A verified merged pinning PR can initialize untouched tag choices from the versions at its merge commit. The chosen tag includes every change in its history.
4. Observations carry source identity, time, and current availability. A failed read may preserve prior values for display. Conditions that require current evidence stay unconfirmed until a successful refresh. Independent work can continue during a source outage.
5. Konflux verification ties a PipelineRun to the repository, commit, GitHub check run, and PR or tag ref. Without that evidence, a displayed status or build creation failure cannot authorize a retry. Conforma findings remain advisory. Reviewing a finding records a session decision.
6. The registry inspector derives component and RC.0 references from the saved catalog. Pinning PR output images use the URL and digest reported by an identity-verified run. Registry checks report availability independently of build status. [Image naming patterns](internal/images/patterns.md)

## Readiness and RC.0

Readiness to create RC.0 requires the included PRs to merge into their
configured branches, selected component tags to cover the intended changes,
both component image families to be available, and the actual ACS branch pins
to match the selected versions. Each inclusion choice takes effect immediately.
The release assessment accepts an empty selection.

When the component prerequisites are met, the assistant can suggest local
commands to prepare or update a pinning PR. The user reviews and runs those
commands. The UI reports proposed pinning PR versions and actual release-branch
pins separately.

RC.0 monitoring requires a supplied pinning PR, a verified merge into the
configured ACS branch, and current branch pins that match selected component
tags. It follows the tag, CI and Konflux image availability, and validation
checks on the candidate commit. Eligible Konflux jobs can receive tag retry
comments through the normal dispatch path. The user starts the upstream release
workflow and reruns GitHub Actions in GitHub.

## Write permissions

1. The process starts read-only. With `--enable-writes`, the user can grant **RW allowed** on individual targets. A PR grant binds the current head and base branch. A tag grant binds the repository, tag, and resolved commit.
2. **Confirm each write operation towards GitHub** starts enabled. Approval is single-use and tied to the exact target, run, and payload. With confirmation disabled, an individually granted target can receive eligible retries automatically.
3. A target identity change, tag deselection, or the PR label `no-release-assistant` clears the affected permission. Revocation prevents new dispatch admission. An already admitted request may finish. Each send revalidates the target and eligibility against current evidence.
4. The dispatcher enforces the per-target comment gap and the longer per-job cooldown after the configured threshold of distinct consecutive unsuccessful runs. An uncertain delivery consumes the cooldown. Rejection suppresses the specific run until the user reconsiders it.

## Session lifecycle

The process keeps choices, evidence, and grants in memory. Browser reloads and
tab switches leave the session running. When the process restarts, it starts
with fresh local state and discovers release work already completed upstream.
Each process has its own session and retry limits.

Activity is written to stdout and retained in a 200-event browser console.
Credential loading happens once at startup. Logs omit credentials and raw
response bodies. The browser saves panel state and updates the visible
dashboard independently of server polling.

## Decision history

These notes record the original design review. See the
[original implementation plan](implementation-plan.md) for the proposed
milestones.


1. Round 1: manual merges; target-scoped permission; interactive approval on by default; revocation boundary; Go rendering with embedded JavaScript allowed.
2. Round 2: separate inclusion and RW controls; revoke permission on new PR commits; manual pinning PR creation; supplied component branches; independent components; successive tag discovery; separate app importing konflux-retest.
3. Round 3: advisory continuous monitoring; startup discovery; reuse existing tags; newest-tag proposals with human confirmation; both image families; user disposition of Conforma errors; immediate outdated-pin notice. Retry policy and confirmation/label scope remain unresolved.

4. Round 4: confirmed failure required before retest; no comments for queued/pending/running jobs; use Konflux evidence when GitHub is insufficient; one comment/minute/target; configurable threshold near ten and possible slower retries; unattended mode permitted; no-release-assistant write opt-out; focus on required checks and dependencies; newer tags require confirmation; one-command application preferred.

5. Round 5: minimum one-minute target comment gap rather than scheduled posting; ten consecutive failures per job/target/commit then hourly retries; wait indefinitely for unseen executions unless user explicitly intervenes; opt-out label permits reads and clears RW; advisory Conforma notices; one executable; external credentials only; persistence optional; one release per instance with prominent release identity.

6. Round 6: empty-state rediscovery required; DB only as optimization; no multi-instance coordination; manual session pinning PR input acceptable; cdrox-based copyable commands without git-spice; Git tags ordered by SemVer rather than dates; PR scope restricted to GitHub-required checks, superseding optional-prerequisite expansion; configurable polling with exponential backoff.

7. Round 7: konflux-retest is authoritative for PR and tag retry dependencies, including non-required prerequisites; application must not filter those suggestions. Select highest patch within the configured component stream. Log actions to a file. Accept CLI/browser configuration. Retry relevant cancelled/timed-out jobs. Keep stale-approval handling simple, with revalidation and user notice.

8. Round 8: component branches/streams are user inputs through CLI/UI; GHA/Prow remain read-only with future retry support deferred; missing builds are reported and observed, with no invented retry command. Version-one behavioral design is agreed; implementation is deferred to another session.

9. Round 9: overall readiness requires a confirmed session PR selection, merged included PRs, suitable confirmed component tags, available component images, and matching release-branch pins. Always show the underlying statuses and explain recommendations. Tags must belong to the configured branch; a tag at its tip needs no historical content audit, while selected open PRs or selected PRs merged after the tag require a newer tag covering them. Delegate retry eligibility and pending/queued/running suppression to konflux-retest, including after restart; do not build application-level comment-history recovery or an indefinite wait for a run after a comment. This supersedes the corresponding recovery rules from rounds 4–5. Both CI and Konflux component images must exist before recommending an ACS pin update because its CI depends on those images.

10. Round 10: PR selection shows only open PRs, for both ACS and component repositories. Merged PRs disappear from selection; historical merge observations remain internal evidence for ancestry and coverage. Preserve the coverage requirement of a PR included while open after it merges. Component tag choice includes every change in that tag, regardless of earlier PR inclusion decisions. Do not ask users to include or exclude already merged PRs, including after startup or restart. This supersedes the merged-PR inclusion/exclusion wording and historical selection implications of earlier rounds.

11. Round 11: distinguish completed tag selection from remaining release work. Selected tags use a collapsed component/version summary, a Selected status, and explicit Change tag…/switch actions. Move SHA comparisons into Evidence and keep section navigation outside recommendations. PR inclusion uses Included, Excluded, and Ignore & hide buttons; independent PR/tag RW checkboxes save immediately. This supersedes the earlier two-checkbox PR control description while preserving permission and readiness rules.

12. Round 12: retry and exact-comment offer views follow accepted tags and explicitly Included PRs, rather than every observed open PR. Keep monitoring and write permission independent of this display filter. Replace advisory acknowledgement checkbox/save controls with Mark as reviewed and Mark as unreviewed buttons; explain the session-local flag and log entry while keeping failures, evidence, retry eligibility, permissions, and readiness unchanged.

13. Round 13: remove the redundant session-wide PR-selection confirmation and gate. Compact the top summary into selected-tag, actual-pin, and linked pinning-PR pills with explicit colour meanings. Replace generic image freshness cards with numbered per-image names, tags, availability, actionable error causes, and exact source links. Include read-only ACS RC.0 image checks as a later build stage, independent of the prerequisites for creating RC.0. This supersedes selection-confirmation and RC.0-image exclusions in earlier rounds.

14. Round 14: compute image references using saved upstream-verified naming patterns, rather than rediscovering scripts and Tekton files at runtime. Preserve the Konflux `release-` repository prefix and `-fast` suffix. Run registry checks independently of repository availability, bind observations to selected versions, and retain separate GitHub prerequisites for overall readiness. This supersedes runtime image-definition resolution in earlier rounds.

15. Round 15: show component prerequisites and later ACS RC.0 images in separate sections. All main sections are foldable; the later ACS RC.0 image section starts collapsed. This supersedes the combined image list presentation.

16. Round 16: initialize untouched Fact, Collector, and applicable Scanner tag choices from a supplied, verified merged ACS pinning PR at its merge commit. Keep the choices editable and preserve manual changes on refresh. Existing stream, branch ancestry, identity, readiness, and write-permission checks still apply. This supersedes requiring manual initial tag confirmation when the merged pinning PR supplies verified defaults.

17. Round 17: replace `-disable-writes` with the opt-in `-enable-writes` flag. Processes start read-only; disable and grey out UI write controls and reject grants and sends server-side unless the process was explicitly started with writes enabled. Per-target grants and interactive confirmation remain required.

18. Round 18: section visibility belongs to the user. Preserve all section and nested evidence folds in the browser tab across refreshes, form submissions, and changing tag status. Only saving release configuration may automatically close its own panel. This supersedes automatic tag-selection folding from earlier rounds.

19. Round 19: the configured pinning PR is an explicit monitoring target without a separate Included action. Display its checks, verified Konflux output-image availability, and library-authorized retries with independent RW controls. Remove repeated phase labels from individual image rows. Component prerequisites and later RC.0 images remain separate.
