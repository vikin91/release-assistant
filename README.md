# ACS release assistant

A local browser application for preparing one ACS 4.x or 5.x patch release before RC.0. It monitors Fact, Collector, applicable Scanner versions, ACS PRs, component tags, build evidence, registry images, and actual release-branch pins. Readiness is advisory. Monitoring continues when the browser closes and after readiness is reached.

## Run

Use Go 1.26.0 or later. This development module requires the local `github.com/vikin91/konflux-retest` checkout at `/Users/prygiels/code/konflux-retest`, including its public run-evidence extension. The `v0.0.0` requirement with a local replacement is intentional; it does not name a published release. The assistant imports the library directly. It neither invokes its CLI nor imports its internal packages.

```sh
cd /Users/prygiels/code/release-assistant
go run ./cmd/release-assistant
```

Open [the local assistant](http://127.0.0.1:8080) and supply the ACS version and target branch, plus each component's repository and major.minor stream. The browser derives its read-only branch as `release-<stream>`; branch overrides remain available through CLI flags. Incomplete configuration shows setup and starts no external monitoring. Browser submissions use the same configuration validation as CLI startup. Scanner is required for ACS 4.x and omitted for ACS 5.x.

Supplying `--version` at startup also pre-fills the component repository, stream, and derived branch from a built-in table: ACS 4.10 uses Fact 0.2, Collector 3.24, and Scanner 2.39; 4.11 uses 0.3, 3.25, and 2.40; 5.0 uses 0.4 and 3.26 without Scanner; later 5.x minors advance Fact and Collector by one. ACS 4.9 and older are rejected. Only CLI startup applies these values. Explicit component flags win, and changing the version in the browser does not re-apply the table.

Saving valid configuration starts discovery immediately. The page fetches release updates in the background every second during loading and selection, then every minute during monitoring. It updates changed content in place while preserving expanded panels, focus, and unsaved input. Hidden tabs pause background requests. Failed requests leave the last results visible and show a retry notice. Forms submit through normal page navigation. The browser tab remembers expanded panels and the activity log reading position across navigation. Saving release configuration may close its own panel.

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
2. Quay uses `QUAY_USERNAME` and `QUAY_PASSWORD`, or the existing `QUAY_RHACS_ENG_RO_USERNAME` and `QUAY_RHACS_ENG_RO_PASSWORD` pair when neither primary variable is supplied. Anonymous access remains possible for public images. Authentication failures leave image availability unknown.
3. Konflux uses the existing `oc whoami --show-server` and `oc whoami --show-token` session. The startup token is returned only for that cluster's hostname. The application does not log in, change contexts, or save credentials.

The assistant enables the retry library's strict run-identity mode. It verifies original PipelineRun annotations through the cluster API or Tekton Results archive, including repository, SHA, GitHub check ID, and PR number or full tag ref. If evidence is unavailable or ambiguous, affected retries remain unconfirmed. A branch run on the same commit is insufficient evidence for a tag retry. Typed Conforma findings remain visible as commit-scoped advisories even when their exact run association cannot be established.

## Session workflow

1. Review discovered PRs. **Included** controls release scope; **RW allowed** separately controls comment permission. Undecided and excluded PRs remain visible. **Ignore** hides a PR and stops its detailed reads and writes. Each choice takes effect immediately; no additional selection confirmation is required, and an empty selection is valid.
2. Review component tags. If the supplied ACS pinning PR is already merged into the configured release branch, its merge-commit versions initialize untouched Fact, Collector, and applicable Scanner selections after tag eligibility is verified. Use **Change tag…** to select another version; refreshes preserve your choice and write permission remains off. Other tags need confirmation. Proposals use final semantic versions from the supplied major.minor stream and verified branch ancestry. A newer tag never silently replaces the confirmed tag. Included open PRs wait for merge; included merged changes need verified tag containment.
3. Review the numbered image list: Available confirms the exact tag exists, Not found confirms absence, and Unable to check explains a missing tag selection or failed registry request. Both CI and Konflux families are listed for the selected component versions and the later ACS RC.0 build. Image references are computed from [saved, verified naming patterns](internal/images/patterns.md) and the selected versions. Image checks make registry requests only, run alongside GitHub discovery, and continue to produce results when source repositories are unavailable. Missing tag choices, access failures, and malformed registry responses remain unknown. Successful build checks do not establish image existence.
4. Copy the suggested component release documentation or guarded pinning commands when their prerequisites are met. Commands use `cdrox`, the explicitly configured ACS branch, verified version-file paths, and named staging. Supply an existing pinning PR number to compare its proposed versions separately from actual branch pins. While it is open, pinning details show its CI/Konflux checks and verified Konflux image outputs; retry suggestions appear automatically without an extra Included choice. Registry availability uses the reported image digest. Missing outputs or registry access remain unknown. Its RW checkbox follows the same independent permission rules. The top pin pills link to details or the PR. Green branch pins mean the selected versions are on the release branch; yellow means waiting or unable to check, and red means a verified mismatch or PR problem. A matching open PR stays yellow until merged.
5. Review recommendations and their underlying evidence. Ready for ACS RC.0 requires all included PRs merged into the configured branches, suitable confirmed component tags, both component image families, and matching actual release-branch pins. ACS RC.0 image availability is checked separately as the subsequent build stage. An open pinning PR does not satisfy the last condition.

GitHub-required check status is displayed without filtering the retry library's job suggestions. Conforma acknowledgements record human review; they do not turn failures into successes. Missing build runs remain visible through library observations; the assistant does not invent retry commands.

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

The CLI prints every recorded event to stdout as `<timestamp> <Level>: <message>`, using RFC 3339 timestamps with a date and timezone, including events beyond the UI console's 200-event window. Messages use sentences such as `Checking release tags on fact/release-0.2.` and `Could not read build results in fact. GitHub returned HTTP 403.` Levels are Error for failures, Warn for retry waits and invalidated authority, Info for normal activity, and Debug for routine diagnostics. All levels are printed. Release, repository, commit, and origin details appear in separate Debug lines. Payloads such as proposed comments appear in Debug entries with indented text. Output failures are visible in the UI. Credentials, response bodies, and unchanged polls are excluded. The assistant does not create a log file; redirect stdout to retain the logs.

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

The linter configuration enables `govet`, `ineffassign`, `staticcheck`, and `unused`; use a golangci-lint v2 binary built for Go 1.26 or later. The fixture release test covers configuration, selection, merge observation, tag confirmation, both image families, pinning, readiness, and a later inclusion change. Adapter tests exercise the public retry library with HTTP fixtures and test external writes only through fake transports.

The imported retry library is MIT-licensed. YAML decoding uses `go.yaml.in/yaml/v3`, the retry library's existing dependency (MIT and Apache licenses); no browser build toolchain or additional service framework is required.

The live read smoke test is opt-in: set `ACS_LIVE_READ_SMOKE=1` when running
`go test ./internal/integration -run TestLiveReadAdapters -v`. It constructs no
writer. `ACS_LIVE_SOURCE_SHA` restricts that smoke test to an already verified
immutable ACS revision. Registry authorization failures are reported as partial
observations, never as successful availability checks.
