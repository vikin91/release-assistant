package github

// retryWorkflowDependency links an Actions job to the workflow that supplies its inputs.
type retryWorkflowDependency struct {
	Workflow       string
	Job            string
	RequiredChecks []string
	Needs          string
	Name           string
	Producers      []retryWorkflowProducer
}

// retryDependencies lists dependencies that GitHub Actions cannot express with needs.
var retryDependencies = map[string][]retryWorkflowDependency{
	"stackrox/stackrox": {
		{
			Workflow: stackroxE2EDispatchWorkflow,
			Job:      waitForActionsImagesJob,
			Needs:    stackroxBuildWorkflow,
			Name:     "Build",
			Producers: []retryWorkflowProducer{
				{Check: "build-and-push-db", Images: "central-db"},
				{Check: "build-operator-bundle", Images: "stackrox-operator-bundle"},
				{Check: "push-main-manifests (RHACS_BRANDING)", Images: "main, roxctl"},
				{Check: "push-matching-collector-scanner (RHACS_BRANDING)", Images: "collector, fact"},
				{Check: "push-scanner-manifests (default, quay.io/rhacs-eng)", Images: "scanner-v4, scanner-v4-db"},
				{Check: "push-operator-manifests (RHACS_BRANDING, *)", Images: "stackrox-operator"},
			},
			RequiredChecks: []string{
				waitForActionsImagesJob,
				"e2e-gke-upgrade-tests / gke-upgrade-tests-central",
				"e2e-gke-upgrade-tests / gke-upgrade-tests-postgres",
				"e2e-nongroovy-tests-gke / run / gke-nongroovy-e2e-tests",
			},
		},
	},
}

type (
	retryWorkflowProducer struct {
		Check  string
		Images string
	}
	retryWorkflowTree struct {
		Branches     []string
		Fingerprints []string
		Roots        []string
		Jobs         map[string][]string
		Calls        map[string]string
	}
)

