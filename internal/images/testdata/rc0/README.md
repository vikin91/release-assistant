# ACS RC.0 source fixtures

These excerpts preserve the upstream release definitions used by the image
tests. The tests read YAML and shell files as data.

| Directory | StackRox release branch | Source revision |
| --- | --- | --- |
| `410` | `release-4.10` | [de310ded87bd](https://github.com/stackrox/stackrox/tree/de310ded87bdec2cbe7f2750c6f9625632a39260) |
| `500` | `release-5.0` | [32ef302fd73a](https://github.com/stackrox/stackrox/tree/32ef302fd73a452b292add4a6a117ce753bcf72a) |

`ci-lib.sh` keeps the relevant functions from `scripts/ci/lib.sh`.
`start-release.yml` keeps the RC.0 tagging step from
`.github/workflows/start-release.yml`. The Tekton YAML excerpts keep the
resource identities, parameters and defaults, and pipeline references
needed by the tests.

See [Image naming patterns](../../patterns.md) for the current catalog.
