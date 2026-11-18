# ACS release assistant

The ACS release assistant is a local browser app for preparing one ACS patch release. It tracks component and ACS pull requests (PRs), component tags and images, the component versions pinned on the ACS branch, and the latest release candidate (RC) for the patch.

It reads GitHub, Quay, and Konflux to suggest what to do next. You merge PRs, publish tags, and start releases in the upstream tools. The assistant starts in read-only mode. Its only optional write is a Konflux retry comment.

## Get started

1. Install Go 1.26.0 or later. The `replace` directive in [go.mod](go.mod) points to a local checkout of `github.com/vikin91/konflux-retest`. Use the revision recorded in `go.mod` or a compatible revision. If your checkout is elsewhere, adjust the local path in the directive.
2. Set up access to the services you need. See [Authentication](#authentication).
3. From the repository root, start the assistant for an ACS 5.0.1 patch.

   ```sh
   go run ./cmd/release-assistant --version 5.0.1 --branch release-5.0
   ```

4. Open the [local assistant](http://127.0.0.1:8080). Valid startup configuration starts discovery immediately. Check **Activity and errors** for progress and access problems.

You can also run `go run ./cmd/release-assistant` and fill in **Configure release** in the browser. Enter the ACS patch version and target branch, then each component's repository and major.minor release stream. The form derives the component branch as `release-<stream>`. Use a CLI branch flag for an override. Add a pinning PR number when you have one. Saving valid configuration starts discovery.

### Component defaults

The CLI uses the following defaults when you pass `--version`. The default repositories are `stackrox/fact`, `stackrox/collector`, and, where applicable, `stackrox/scanner`. Component flags override these defaults. If you enter or change the version in the browser, enter the component streams yourself.

| ACS stream | Fact | Collector | Scanner V2 |
| --- | --- | --- | --- |
| 4.10 | 0.2 | 3.24 | 2.39 |
| 4.11 | 0.3 | 3.25 | 2.40 |
| 5.0 | 0.4 | 3.26 | Omitted |
| 5.1 and later 5.x | Starts at 0.5 and increases by one per ACS minor | Starts at 3.27 and increases by one per ACS minor | Omitted |

Use a final patch version in ACS 4.10 or later 4.x, or ACS 5.x. For a later 4.x stream outside the table, supply component streams explicitly. Scanner V2 is required for ACS 4.x.

This example supplies all component streams explicitly for ACS 4.11.1. The CLI derives the component branches from those streams.

```sh
go run ./cmd/release-assistant \
  --version 4.11.1 --branch release-4.11 \
  --fact-stream 0.3 --collector-stream 3.25 --scanner-stream 2.40
```

Run `go run ./cmd/release-assistant --help` for repository and branch overrides, `--pinning-pr`, and monitoring and retry settings. Use a different `--port` and distinct targets for each concurrent instance. Each process manages its own permissions and retry limits.

## Authentication

The assistant loads credentials at startup. Restart it after changing a token, environment variable, or login.

| Service | Credential source |
| --- | --- |
| GitHub | `GH_TOKEN`, then `GITHUB_TOKEN`, then `gh auth token --hostname github.com`. The first available source wins. Grant **Actions: read** if you want RC validation to show the corresponding workflow runs and trigger information. |
| Quay | `QUAY_USERNAME` and `QUAY_PASSWORD`. If both are unset or empty, the assistant uses `QUAY_RHACS_ENG_RO_USERNAME` and `QUAY_RHACS_ENG_RO_PASSWORD`. Use read-only credentials for private release images. Public images support anonymous access. |
| Konflux | The active `oc` login at startup and an explicit `--allow-konflux-lookup` entry for each trusted GitHub App ID, API hostname, and namespace combination. The assistant uses the token only for the hostname reported by `oc`. |

### Allow Konflux lookups

`lookup denied by caller policy` means the GitHub check's app, API host, and namespace did not match an approved combination. Without a verified run, the assistant withholds retries that require that evidence.

1. Look up the GitHub App behind the check. Use the repository and the check-run ID from the warning. For example, this command queries a Fact check. Verify the app slug and owner, then record the numeric `id`.

   ```sh
   gh api repos/stackrox/fact/check-runs/131115008122 \
     --jq '.app | {id, slug, owner: .owner.login}'
   ```

2. Run `oc whoami --show-server` and confirm it matches the API for the check's Konflux cluster. For example, the UI on `konflux-ui.apps.stone-prd-rh01.pg1f.p1.openshiftapps.com` uses the API host `api.stone-prd-rh01.pg1f.p1.openshiftapps.com`. Find the namespace after `/ns/` in the check's build URL.
3. Set `APP_ID` to the numeric ID from step 1, then restart with your release flags and the trusted combination. Use the API hostname without a scheme or port. Repeat the flag for each combination you approve.

   ```sh
   go run ./cmd/release-assistant --version 5.0.1 --branch release-5.0 \
     --allow-konflux-lookup "${APP_ID:?Set APP_ID to the numeric GitHub App ID},api.stone-prd-rh01.pg1f.p1.openshiftapps.com,rh-acs-tenant"
   ```

The assistant uses PipelineRun annotations from the cluster API or Tekton Results archive to verify the repository, commit, check-run ID, and PR number or full tag ref. This matters when a branch build and a tag build share a commit. If reads fail, **Konflux status** shows which endpoint failed or paused and when the assistant can check again. A reported pipeline creation failure describes a specific attempt. Use its evidence link to investigate.

## Work through a release

You can switch tabs at any time. Monitoring continues in the background, including after you close the browser or reach readiness.

1. **Component PRs.** Review open PRs targeting the configured component branches. Mark the changes you want to ship as **Included**, review their checks, and merge them in GitHub. You can start before any component tags exist. If an included PR closes without merging, it stays visible for a decision.
2. **Component releases.** Use each component's release guide to cut a tag and start the release, or select an existing suitable tag. The assistant checks that the tag is a final version in the configured stream, belongs to the release branch, and contains the merges of included component PRs. Newer tags appear as options. Use **Prepare a new tag** to clear the current selection while you work. The chosen tag contains all changes in its commit history, regardless of your PR choices.
3. **ACS PRs and pinning.** Review backports and dependency updates. When the selected tags cover the intended changes and both component image families are available, review the generated pinning commands. The commands expect `cdrox` to enter a clean StackRox checkout and use Git and Python 3. Updating an existing PR also uses `gh`. Enter the pinning PR number under **Release configuration** to see its proposed pins, checks, and verified Konflux output images. The assistant reports the actual branch pins separately. A verified merged pinning PR can fill in untouched tag choices. Refreshes preserve manual choices, including a cleared selection.
4. **ACS RCs.** See the highest-numbered StackRox RC tag on the release branch, how many commits landed after it, and links to their merged PRs. If no RC tag exists, open the linked release workflow. Once the supplied pinning PR has merged and the branch pins match your selections, the assistant checks the current RC’s builds and images. **RC validation** shows failed, canceled, and running checks on the candidate commit, including GitHub Actions and Conforma checks. Open GitHub for the full check list or to rerun Actions. Eligible Konflux checks use the tag retry controls.

PR choices apply immediately, and an empty selection is valid. **Excluded** keeps a PR visible and stops detailed monitoring and retries. **Ignore & hide** also removes it from the normal list. **RW allowed** is the separate permission to send a retry comment.

**Release prerequisites met** means the included PRs have merged into their configured branches, the chosen component tags cover the included component changes, the component CI and Konflux images are available, and the actual ACS branch pins match. Readiness is advisory. Candidate images are outputs of the RC build.

The image views distinguish **Available**, **Not found**, and **Unable to check**. A successful build check and a published image are separate observations. Registry errors or missing output references leave availability unknown. See [Image naming patterns](internal/images/patterns.md).

## Retry comments

Review and copy proposed comments in read-only mode. To send comments from the assistant, restart with your usual flags and add `--enable-writes`.

1. Set **RW allowed** on the PR or tag you want to permit. Release inclusion alone grants no write access.
2. Leave **Confirm each comment** on to approve a single exact comment at a time. To allow eligible retries without confirmation for permitted targets, change this setting under **Retry comment settings** and select **Save mode**.

The retry library decides which Konflux jobs are eligible. PR retries use `/konflux-retest <job>`. Tag retries use `/retest <job> tag:<tag>` on the resolved commit. GitHub required-check settings inform the status display. Marking a Conforma advisory as reviewed only records your review for the session.

A PR head or target branch change revokes its grant. The `no-release-assistant` label also clears the grant, and removing the label requires a new grant. Tag grants bind the repository, tag name, and commit. Clearing or switching a tag selection revokes the old grant. Revocation prevents new sends. A request already in progress may complete.

Before sending, the assistant rechecks the target, current permission, eligible run, and exact comment. By default, it waits at least one minute between comments on a target. After ten distinct consecutive unsuccessful eligible runs for a job on the same target and commit, it waits at least an hour between comments for that job. Repeated polls do not increase the failure count. If delivery is uncertain, the assistant records it and waits for the cooldown before reconsidering.

## Monitoring and logs

The server listens on loopback. It polls active checks every minute and discovers PRs and tags every five minutes by default. Failed reads can leave the last results visible with their age while current evidence remains unverified. Browser refresh runs separately and preserves open panels and unsaved form input.

**Activity and errors** shows the latest 200 events and refreshes every two seconds. The terminal prints timestamped activity to stdout, including discovery, check status changes, retry decisions, and Konflux read pauses and recovery. Redirect stdout if you need to retain the log.

Choices, grants, approvals, counters, and recent activity live in memory. Reloading the browser preserves the session. Restarting clears it, restores **Confirm each comment**, and rediscovers release work already completed in external systems.

## Develop and validate

Run the checks from the repository root. The default test suite uses fixtures for external services and fake writers for comments. Use golangci-lint v2 built for Go 1.26 or later.

```sh
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/release-assistant
golangci-lint run ./...
node --test internal/web/app_js_test.js
```

For a manual browser check, start the local fixture server.

```sh
ACS_BROWSER_FIXTURE=1 go test ./internal/integration -run '^TestBrowserFixture$' -count=1 -v
```

Open [fixture controls](http://127.0.0.1:18088/fixture), then **Open assistant**. Advance through the fixture observations as you exercise the UI. Select **Finish browser validation** in the fixture controls to stop the test. The fixture uses fake services and records comments locally in memory.

Use this command to smoke-test live GitHub and Quay reads with your credentials.

```sh
ACS_LIVE_READ_SMOKE=1 go test ./internal/integration -run '^TestLiveReadAdapters$' -count=1 -v
```

The live test reads the ACS 4.10 release branch by default. Set `ACS_LIVE_SOURCE_SHA` to check the version files at a known immutable ACS 4.10 revision. Registry access failures may appear as partial observations. The test has no comment writer.

See [Application design](design.md) for architecture and invariants and [Image naming patterns](internal/images/patterns.md) for the catalog and upstream sources.
