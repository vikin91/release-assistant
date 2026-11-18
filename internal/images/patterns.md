# Image naming patterns

The inspector builds component and RC.0 references from the saved catalog
and checks them in Quay. When upstream naming changes, update the catalog,
the image tests, and the source references in this guide. [Image catalog](image_config.go)

## Component images

For each selected component version `V`, the CI reference is
`quay.io/rhacs-eng/<name>:V` and the Konflux reference is
`quay.io/rhacs-eng/release-<name>:V-fast`. For example, Fact tag `0.4.7`
maps to `quay.io/rhacs-eng/fact:0.4.7` and
`quay.io/rhacs-eng/release-fact:0.4.7-fast`.

Fact uses `fact`, Collector uses `collector`, and ACS 4.x Scanner V2 uses
`scanner`, `scanner-db`, `scanner-slim`, and `scanner-db-slim`. ACS 5.x does not
include Scanner V2.

## ACS RC.0 images

For normalized ACS version `V`, the RC.0 checker uses the release tag `V-rc.0`.
CI checks `main`, `central-db`, `roxctl`, `scanner-v4`, `scanner-v4-db`,
`stackrox-operator`, `collector`, and `fact` in both `quay.io/rhacs-eng`
and `quay.io/stackrox-io`. ACS 4.x also checks the four Scanner V2 variants
in both namespaces. In `rhacs-eng`, the CI bundle and index add a leading
`v` to the tag. For example, an ACS 5.0.1 bundle uses
`quay.io/rhacs-eng/stackrox-operator-bundle:v5.0.1-rc.0`.

Konflux checks `release-main`, `release-central-db`, `release-operator`,
`release-roxctl`, `release-scanner-v4`, `release-scanner-v4-db`,
`release-collector`, and `release-fact`, all tagged `V-rc.0`. ACS 4.x also
checks the four Scanner V2 retags with that release tag. The bundle uses
`release-operator-bundle:vV-rc.0`. The separate catalog build uses
`stackrox-operator-index:vV-rc.0-fast` outside the release snapshot.

## Pinning PR build images

PR build images use literal `IMAGE_URL` and `IMAGE_DIGEST` results from
identity-verified PipelineRuns or their archived records. The assistant checks
the reported immutable digest in Quay. The CI check results show build status
independently of image availability.

The ACS 5.0 main pipeline exports both image results. [ACS 5.0 main pipeline](https://github.com/stackrox/stackrox/blob/32ef302fd73a452b292add4a6a117ce753bcf72a/.tekton/main-pipeline.yaml)

## Source revisions

The component CI definitions come from `scripts/ci/lib.sh`. Component
Konflux definitions come from `.tekton/retag-<name>.yaml` and the shared
retag pipeline. Use the immutable StackRox revisions in this table to
trace the naming patterns.

| Stream | Revision | Source links |
| --- | --- | --- |
| 4.10 | `de310ded87bdec2cbe7f2750c6f9625632a39260` | [CI library](https://github.com/stackrox/stackrox/blob/de310ded87bdec2cbe7f2750c6f9625632a39260/scripts/ci/lib.sh), [fact retag PipelineRun](https://github.com/stackrox/stackrox/blob/de310ded87bdec2cbe7f2750c6f9625632a39260/.tekton/retag-fact.yaml), [retag pipeline](https://github.com/stackrox/stackrox/blob/de310ded87bdec2cbe7f2750c6f9625632a39260/.tekton/retag-pipeline.yaml), [release workflow](https://github.com/stackrox/stackrox/blob/de310ded87bdec2cbe7f2750c6f9625632a39260/.github/workflows/start-release.yml) |
| 4.11 | `8b36e9ad94e885ab7d7756fe88de74d9655c8716` | [CI library](https://github.com/stackrox/stackrox/blob/8b36e9ad94e885ab7d7756fe88de74d9655c8716/scripts/ci/lib.sh), [fact retag PipelineRun](https://github.com/stackrox/stackrox/blob/8b36e9ad94e885ab7d7756fe88de74d9655c8716/.tekton/retag-fact.yaml), [retag pipeline](https://github.com/stackrox/stackrox/blob/8b36e9ad94e885ab7d7756fe88de74d9655c8716/.tekton/retag-pipeline.yaml), [release workflow](https://github.com/stackrox/stackrox/blob/8b36e9ad94e885ab7d7756fe88de74d9655c8716/.github/workflows/start-release.yml) |
| 5.0 | `32ef302fd73a452b292add4a6a117ce753bcf72a` | [CI library](https://github.com/stackrox/stackrox/blob/32ef302fd73a452b292add4a6a117ce753bcf72a/scripts/ci/lib.sh), [fact retag PipelineRun](https://github.com/stackrox/stackrox/blob/32ef302fd73a452b292add4a6a117ce753bcf72a/.tekton/retag-fact.yaml), [retag pipeline](https://github.com/stackrox/stackrox/blob/32ef302fd73a452b292add4a6a117ce753bcf72a/.tekton/retag-pipeline.yaml), [release workflow](https://github.com/stackrox/stackrox/blob/32ef302fd73a452b292add4a6a117ce753bcf72a/.github/workflows/start-release.yml) |

The catalog uses the same rules across the supported 4.x streams and the
same rules across 5.x streams. The RC.0 tests use excerpts from the 4.10
and 5.0 revisions. [RC.0 source fixtures](testdata/rc0/README.md)
