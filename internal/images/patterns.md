# Image naming patterns

Edit the image names, repository prefixes, and tag suffixes in
[image_config.go](image_config.go) when upstream naming changes. The inspector
builds the expected references from that file and checks the resulting tags in
Quay. The links below document how the defaults were verified.

## Component images

For each selected component version `V`, the CI reference is
`quay.io/rhacs-eng/<name>:V` and the Konflux reference is
`quay.io/rhacs-eng/release-<name>:V-fast`. The saved CI definitions are
`scripts/ci/lib.sh`; the saved Konflux definitions are
`.tekton/retag-<name>.yaml`.

Fact uses `fact`, Collector uses `collector`, and ACS 4.x Scanner V2 uses
`scanner`, `scanner-db`, `scanner-slim`, and `scanner-db-slim`. ACS 5.x does not
include Scanner V2.

## ACS RC.0 images

For normalized ACS version `V`, the RC.0 checker uses the release tag `V-rc.0`.
CI checks `main`, `central-db`, `roxctl`, `scanner-v4`, `scanner-v4-db`,
`stackrox-operator`, `collector`, and `fact` in both `quay.io/rhacs-eng`
and `quay.io/stackrox-io`. ACS 4.x also checks the four Scanner V2 variants
in both namespaces. In `rhacs-eng`, the CI bundle and index use
`stackrox-operator-bundle:vV-rc.0` and `stackrox-operator-index:vV-rc.0`.

Konflux checks `release-main`, `release-central-db`, `release-operator`,
`release-roxctl`, `release-scanner-v4`, `release-scanner-v4-db`,
`release-collector`, and `release-fact`, all tagged `V-rc.0`. ACS 4.x also
checks the four Scanner V2 retags with that release tag. The bundle uses
`release-operator-bundle:vV-rc.0`. The separate catalog build uses
`stackrox-operator-index:vV-rc.0-fast` outside the release snapshot.

## Pinning PR build images

PR build images use literal `IMAGE_URL` and `IMAGE_DIGEST` results from
identity-verified PipelineRuns or their archived records. The assistant checks
the reported immutable digest in Quay; it does not derive a PR tag from Git
history, version files, or a branch name. CI check results remain visible without
claiming CI image availability from check success alone.

The verified [ACS 5.0 main pipeline](https://github.com/stackrox/stackrox/blob/32ef302fd73a452b292add4a6a117ce753bcf72a/.tekton/main-pipeline.yaml)
exports both image results. This is evidence discovery from the actual run,
separate from the fixed component and RC.0 naming patterns above.

## Provenance streams

The patterns were verified against these immutable StackRox revisions:

| Stream | Revision | Source links |
| --- | --- | --- |
| 4.10 | `de310ded87bdec2cbe7f2750c6f9625632a39260` | [CI library](https://github.com/stackrox/stackrox/blob/de310ded87bdec2cbe7f2750c6f9625632a39260/scripts/ci/lib.sh), [fact retag PipelineRun](https://github.com/stackrox/stackrox/blob/de310ded87bdec2cbe7f2750c6f9625632a39260/.tekton/retag-fact.yaml), [retag pipeline](https://github.com/stackrox/stackrox/blob/de310ded87bdec2cbe7f2750c6f9625632a39260/.tekton/retag-pipeline.yaml), [release workflow](https://github.com/stackrox/stackrox/blob/de310ded87bdec2cbe7f2750c6f9625632a39260/.github/workflows/start-release.yml) |
| 4.11 | `8b36e9ad94e885ab7d7756fe88de74d9655c8716` | [CI library](https://github.com/stackrox/stackrox/blob/8b36e9ad94e885ab7d7756fe88de74d9655c8716/scripts/ci/lib.sh), [fact retag PipelineRun](https://github.com/stackrox/stackrox/blob/8b36e9ad94e885ab7d7756fe88de74d9655c8716/.tekton/retag-fact.yaml), [retag pipeline](https://github.com/stackrox/stackrox/blob/8b36e9ad94e885ab7d7756fe88de74d9655c8716/.tekton/retag-pipeline.yaml), [release workflow](https://github.com/stackrox/stackrox/blob/8b36e9ad94e885ab7d7756fe88de74d9655c8716/.github/workflows/start-release.yml) |
| 5.0 | `32ef302fd73a452b292add4a6a117ce753bcf72a` | [CI library](https://github.com/stackrox/stackrox/blob/32ef302fd73a452b292add4a6a117ce753bcf72a/scripts/ci/lib.sh), [fact retag PipelineRun](https://github.com/stackrox/stackrox/blob/32ef302fd73a452b292add4a6a117ce753bcf72a/.tekton/retag-fact.yaml), [retag pipeline](https://github.com/stackrox/stackrox/blob/32ef302fd73a452b292add4a6a117ce753bcf72a/.tekton/retag-pipeline.yaml), [release workflow](https://github.com/stackrox/stackrox/blob/32ef302fd73a452b292add4a6a117ce753bcf72a/.github/workflows/start-release.yml) |

Other ACS 4.x patch streams use the ACS 4.x family with 4.10 provenance;
other ACS 5.x patch streams use the ACS 5.x family with 5.0 provenance.