// retryWorkflowTrees records job IDs and their direct prerequisites for audited
// workflow revisions. Fingerprints bind the edges to their source file, so
// changed workflows use their current YAML and current required check rules.
var retryWorkflowTrees = map[string]map[string][]retryWorkflowTree{
	"stackrox/collector": {
		".github/workflows/collector-builder.yml": {
			{
				Branches: []string{"master", "release-3.24", "release-3.25", "release-3.26"},
				Fingerprints: []string{
					"504d111b8aa052293fa21ef3bd521b361af10d48346a41ce9007e551ac4c2851",
					"5a0c8fdd6826c558f09cc6de728121ef9731a5e8096504f6bd3c7f746a37f3c8",
				},
				Jobs: map[string][]string{
					"build-builder-image":       {"builder-needs-rebuilding"},
					"builder-needs-rebuilding":  nil,
					"create-multiarch-manifest": {"build-builder-image", "builder-needs-rebuilding"},
				},
			},
		},
		".github/workflows/collector.yml": {
			{
				Branches: []string{"master", "release-3.24", "release-3.25", "release-3.26"},
				Fingerprints: []string{
					"0576c015c8fe9c5c4fbf93657ab5273fcf4ccfcfb7c662e2eacdc006756090a7",
					"7ca42f00a7ae05a47011d5de1c271117484725e432b4bcc6f02e4ba0ff9a1b01",
				},
				Roots: []string{"build-collector-image"},
				Jobs: map[string][]string{
					"build-collector-image": nil,
				},
			},
		},
		".github/workflows/init.yml": {
			{
				Branches: []string{"master", "release-3.24", "release-3.25", "release-3.26"},
				Fingerprints: []string{
					"59b36129f5206cf2160f2d2d6b618df0de36d7c999c4d7bcb6a72ce548753bdc",
				},
				Jobs: map[string][]string{
					"common-variables": nil,
				},
			},
		},
		".github/workflows/main.yml": {
			{
				Branches: []string{"master", "release-3.24", "release-3.25", "release-3.26"},
				Fingerprints: []string{
					"6a272e9e3ba13568cd43e2505fabe19782366943ad6db66c1716e059758abee3",
				},
				Roots: []string{"build-collector", "unit-tests"},
				Jobs: map[string][]string{
					"build-builder-image": {"init"},
					"build-collector":     {"build-builder-image", "init"},
					"init":                nil,
					"unit-tests":          {"build-builder-image"},
				},
				Calls: map[string]string{
					"build-builder-image": "./.github/workflows/collector-builder.yml",
					"build-collector":     "./.github/workflows/collector.yml",
					"init":                "./.github/workflows/init.yml",
					"unit-tests":          "./.github/workflows/unit-tests.yml",
				},
			},
		},
		".github/workflows/unit-tests.yml": {
			{
				Branches: []string{"master", "release-3.24", "release-3.25", "release-3.26"},
				Fingerprints: []string{
					"26a18a28e0c85acdd69a2cea1e5d38b0655d946e2c02a58364a2da6f29ae7393",
				},
				Roots: []string{"unit-tests"},
				Jobs: map[string][]string{
					"unit-tests": nil,
				},
			},
		},
	},
	"stackrox/fact": {
		".github/workflows/ci.yml": {
			{
				Branches: []string{"main", "release-0.4"},
				Fingerprints: []string{
					"f5bd8f52ab90c0678433f7c69048b202c685f172b39a2db7b2147c5a976d7e7d",
					"6a124a8ffac61b414d6a3b1a7354aad6c65184dc3153ce8474a8bae1e9b727f6",
				},
				Roots: []string{"basic", "integration-tests"},
				Jobs: map[string][]string{
					"basic":                 nil,
					"container-build":       nil,
					"integration-tests":     {"container-build", "test-containers-build"},
					"test-containers-build": nil,
				},
				Calls: map[string]string{
					"container-build":       "./.github/workflows/container-build.yml",
					"integration-tests":     "./.github/workflows/integration-tests.yml",
					"test-containers-build": "./.github/workflows/test-containers-build.yml",
				},
			},
			{
				Branches: []string{"release-0.2"},
				Fingerprints: []string{
					"e6d4eaf0ac1fdb294b4536dd447232454315e7bb5b55218ab8e6139c201e8784",
				},
				Roots: []string{"basic", "integration-tests"},
				Jobs: map[string][]string{
					"basic":                nil,
					"container":            {"vars"},
					"integration-tests":    {"manifest-rhacs-eng", "manifest-stackrox-io", "vars"},
					"manifest-rhacs-eng":   {"container", "vars"},
					"manifest-stackrox-io": {"container", "vars"},
					"vars":                 nil,
				},
				Calls: map[string]string{
					"integration-tests": "./.github/workflows/integration-tests.yml",
				},
			},
			{
				Branches: []string{"release-0.3"},
				Fingerprints: []string{
					"9ba9fc237c752b28c4fb99605656c662f99026752c187778c0c6553c5456b35e",
				},
				Roots: []string{"basic", "integration-tests"},
				Jobs: map[string][]string{
					"basic":                 nil,
					"container":             {"vars"},
					"integration-tests":     {"manifest-rhacs-eng", "manifest-stackrox-io", "test-containers-build", "vars"},
					"manifest-rhacs-eng":    {"container", "vars"},
					"manifest-stackrox-io":  {"container", "vars"},
					"test-containers-build": nil,
					"vars":                  nil,
				},
				Calls: map[string]string{
					"integration-tests":     "./.github/workflows/integration-tests.yml",
					"test-containers-build": "./.github/workflows/test-containers-build.yml",
				},
			},
		},
		".github/workflows/container-build.yml": {
			{
				Branches: []string{"main", "release-0.4"},
				Fingerprints: []string{
					"deffce7933b79706869a6beb8ea2d2e86f3c87c0a3e9461e03807e74fecd9d2b",
				},
				Jobs: map[string][]string{
					"container":            {"vars"},
					"manifest-rhacs-eng":   {"container", "vars"},
					"manifest-stackrox-io": {"container", "vars"},
					"vars":                 nil,
				},
			},
		},
		".github/workflows/integration-tests.yml": {
			{
				Branches: []string{"main", "release-0.2", "release-0.3", "release-0.4"},
				Fingerprints: []string{
					"655100ec0d29c9a7118962a306e3f1ce4935f58ad34c99bd513909ef546d88ef",
					"9a536bf2faacbcb8d94cd6caa2a9789a436544af41974f1c0aefee6c7e5978f1",
					"50b170f26ce67835d86ab223d28c1e4a0c04e3e1e8ec71581a0c25b379e3a78d",
					"a509e94307fe7ab7539d72b5a18bbe568adc62793ca8b5a1c9f22cee5f807324",
				},
				Roots: []string{"integration-tests"},
				Jobs: map[string][]string{
					"integration-tests": nil,
				},
			},
		},
		".github/workflows/test-containers-build.yml": {
			{
				Branches: []string{"main", "release-0.3", "release-0.4"},
				Fingerprints: []string{
					"aeb14041bc4377593ea1089628709f3498552b1c3f8281f982bd622f29be2424",
				},
				Jobs: map[string][]string{
					"build-test-containers":        {"determine-qa-tag", "should-build-test-containers"},
					"determine-qa-tag":             {"should-build-test-containers"},
					"should-build-test-containers": nil,
				},
			},
		},
	},
	"stackrox/scanner": {
		".github/workflows/ci.yaml": {
			{
				Branches: []string{"master", "release-2.39"},
				Fingerprints: []string{
					"1191dfee1a1c720632ead9d31a20082c650c5df2977eccb93f175a251a779491",
					"7c6f3d72f6b4defb3da2a6d82faf6107405a14c1e14797db6de398af53fbecce",
				},
				Roots: []string{"db-integration-tests", "style-check", "unit-tests"},
				Jobs: map[string][]string{
					"db-integration-tests": nil,
					"style-check":          nil,
					"unit-tests":           nil,
				},
			},
		},
	},
	"stackrox/stackrox": {
		".github/workflows/build.yaml": {
			{
				Branches: []string{"master"},
				Fingerprints: []string{
					"898f9bd16ecac57ca9bdef6644e443e92a94051ada771e1bb53790dc1ec4759d",
				},
				Roots: []string{"build-and-push-db", "build-and-push-main", "build-and-push-scanner", "build-operator-bundle", "push-main-manifests", "push-matching-collector-scanner", "push-operator-manifests", "push-scanner-manifests"},
				Jobs: map[string][]string{
					"build-and-push-db":               {"define-job-matrix"},
					"build-and-push-main":             {"define-job-matrix", "pre-build-cli", "pre-build-docs", "pre-build-go-binaries", "pre-build-oss-notice", "pre-build-ui"},
					"build-and-push-operator":         {"define-job-matrix", "pre-build-go-binaries", "pre-build-oss-notice"},
					"build-and-push-scanner":          {"build-and-push-db", "define-job-matrix", "pre-build-go-binaries", "pre-build-oss-notice"},
					"build-operator-bundle":           nil,
					"define-job-matrix":               nil,
					"pre-build-cli":                   {"define-job-matrix"},
					"pre-build-docs":                  nil,
					"pre-build-go-binaries":           {"define-job-matrix"},
					"pre-build-oss-notice":            nil,
					"pre-build-ui":                    nil,
					"push-main-manifests":             {"build-and-push-main", "define-job-matrix"},
					"push-matching-collector-scanner": {"define-job-matrix"},
					"push-operator-manifests":         {"build-and-push-operator", "define-job-matrix"},
					"push-scanner-manifests":          {"build-and-push-scanner", "define-job-matrix"},
				},
			},
			{
				Branches: []string{"release-4.10"},
				Fingerprints: []string{
					"2342bfb0fb1034d5a71e2f0f789901ca8e97ab55bbc3ff2a2645f70f9bcd21ba",
				},
				Roots: []string{"push-main-manifests", "push-operator-manifests"},
				Jobs: map[string][]string{
					"build-and-push-main":     {"define-job-matrix", "pre-build-cli", "pre-build-docs", "pre-build-go-binaries", "pre-build-ui"},
					"build-and-push-operator": {"define-job-matrix"},
					"define-job-matrix":       nil,
					"pre-build-cli":           nil,
					"pre-build-docs":          nil,
					"pre-build-go-binaries":   {"define-job-matrix"},
					"pre-build-ui":            nil,
					"push-main-manifests":     {"build-and-push-main", "define-job-matrix"},
					"push-operator-manifests": {"build-and-push-operator", "define-job-matrix"},
				},
			},
			{
				Branches: []string{"release-4.11"},
				Fingerprints: []string{
					"c2c40eb20e1a44b0442291173df370a90631e71adb3d36e8a7b4e3b85283b7de",
				},
				Roots: []string{"push-main-manifests", "push-matching-collector-scanner", "push-operator-manifests", "push-scanner-manifests"},
				Jobs: map[string][]string{
					"build-and-push-main":             {"define-job-matrix", "pre-build-cli", "pre-build-docs", "pre-build-go-binaries", "pre-build-oss-notice", "pre-build-ui"},
					"build-and-push-operator":         {"define-job-matrix", "pre-build-go-binaries"},
					"build-and-push-scanner":          {"define-job-matrix", "pre-build-go-binaries", "pre-build-oss-notice"},
					"define-job-matrix":               nil,
					"pre-build-cli":                   {"define-job-matrix"},
					"pre-build-docs":                  nil,
					"pre-build-go-binaries":           {"define-job-matrix"},
					"pre-build-oss-notice":            nil,
					"pre-build-ui":                    nil,
					"push-main-manifests":             {"build-and-push-main", "define-job-matrix"},
					"push-matching-collector-scanner": {"define-job-matrix"},
					"push-operator-manifests":         {"build-and-push-operator", "define-job-matrix"},
					"push-scanner-manifests":          {"build-and-push-scanner", "define-job-matrix"},
				},
			},
			{
				Branches: []string{"release-5.0"},
				Fingerprints: []string{
					"97707ef7f56883a70999b9c4932637b444327ebb5b1d7fd39d796321a8d4e657",
				},
				Roots: []string{"build-and-push-db", "build-operator-bundle", "push-main-manifests", "push-matching-collector-scanner", "push-operator-manifests", "push-scanner-manifests"},
				Jobs: map[string][]string{
					"build-and-push-db":               {"define-job-matrix"},
					"build-and-push-main":             {"define-job-matrix", "pre-build-cli", "pre-build-docs", "pre-build-go-binaries", "pre-build-oss-notice", "pre-build-ui"},
					"build-and-push-operator":         {"define-job-matrix", "pre-build-go-binaries", "pre-build-oss-notice"},
					"build-and-push-scanner":          {"build-and-push-db", "define-job-matrix", "pre-build-go-binaries", "pre-build-oss-notice"},
					"build-operator-bundle":           nil,
					"define-job-matrix":               nil,
					"pre-build-cli":                   {"define-job-matrix"},
					"pre-build-docs":                  nil,
					"pre-build-go-binaries":           {"define-job-matrix"},
					"pre-build-oss-notice":            nil,
					"pre-build-ui":                    nil,
					"push-main-manifests":             {"build-and-push-main", "define-job-matrix"},
					"push-matching-collector-scanner": {"define-job-matrix"},
					"push-operator-manifests":         {"build-and-push-operator", "define-job-matrix"},
					"push-scanner-manifests":          {"build-and-push-scanner", "define-job-matrix"},
				},
			},
		},
		".github/workflows/check-crd-compatibility.yaml": {
			{
				Branches: []string{"master"},
				Fingerprints: []string{
					"40421d0733ea2da7c5d9bec9ba4da72c0cc67f2e9aaaf59e4978fb603a30262d",
				},
				Roots: []string{"check-crd-compatibility"},
				Jobs: map[string][]string{
					"check-crd-compatibility": {"detect-changes"},
					"detect-changes":          nil,
				},
				Calls: map[string]string{
					"detect-changes": "./.github/workflows/detect-changes.yml",
				},
			},
		},
		".github/workflows/detect-changes.yml": {
			{
				Branches: []string{"master"},
				Fingerprints: []string{
					"076f6e1a8a170d987ae86388ccbf5d82f52ac3333a62dcb42dd8ffab39f99f8f",
				},
				Jobs: map[string][]string{
					"detect": nil,
				},
			},
		},
		".github/workflows/e2e-dispatch.yaml": {
			{
				Branches: []string{"master"},
				Fingerprints: []string{
					"f55c83f5b4593df2bb157f4054afea5e2bcc16b423032c7e6f28957e7a1f92af",
				},
				Roots: []string{"e2e-gke-upgrade-tests", "e2e-nongroovy-tests-gke", "wait-for-images"},
				Jobs: map[string][]string{
					"detect-changes":          nil,
					"e2e-gke-upgrade-tests":   {"should-dispatch", "wait-for-images"},
					"e2e-nongroovy-tests-gke": {"should-dispatch", "wait-for-images"},
					"should-dispatch":         {"detect-changes"},
					"wait-for-images":         {"should-dispatch"},
				},
				Calls: map[string]string{
					"detect-changes":          "./.github/workflows/detect-changes.yml",
					"e2e-gke-upgrade-tests":   "./.github/workflows/e2e-gke-upgrade-tests.yaml",
					"e2e-nongroovy-tests-gke": "./.github/workflows/e2e-nongroovy-tests-gke.yaml",
				},
			},
		},
		".github/workflows/e2e-gke-upgrade-tests.yaml": {
			{
				Branches: []string{"master"},
				Fingerprints: []string{
					"5f53d21ac9355bb8e1745c8f2252a678d368af39e0de96fad6678406956a14b2",
				},
				Roots: []string{"gke-upgrade-tests-central", "gke-upgrade-tests-postgres"},
				Jobs: map[string][]string{
					"check-postgres-upgrade":     nil,
					"gke-upgrade-tests-central":  nil,
					"gke-upgrade-tests-postgres": {"check-postgres-upgrade"},
				},
			},
		},
		".github/workflows/e2e-nongroovy-tests-gke.yaml": {
			{
				Branches: []string{"master"},
				Fingerprints: []string{
					"019a23b2d36b27db35c1cfd412eb8bee06b58a7d2a36976296ec04e66f2ea9d5",
				},
				Roots: []string{"run"},
				Jobs: map[string][]string{
					"run": nil,
				},
				Calls: map[string]string{
					"run": "./.github/workflows/e2e-nongroovy-tests.yaml",
				},
			},
		},
		".github/workflows/e2e-nongroovy-tests.yaml": {
			{
				Branches: []string{"master"},
				Fingerprints: []string{
					"9c53b36e34272c7c04d62c77a2bf1065d3c460e1b53873173fcd60fa439ef7c5",
				},
				Roots: []string{"gke-nongroovy-e2e-tests"},
				Jobs: map[string][]string{
					"gke-nongroovy-e2e-tests": nil,
				},
			},
		},
		".github/workflows/scanner-build.yaml": {
			{
				Branches: []string{"release-4.10"},
				Fingerprints: []string{
					"f6690734f4acddbcbcd518343a751c513e32ae53947ddb8714a3b6799d546e9c",
				},
				Roots: []string{"push-scanner-manifests"},
				Jobs: map[string][]string{
					"build-and-push-scanner":      {"define-scanner-job-matrix", "pre-build-scanner-go-binary"},
					"define-scanner-job-matrix":   nil,
					"pre-build-scanner-go-binary": {"define-scanner-job-matrix"},
					"push-scanner-manifests":      {"build-and-push-scanner", "define-scanner-job-matrix"},
				},
			},
		},
		".github/workflows/style.yaml": {
			{
				Branches: []string{"master"},
				Fingerprints: []string{
					"218a655e28951b5eb2320cd5bfef0a80b6afc0c2f06e282becca29cc3adf01ce",
				},
				Roots: []string{"check-dependent-images-exist", "check-generated-files", "golangci-lint", "misc-checks", "style-check"},
				Jobs: map[string][]string{
					"check-dependent-images-exist": {"detect-changes"},
					"check-generated-files":        {"detect-changes"},
					"detect-changes":               nil,
					"golangci-lint":                {"detect-changes"},
					"misc-checks":                  nil,
					"style-check":                  nil,
				},
				Calls: map[string]string{
					"detect-changes": "./.github/workflows/detect-changes.yml",
				},
			},
			{
				Branches: []string{"release-4.10"},
				Fingerprints: []string{
					"6863183b85e8c867cc455f4fdec056e8d21e221375e3936dd399958d6af98578",
				},
				Roots: []string{"check-dependent-images-exist"},
				Jobs: map[string][]string{
					"check-dependent-images-exist": nil,
				},
			},
		},
		".github/workflows/unit-tests.yaml": {
			{
				Branches: []string{"master"},
				Fingerprints: []string{
					"4aa718c6881a238ec05894038852a65f1ab8b576089c9ef2f5fc8ea40754e794",
				},
				Roots: []string{"go", "go-bench", "go-postgres", "local-roxctl-tests", "ui"},
				Jobs: map[string][]string{
					"detect-changes":     nil,
					"go":                 {"detect-changes"},
					"go-bench":           {"detect-changes"},
					"go-postgres":        {"detect-changes"},
					"local-roxctl-tests": {"detect-changes"},
					"ui":                 {"detect-changes"},
				},
				Calls: map[string]string{
					"detect-changes": "./.github/workflows/detect-changes.yml",
				},
			},
		},
	},
}
