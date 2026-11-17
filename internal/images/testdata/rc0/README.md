# ACS RC.0 source fixtures

1. The 410 excerpts come from stackrox/stackrox commit `de310ded87bdec2cbe7f2750c6f9625632a39260` (release-4.10).
2. The 500 excerpts come from commit `32ef302fd73a452b292add4a6a117ce753bcf72a` (release-5.0).
3. `ci-lib.sh` preserves the five relevant functions from `scripts/ci/lib.sh`. `start-release.yml` preserves the RC.0 tagging step from `.github/workflows/start-release.yml`. Tekton YAML retains the original identity, parameter values/defaults, and pipeline reference; unrelated tasks are omitted.
4. Tests read these excerpts as data. No fixture script or workflow is executed.
