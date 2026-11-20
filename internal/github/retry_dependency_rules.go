package github

// retryWorkflowDependency links an Actions job to the workflow that supplies its inputs.
type retryWorkflowDependency struct {
	Workflow       string
	Job            string
	RequiredChecks []string
	Needs          string
	Name           string
}

// retryDependencies lists dependencies that GitHub Actions cannot express with needs.
var retryDependencies = map[string][]retryWorkflowDependency{
	"stackrox/stackrox": {
		{
			Workflow: stackroxE2EDispatchWorkflow,
			Job:      waitForActionsImagesJob,
			Needs:    stackroxBuildWorkflow,
			Name:     "Build",
			RequiredChecks: []string{
				waitForActionsImagesJob,
				"e2e-gke-upgrade-tests / gke-upgrade-tests-central",
				"e2e-gke-upgrade-tests / gke-upgrade-tests-postgres",
				"e2e-nongroovy-tests-gke / run / gke-nongroovy-e2e-tests",
			},
		},
	},
}

/*
Audited merge dependency trees

These snapshots record required jobs and their prerequisites. The planner reads
the current PR workflow to resolve job IDs, matrix jobs, and prerequisites.
"needs" lists prerequisites. "calls" continues the tree in a local workflow.
Konflux uses its own planner.

stackrox/stackrox

  master at 22c102789358a78e332af80ca73e568bcebaca95
    Required Prow: ci/prow/gke-ui-e2e-tests
    build.yaml
      Required roots: build-and-push-main, build-and-push-scanner, push-main-manifests, push-operator-manifests
      build-and-push-db needs define-job-matrix
      build-and-push-main needs define-job-matrix, pre-build-cli, pre-build-docs, pre-build-go-binaries, pre-build-oss-notice, pre-build-ui
      build-and-push-operator needs define-job-matrix, pre-build-go-binaries, pre-build-oss-notice
      build-and-push-scanner needs build-and-push-db, define-job-matrix, pre-build-go-binaries, pre-build-oss-notice
      define-job-matrix
      pre-build-cli needs define-job-matrix
      pre-build-docs
      pre-build-go-binaries needs define-job-matrix
      pre-build-oss-notice
      pre-build-ui
      push-main-manifests needs build-and-push-main, define-job-matrix
      push-operator-manifests needs build-and-push-operator, define-job-matrix
    check-crd-compatibility.yaml
      Required roots: check-crd-compatibility
      check-crd-compatibility needs detect-changes
      detect-changes calls detect-changes.yml
    detect-changes.yml
      detect
    e2e-dispatch.yaml
      Required roots: e2e-gke-upgrade-tests, e2e-nongroovy-tests-gke, wait-for-images
      detect-changes calls detect-changes.yml
      e2e-gke-upgrade-tests needs should-dispatch, wait-for-images, calls e2e-gke-upgrade-tests.yaml
      e2e-nongroovy-tests-gke needs should-dispatch, wait-for-images, calls e2e-nongroovy-tests-gke.yaml
      should-dispatch needs detect-changes
      wait-for-images needs should-dispatch
    e2e-gke-upgrade-tests.yaml
      Required roots: gke-upgrade-tests-central, gke-upgrade-tests-postgres
      check-postgres-upgrade
      gke-upgrade-tests-central
      gke-upgrade-tests-postgres needs check-postgres-upgrade
    e2e-nongroovy-tests-gke.yaml
      Required roots: run
      run calls e2e-nongroovy-tests.yaml
    e2e-nongroovy-tests.yaml
      Required roots: gke-nongroovy-e2e-tests
      gke-nongroovy-e2e-tests
    style.yaml
      Required roots: check-dependent-images-exist, check-generated-files, golangci-lint, misc-checks, style-check
      check-dependent-images-exist needs detect-changes
      check-generated-files needs detect-changes
      detect-changes calls detect-changes.yml
      golangci-lint needs detect-changes
      misc-checks
      style-check
    unit-tests.yaml
      Required roots: go, go-bench, go-postgres, local-roxctl-tests, ui
      detect-changes calls detect-changes.yml
      go needs detect-changes
      go-bench needs detect-changes
      go-postgres needs detect-changes
      local-roxctl-tests needs detect-changes
      ui needs detect-changes

  release-4.10 at 13120bb5123105dcece7a6c251dcf89b6d7aa8e0
    build.yaml
      Required roots: push-main-manifests, push-operator-manifests
      build-and-push-main needs define-job-matrix, pre-build-cli, pre-build-docs, pre-build-go-binaries, pre-build-ui
      build-and-push-operator needs define-job-matrix
      define-job-matrix
      pre-build-cli
      pre-build-docs
      pre-build-go-binaries needs define-job-matrix
      pre-build-ui
      push-main-manifests needs build-and-push-main, define-job-matrix
      push-operator-manifests needs build-and-push-operator, define-job-matrix
    scanner-build.yaml
      Required roots: push-scanner-manifests
      build-and-push-scanner needs define-scanner-job-matrix, pre-build-scanner-go-binary
      define-scanner-job-matrix
      pre-build-scanner-go-binary needs define-scanner-job-matrix
      push-scanner-manifests needs build-and-push-scanner, define-scanner-job-matrix
    style.yaml
      Required roots: check-dependent-images-exist
      check-dependent-images-exist

  release-4.11 at 81281912d3511c0863576008aa64d2422fc201a3
    build.yaml
      Required roots: push-main-manifests, push-matching-collector-scanner, push-operator-manifests, push-scanner-manifests
      build-and-push-main needs define-job-matrix, pre-build-cli, pre-build-docs, pre-build-go-binaries, pre-build-oss-notice, pre-build-ui
      build-and-push-operator needs define-job-matrix, pre-build-go-binaries
      build-and-push-scanner needs define-job-matrix, pre-build-go-binaries, pre-build-oss-notice
      define-job-matrix
      pre-build-cli needs define-job-matrix
      pre-build-docs
      pre-build-go-binaries needs define-job-matrix
      pre-build-oss-notice
      pre-build-ui
      push-main-manifests needs build-and-push-main, define-job-matrix
      push-matching-collector-scanner needs define-job-matrix
      push-operator-manifests needs build-and-push-operator, define-job-matrix
      push-scanner-manifests needs build-and-push-scanner, define-job-matrix

  release-5.0 at 4e1faee1665ade2c7c238f30a9a574d219467582
    build.yaml
      Required roots: push-main-manifests, push-matching-collector-scanner, push-operator-manifests, push-scanner-manifests
      build-and-push-db needs define-job-matrix
      build-and-push-main needs define-job-matrix, pre-build-cli, pre-build-docs, pre-build-go-binaries, pre-build-oss-notice, pre-build-ui
      build-and-push-operator needs define-job-matrix, pre-build-go-binaries, pre-build-oss-notice
      build-and-push-scanner needs build-and-push-db, define-job-matrix, pre-build-go-binaries, pre-build-oss-notice
      define-job-matrix
      pre-build-cli needs define-job-matrix
      pre-build-docs
      pre-build-go-binaries needs define-job-matrix
      pre-build-oss-notice
      pre-build-ui
      push-main-manifests needs build-and-push-main, define-job-matrix
      push-matching-collector-scanner needs define-job-matrix
      push-operator-manifests needs build-and-push-operator, define-job-matrix
      push-scanner-manifests needs build-and-push-scanner, define-job-matrix

stackrox/collector

  master at 129987e7488e0c4084290cb6f40e2aa04ba27065
  release-3.24 at 325f5972ec81d02e23596f77c430355c3af38a66
  release-3.25 at 83b80ac0de663c26a043b21531cc96e1a28cc0e2
  release-3.26 at 041e3a5b1e6378555b8a878651d7baff4480abdf
    collector-builder.yml
      build-builder-image needs builder-needs-rebuilding
      builder-needs-rebuilding
      create-multiarch-manifest needs build-builder-image, builder-needs-rebuilding
    collector.yml
      Required roots: build-collector-image
      build-collector-image
    init.yml
      common-variables
    main.yml
      Required roots: build-collector, unit-tests
      build-builder-image needs init, calls collector-builder.yml
      build-collector needs build-builder-image, init, calls collector.yml
      init calls init.yml
      unit-tests needs build-builder-image, calls unit-tests.yml
    unit-tests.yml
      Required roots: unit-tests
      unit-tests

stackrox/fact

  main at ab9082c8135a1b49b548d568342ad8c77564998d
  release-0.4 at cb02e68273261cf9835c1b419cf0708cd6d557a1
    ci.yml
      Required roots: basic, integration-tests
      basic
      container-build calls container-build.yml
      integration-tests needs container-build, test-containers-build, calls integration-tests.yml
      test-containers-build calls test-containers-build.yml
    container-build.yml
      container needs vars
      manifest-rhacs-eng needs container, vars
      manifest-stackrox-io needs container, vars
      vars
    integration-tests.yml
      Required roots: integration-tests
      integration-tests
    test-containers-build.yml
      build-test-containers needs determine-qa-tag, should-build-test-containers
      determine-qa-tag needs should-build-test-containers
      should-build-test-containers

  release-0.2 at 5c3cdb95be54e2b015f78e6cb5ff9198508373cd
    ci.yml
      Required roots: basic, integration-tests
      basic
      container needs vars
      integration-tests needs manifest-rhacs-eng, manifest-stackrox-io, vars, calls integration-tests.yml
      manifest-rhacs-eng needs container, vars
      manifest-stackrox-io needs container, vars
      vars
    integration-tests.yml
      Required roots: integration-tests
      integration-tests

  release-0.3 at 8a5289d504e584d19ba61ca335e69a02e882f0ff
    ci.yml
      Required roots: basic, integration-tests
      basic
      container needs vars
      integration-tests needs manifest-rhacs-eng, manifest-stackrox-io, test-containers-build, vars, calls integration-tests.yml
      manifest-rhacs-eng needs container, vars
      manifest-stackrox-io needs container, vars
      test-containers-build calls test-containers-build.yml
      vars
    integration-tests.yml
      Required roots: integration-tests
      integration-tests
    test-containers-build.yml
      build-test-containers needs determine-qa-tag, should-build-test-containers
      determine-qa-tag needs should-build-test-containers
      should-build-test-containers

stackrox/scanner

  master at c4a592236e08ecfb7b98be29c70d698709e1585b
  release-2.39 at 959fa293a4f00ee701dd00b9ab327ce12788d0db
    ci.yaml
      Required roots: db-integration-tests, style-check, unit-tests
      db-integration-tests
      style-check
      unit-tests
*/
