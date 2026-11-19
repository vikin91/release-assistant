# ACS release assistant

The ACS release assistant is a local browser app for preparing one ACS patch release. It tracks component and ACS pull requests (PRs), component tags and images, the component versions pinned on the ACS branch, and the latest release candidate (RC) for the patch.

It reads GitHub, Quay, and Konflux to suggest what to do next. You merge PRs, publish tags, and start releases in the upstream tools. The assistant starts in read-only mode. Its only optional write is a Konflux retry comment.

## Quick start

Read-only access to GitHub and Quay is sufficient to start the assistant
and monitor a release. To let the assistant post retry comments, enable
writes and use a GitHub credential with permission to comment on the
target PR or commit. [Retry comments](#retry-comments)

For Konflux, log in to the build cluster before starting the assistant. An
active login to that cluster is enough. GitHub and Quay can run without it.

```sh
oc login https://api.stone-prd-rh01.pg1f.p1.openshiftapps.com --token='YOUR_OPENSHIFT_TOKEN'
```

From the repository root, replace the credential placeholders and start
the assistant. This example uses App ID `296509` for `rh-acs-tenant`.

```sh
export APP_ID=296509
GH_TOKEN='YOUR_GITHUB_TOKEN' QUAY_USERNAME='YOUR_QUAY_USERNAME' QUAY_PASSWORD='YOUR_QUAY_PASSWORD' \
  go run ./cmd/release-assistant --version 5.0.1 --branch release-5.0 \
    --allow-konflux-lookup "${APP_ID},api.stone-prd-rh01.pg1f.p1.openshiftapps.com,rh-acs-tenant"
```

Open the [local assistant](http://127.0.0.1:8080). [Setup details](#setup-details)

## Setup details

Have the GitHub CLI (`gh`) and OpenShift CLI (`oc`) available in the
terminal you’ll use to start the assistant. The assistant reads credentials
at startup. Restart it after changing a token or login.

1. **Set up Go.** Install Go 1.26.0 or later. Go downloads the
   `github.com/vikin91/konflux-retest` version pinned in [go.mod](go.mod).

2. **Authenticate to GitHub.** If you use the GitHub CLI for credentials,
   check whether you’re signed in and log in if needed.

   ```sh
   gh auth status --hostname github.com || gh auth login --hostname github.com
   ```

   If you supply a token through the environment, export it as `GH_TOKEN`
   or `GITHUB_TOKEN`. The assistant checks `GH_TOKEN` first, then
   `GITHUB_TOKEN`, then the saved `gh` login. A fine-grained token needs
   access to the monitored repositories and read access to contents, pull
   requests, checks, and commit statuses. **Actions: read** adds workflow
   run and trigger information to RC validation. Reading required checks
   from branch protection also needs **Administration: read**.

3. **Set Quay credentials for private images.** Load a read-only username
   and password or robot token into `QUAY_USERNAME` and `QUAY_PASSWORD`,
   then export both.

   ```sh
   export QUAY_USERNAME QUAY_PASSWORD
   ```

   If both are unset or empty, the assistant uses
   `QUAY_RHACS_ENG_RO_USERNAME` and `QUAY_RHACS_ENG_RO_PASSWORD`.
   Public images can be checked anonymously.

4. **Log in to Konflux and identify the lookup to allow.** In the OpenShift
   console for the cluster that runs your builds, choose **Copy login
   command** and run the provided `oc login` command. Check the active server.

   ```sh
   oc whoami --show-server
   ```

   Your account needs read access to the relevant namespace’s PipelineRuns
   or their Tekton Results records. The assistant uses the token from the
   active `oc` login for that cluster.

   Use the hostname from the API URL printed by `oc`. The namespace is the
   segment after `/ns/` in the Konflux build URL.

   Get the App ID from GitHub. Replace `CHECK_RUN_ID` with the numeric ID of
   a Konflux check run and use the repository that owns the check.

   ```sh
   gh api 'repos/stackrox/fact/check-runs/CHECK_RUN_ID' \
     --jq '.app | {id, slug, owner: .owner.login}'
   ```

   Verify the app slug and owner, then set `APP_ID` to the returned numeric
   `id` in the launch example.

   Repeat `--allow-konflux-lookup` for each trusted app, host, and namespace
   combination. If you still need a check-run ID, start without the flag.
   A `lookup denied by caller policy` message in **Activity and errors**
   names the repository and check-run ID. Verify the combination and
   restart with the flag.

## Release configuration

You can also run `go run ./cmd/release-assistant` and fill in **Configure
release** in the browser. Enter the ACS patch version and target branch,
then each component’s repository and major.minor release stream. The form
derives the component branch as `release-<stream>`. Use a CLI branch flag
for an override. Add a pinning PR number when you have one. Saving valid
configuration starts discovery.

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

## Konflux verification

The assistant uses PipelineRun annotations from the cluster API or Tekton Results archive to verify the repository, commit, check-run ID, and PR number or full tag ref. This matters when a branch build and a tag build share a commit. The header pill shows observed pipeline admission. If reads fail, **Log** shows which endpoint failed or paused and when the assistant can check again. A reported pipeline creation failure describes a specific attempt. Use its evidence link to investigate.

## Work through a release

You can switch tabs at any time. Monitoring continues in the background, including after you close the browser or reach readiness.

1. **Component PRs.** Review open PRs targeting the configured component branches. Mark the changes you want to ship as **Included**, review their checks, and merge them in GitHub. You can start before any component tags exist. If an included PR closes without merging, it stays visible for a decision.
2. **Component releases.** Use each component's release guide to cut a tag and start the release, or select an existing suitable tag. The assistant checks that the tag is a final version in the configured stream, belongs to the release branch, and contains the merges of included component PRs. Newer tags appear as options. Use **Prepare a new tag** to clear the current selection while you work. The chosen tag contains all changes in its commit history, regardless of your PR choices.
3. **ACS PRs.** Review backports and dependency updates. When the selected tags cover the intended changes and both component image families are available, review the generated pinning commands. The commands expect `cdrox` to enter a clean StackRox checkout and use Git and Python 3. Updating an existing PR also uses `gh`. The ACS PR list identifies open PRs that change a release-specific component pin file. Select **Use as pinning PR** on any open release PR, or enter its number under **Release configuration**, to monitor its proposed pins, checks, and verified Konflux output images. Clear the number to resume discovery. Select **Detect pinning PR even when a number is supplied** to review candidates while retaining the entered number until you confirm a PR. The confirmed pinning PR is automatically included. Retry comments still require separate permission. The assistant reports the actual branch pins separately. A verified merged pinning PR can fill in untouched tag choices. Refreshes preserve manual choices, including a cleared selection.
4. **ACS RCs.** See the highest-numbered StackRox RC tag on the release branch, how many commits landed after it, and links to their merged PRs. If no RC tag exists, open the linked release workflow. As soon as GitHub verifies the RC tag and its commit, the assistant checks its builds and images. Monitoring works without a pinning PR or selected component tags. **RC validation** shows failed, canceled, and running checks on the candidate commit, including GitHub Actions and Conforma checks. Open GitHub for the full check list or to rerun Actions. Eligible Konflux checks use the tag retry controls.

PR choices apply immediately, and an empty selection is valid. **Excluded** keeps a PR visible and stops detailed monitoring and retries. **Ignore & hide** also removes it from the normal list. **Allow posting retry comments** is the separate permission to send a retry comment.

Release preparation requires that the included PRs have merged into their configured branches, the chosen component tags cover the included component changes, the component CI and Konflux images are available, and the actual ACS branch pins match. Readiness is advisory. Candidate images are outputs of the RC build.

The image views distinguish **Available**, **Not found**, and **Unable to check**. A successful build check and a published image are separate observations. Registry errors or missing output references leave availability unknown. See [Image naming patterns](internal/images/patterns.md).

## Retry comments

Review and copy proposed comments in read-only mode. To send comments from the assistant, restart with your usual flags and add `--enable-writes`.

1. Set **Allow posting retry comments** on the PR or tag you want to permit. Release inclusion alone grants no write access.
2. Leave **Confirm each write operation towards GitHub** on to approve a single exact comment at a time. To allow eligible retries without confirmation for permitted targets, change this setting under **Application settings** and select **Save**.

The retry library decides which Konflux jobs are eligible. PR retries use `/konflux-retest <job>`. Tag retries use `/retest <job> tag:<tag>` on the resolved commit. GitHub required-check settings inform the status display. Marking a Conforma advisory as reviewed only records your review for the session.

A PR head or target branch change revokes its grant. The `no-release-assistant` label also clears the grant, and removing the label requires a new grant. Tag grants bind the repository, tag name, and commit. Clearing or switching a tag selection revokes the old grant. Revocation prevents new sends. A request already in progress may complete.

Before sending, the assistant rechecks the target, current permission, eligible run, and exact comment. By default, it waits at least one minute between comments on a target. After ten distinct consecutive unsuccessful eligible runs for a job on the same target and commit, it waits at least an hour between comments for that job. Repeated polls do not increase the failure count. If delivery is uncertain, the assistant records it and waits for the cooldown before reconsidering.

On the **ACS PRs** tab, **Checks of Included PRs** groups Konflux retry comments, GitHub Actions rerun commands, and Prow comments by PR. On the other workflow tabs, **Manual CI retries** shows GitHub Actions job links and copyable `gh api` commands for monitored targets. For failed Prow presubmits, copy the listed `/test <job>` commands and post them together on the linked PR. These options are available in read-only mode.

## Standalone CI retry CLI

Use `ci-retest` to check GitHub Actions jobs and OpenShift CI presubmits independently of the dashboard. It accepts the same target lists and GitHub credentials as `konflux-retest`.

```sh
go run ./cmd/ci-retest --repo stackrox --prs 24136 24135
go run ./cmd/ci-retest --repo stackrox --tags 4.11.8-rc.0,5.0.0-rc.0
```

Open an offered Actions job link and select the job rerun control on GitHub. For Prow, post the printed comment on the linked PR. It contains one configured `/test <job>` line per verified failed presubmit. Prow evaluates the PR when it processes the comment. The CLI performs only reads. Each target gets a separate report, including warnings when the run, tag trigger or current attempt cannot be verified. Run `go run ./cmd/ci-retest --help` for authentication and input details.

## Monitoring and logs

The server listens on loopback. It polls active checks every minute and discovers PRs and tags every five minutes by default. Failed reads can leave the last results visible with their age while current evidence remains unverified. Browser refresh runs separately and preserves open panels and unsaved form input.

Under **Application settings**, **Concurrent GitHub reads during startup** accepts 1–5 and defaults to 3. The five-minute window starts when the first configured release begins monitoring. **Concurrent GitHub reads after startup** accepts 1–3 and defaults to 1 for the rest of the session. Saving either limit updates how queued reads are admitted. The hourly read budget and GitHub rate-limit backoff apply throughout.

**Log** shows the latest 1,000 events and refreshes every two seconds. The terminal prints timestamped activity to stdout, including discovery, check status changes, retry decisions, and Konflux read pauses and recovery. Redirect stdout if you need to retain the log.

GitHub discovery reuses commit history in memory by default and logs cache hits at Debug level. In **Settings**, uncheck **Reuse GitHub commit history in memory** and select **Save** to turn it off and empty the cache. **Clear cache** empties it without changing the setting. A restart starts with an empty, enabled cache.

Choices, grants, approvals, counters, and recent activity live in memory. Reloading the browser preserves the session. Restarting clears it, restores **Confirm each write operation towards GitHub**, and rediscovers release work already completed in external systems.

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
