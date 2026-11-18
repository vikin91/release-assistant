# ACS release assistant

A local browser application for preparing one ACS 4.x or 5.x patch release, from component PRs through ACS pinning and RC.0 image checks. It monitors Fact, Collector, applicable Scanner versions, ACS PRs, component tags, build evidence, registry images, and actual release-branch pins. Readiness is advisory. Monitoring continues when the browser closes and after readiness is reached.

## Run

Use Go 1.26.0 or later. The local `replace` uses the checkout at `/Users/prygiels/code/konflux-retest`. Use revision `38540d9` or a compatible later revision that supports `AllowKonfluxLookup`. The assistant imports the library directly. It neither invokes its CLI nor imports its internal packages.

```sh
cd /Users/prygiels/code/release-assistant
go run ./cmd/release-assistant
```

Open [the local assistant](http://127.0.0.1:8080) and supply the ACS version and target branch, plus each component's repository and major.minor stream. The browser derives its read-only branch as `release-<stream>`; branch overrides remain available through CLI flags. Incomplete configuration shows setup and starts no external monitoring. Browser submissions use the same configuration validation as CLI startup. Scanner is required for ACS 4.x and omitted for ACS 5.x.

Supplying `--version` at startup also pre-fills the component repository, stream, and derived branch from a built-in table: ACS 4.10 uses Fact 0.2, Collector 3.24, and Scanner 2.39; 4.11 uses 0.3, 3.25, and 2.40; 5.0 uses 0.4 and 3.26 without Scanner; later 5.x minors advance Fact and Collector by one. ACS 4.9 and older are rejected. Only CLI startup applies these values. Explicit component flags win, and changing the version in the browser does not re-apply the table.

Saving valid configuration starts discovery immediately. The page fetches release updates in the background every second during initial loading, then every minute during monitoring. It updates changed content in place while preserving expanded panels, focus, and unsaved input. A hidden browser tab pauses browser refresh requests; server monitoring continues. Failed requests leave the last results visible and show a retry notice. Forms submit through normal page navigation. The browser remembers the selected work tab, expanded panels, and activity log reading position across navigation. Saving release configuration may close its own panel.

You can supply the same configuration through flags. This example illustrates explicit inputs; choose streams and branches appropriate to your release:

```sh
go run ./cmd/release-assistant \
  --version 5.0.1 --branch release-5.0 \
  --fact-branch release-0.3 --fact-stream 0.3 \
  --collector-branch release-3.25 --collector-stream 3.25 \
  --port 8080
```

Run `go run ./cmd/release-assistant --help` for repository overrides, optional `--pinning-pr`, polling, backoff, request timeout, and retry limits. Defaults are one-minute active-check polls, five-minute discovery, a five-minute backoff cap, one comment per minute per target, and an hourly per-job cooldown after ten distinct eligible unsuccessful runs. Failure counts never increase merely because a poll repeats. Writes are disabled by default. Start with `--enable-writes` to allow retry comments after per-target RW permission and required confirmation. Without this flag, RW and send controls are disabled in the UI and the server rejects grants and sends.

Use a different port for each instance. Avoid overlapping targets across instances; there is no cross-process coordination.

## Authentication

Credentials load once at startup. Restart after changing environment variables or authenticated sessions:

1. GitHub uses `GH_TOKEN`, then `GITHUB_TOKEN`, then `gh auth token --hostname github.com`. An invalid explicit token never causes an account fallback.
2. Quay uses `QUAY_USERNAME` and `QUAY_PASSWORD`, or the existing `QUAY_RHACS_ENG_RO_USERNAME` and `QUAY_RHACS_ENG_RO_PASSWORD` pair when neither primary variable is supplied. Use a read-only account scoped to the release image repositories you are authorized to inspect. Anonymous access remains possible for public images. Authentication failures leave image availability unknown.
3. Konflux uses the existing `oc whoami --show-server` and `oc whoami --show-token` session. Each authenticated lookup requires an explicit `--allow-konflux-lookup APP_ID,API_HOST,NAMESPACE` startup flag. Repeat the flag for each trusted combination. Use the GitHub App's numeric ID, the cluster API hostname without a scheme or port, and the exact namespace. The startup token is available only for the active `oc` hostname. With no approved combinations, affected runs stay unverified.

The assistant enables the retry library's strict run-identity mode. It verifies original PipelineRun annotations through the cluster API or Tekton Results archive, including repository, SHA, GitHub check ID, and PR number or full tag ref. If evidence is unavailable or ambiguous, affected retries remain unconfirmed. A branch run on the same commit is insufficient evidence for a tag retry. Typed Conforma findings remain visible as commit-scoped advisories even when their exact run association cannot be established.

The adapter preserves structured PipelineRun creation failures as commit-scoped check evidence, including their category, cluster, reported reason, source check, and read time. The relevant work tab groups these reports by cluster and retains earlier attempts separately. Logs summarize newly observed reports without treating them as proof that a cluster is currently unavailable.

Failed reads mark retained reports as stale. A newer verified check for the same job and monitored target can supersede an older report without proving cluster-wide recovery. These observations neither authorize retries nor add release gates, and collecting them makes no additional health-probe requests. Developer Conforma findings remain advisories without triggering build identity lookups or monitoring backoff on their own.

## Work tabs

All four tabs remain available throughout the release. Switching tabs changes the view; monitoring continues in the background. Component and ACS PR discovery begins with valid configuration, even when no component release tags exist or are selected. PR choices take effect immediately, and an empty selection is valid.

1. **Component PRs:** review the PRs targeting the configured component release branches. Include the changes you want in this patch, review checks and retry suggestions, and merge them manually when ready. Tag selection is optional during this work. New PRs continue to appear after tags or pins have been selected. The tab points you to PR preparation when the previous pins are known and complete inventories show no eligible tags for any configured component.
2. **Component releases:** follow each component's release guide to cut tags and trigger releases manually, then select the intended tags. You can use an existing suitable tag, leave the choice empty while preparing a newer tag, or use **Prepare a new tag** to clear a selection. The tab compares each previous ACS pin with the newest eligible tag in the configured stream and flags incomplete evidence. Newly discovered tags remain alternatives until you choose one. The tab shows selected-PR coverage, build observations, CI and Konflux images, and previous component cycles.
3. **ACS PRs:** review backports, dependency updates, and the pinning PR together. Copy guarded pinning commands when selected component tags cover the included changes and both component image families exist. Supply a pinning PR number to compare proposed versions separately from actual branch pins. Its checks, verified Konflux output-image availability, and retry suggestions appear without another Included choice. Ordinary ACS PR discovery and monitoring continue after pinning merges, so you can include later changes at any time.
4. **ACS RCs:** inspect the existing read-only RC.0 image results. Checks start after the supplied pinning PR is verified as merged and branch pins match selected tags. This tab retains RC.0 scope; candidate creation, later candidates, and RC build retries remain manual and untracked.

**Included** controls release scope. **RW allowed** separately controls permission to post a retry comment. Undecided and excluded PRs stay visible; Exclude stops detailed reads and retries, and Ignore also hides the PR. Clearing a tag selection revokes that tag's write permission while PR monitoring continues.

Tag proposals use final semantic versions from the configured major.minor stream and verified branch ancestry. Included merged component PRs need verified tag containment. Selecting a different tag reassesses coverage, images, and pins without changing unrelated PR choices. A supplied merged pinning PR can initialize untouched tag choices from its merge-commit versions after eligibility is verified. Manual choices, including clearing a tag to prepare a newer release, remain in effect across refreshes.

The numbered image lists distinguish Available, Not found, and Unable to check. Registry checks use [saved, verified naming patterns](internal/images/patterns.md) for component and RC.0 images and exact reported digests for pinning-PR Konflux outputs. Successful build checks alone do not establish image existence. Missing tag choices, missing output references, access failures, and malformed responses remain unconfirmed.

Ready for ACS RC.0 requires all included PRs merged into their configured branches, suitable selected component tags, both component image families, and matching actual release-branch pins. A matching open pinning PR still awaits merge. Green branch pins mean selected versions are on the branch; readiness also depends on the other conditions. RC.0 image availability describes the later build stage and does not block preparation for creating RC.0.

Pinning commands use `cdrox`, the configured ACS release branch, verified version-file paths, and named staging. They are suggestions for you to review and run. GitHub-required checks remain display evidence; the retry library decides retest eligibility. Conforma acknowledgements record human review without changing checks, permissions, or readiness.

## Comment permission

The only external write is one authorized Konflux retest comment. PR commands use `/konflux-retest <job>`; tag commands use `/retest <job> tag:<tag>` on the resolved commit. The library alone decides eligibility and sequencing. The assistant never merges, enables automerge, pushes branches, creates PRs or tags, adds labels, or triggers RC.0.

The process starts read-only. To enable write controls, restart with:

```sh
go run ./cmd/release-assistant -enable-writes
```

The startup flag does not grant permission to any target. Interactive mode starts on and requires an exact single-use confirmation in addition to RW permission. Turning it off permits unattended retest comments for individually granted targets. Every attempt rechecks current target identity, opt-out status, library eligibility, exact job/run/payload, and rate limits. There is no persistent outgoing queue. Rejection suppresses the specific run until reconsidered. Transport uncertainty reserves the cooldown and is logged; it does not cause immediate replay.

A PR head change revokes its grant and preserves inclusion. A tag grant binds repository, tag name, and resolved commit. The `no-release-assistant` PR label overrides grants and clears them; removing it does not restore permission. Local revocation prevents new dispatch admission. Requests already admitted may complete, and a fresh GitHub read cannot provide a transaction against later remote changes.

## State, logs, and limits

Session state is in memory. Browser reload preserves it; server restart clears choices, confirmations, counters, and grants, starts interactive mode on, and rediscovers completed external work. Logs are not replayed.

The Activity and errors console at the bottom shows the latest 200 events from this process, including initial discovery progress, safe GitHub request failures, retry waits, and recovery. It refreshes every two seconds independently of forms. Request diagnostics include endpoints and HTTP status or timeout/network categories; credentials and response bodies are excluded.

The CLI prints activity to stdout as `<timestamp> <Level>: <message>`, using RFC 3339 timestamps with a date and timezone, including events beyond the UI console's 200-event window. Messages use sentences such as `Checking release tags on fact/release-0.2.` and `Could not read build results in fact. GitHub returned HTTP 403.` Levels are Error for failures, Warn for retry waits and invalidated authority, Info for normal activity, and Debug for routine diagnostics. All levels are printed. Discovery messages name the branch and only report data requested in that refresh. Commit IDs are labeled as branch heads, pull request heads, or tag commits. Routine request-count summaries and repeated context lines are omitted from CLI output. Proposed retry comments appear in Debug entries with indented text. Output failures are visible in the UI. Credentials, response bodies, and unchanged polls are excluded. The assistant does not create a log file; redirect stdout to retain the logs.

The server binds to loopback and protects forms with local Host/Origin checks and CSRF tokens. Read failures preserve prior observations for display while invalidating affected current evidence. Per-source polling and bounded requests allow other components to progress during outages.

Image naming patterns are built into the executable: CI uses `quay.io/rhacs-eng/<image>:<version>` and Konflux uses `quay.io/rhacs-eng/release-<image>:<version>-fast`. Scanner V2 variants apply to ACS 4.x. ACS RC.0 uses `<acs-version>-rc.0` and a separate image set for ACS 4.x and 5.x, verified against streams 4.10, 4.11, and 5.0. Naming changes require updating the saved patterns and their tests; running the app never fetches Makefiles or CI scripts to determine image names. The retry library retains its independent source validation for job eligibility. The scope excludes RC.0 creation/build retries, architecture or attestation audits, persistent storage, and automated component release creation.

## Validate

Tests use fixture services and fake writers; they never post live comments:

```sh
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/release-assistant
golangci-lint run ./...
node --test internal/web/app_js_test.js
```

The linter configuration enables `govet`, `ineffassign`, `staticcheck`, and `unused`; use a golangci-lint v2 binary built for Go 1.26 or later. Fixture tests cover tag-free PR monitoring, new PR and tag discovery, merge observation, tag confirmation, both image families, pinning, readiness, and later inclusion changes through the scheduled polling path. Adapter tests exercise the public retry library with HTTP fixtures and test external writes only through fake transports.

The imported retry library is MIT-licensed. YAML decoding uses `go.yaml.in/yaml/v3`, the retry library's existing dependency (MIT and Apache licenses); no browser build toolchain or additional service framework is required.

The live read smoke test is opt-in: set `ACS_LIVE_READ_SMOKE=1` when running
`go test ./internal/integration -run TestLiveReadAdapters -v`. It constructs no
writer. `ACS_LIVE_SOURCE_SHA` restricts that smoke test to an already verified
immutable ACS revision. Registry authorization failures are reported as partial
observations, never as successful availability checks.
