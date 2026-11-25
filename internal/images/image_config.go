// Image naming configuration.
// Edit this file when upstream changes the expected images, repository names,
// or tag suffixes. Rebuild the assistant to apply the changes.
package images

import "release-assistant/internal/release"

const (
	ciFamily                = "ci"
	konfluxFamily           = "konflux"
	ciRepositoryPrefix      = "quay.io/rhacs-eng/"
	konfluxRepositoryPrefix = "quay.io/rhacs-eng/release-"
)

var (
	imageFamilies = [...]struct {
		family, repositoryPrefix, tagSuffix string
	}{
		{family: ciFamily, repositoryPrefix: ciRepositoryPrefix},
		{family: konfluxFamily, repositoryPrefix: konfluxRepositoryPrefix, tagSuffix: "-fast"},
		{family: release.ImagePublic, repositoryPrefix: "quay.io/stackrox-io/"},
	}

	componentImageNames = map[string][]string{
		"fact":      {"fact"},
		"collector": {"collector"},
		"scanner":   {"scanner", "scanner-db", "scanner-slim", "scanner-db-slim"},
	}

	rcCIRepositoryPrefixes = [...]string{ciRepositoryPrefix, "quay.io/stackrox-io/"}

	// rcCICatalog lists images mirrored across the CI namespaces. The operator
	// bundle and index are published only in rhacs-eng.
	rcCICatalog = [...]struct {
		name, repositoryName, tagPrefix string
		rhacsOnly, acs4Only             bool
	}{
		{name: "main"},
		{name: "central-db"},
		{name: "roxctl"},
		{name: "scanner-v4"},
		{name: "scanner-v4-db"},
		{name: "operator", repositoryName: "stackrox-operator"},
		{name: "collector"},
		{name: "fact"},
		{name: "scanner", acs4Only: true},
		{name: "scanner-slim", acs4Only: true},
		{name: "scanner-db", acs4Only: true},
		{name: "scanner-db-slim", acs4Only: true},
		{name: "operator-bundle", repositoryName: "stackrox-operator-bundle", tagPrefix: "v", rhacsOnly: true},
		{name: "operator-index", repositoryName: "stackrox-operator-index", tagPrefix: "v", rhacsOnly: true},
	}

	// rcKonfluxCatalog uses the full release ref. The repository and job overrides
	// cover pipelines that do not follow release-<name> and <name>-on-push.
	rcKonfluxCatalog = [...]struct {
		name, repository, job, tagPrefix, tagSuffix string
		acs4Only                                    bool
	}{
		{name: "main"},
		{name: "central-db"},
		{name: "operator"},
		{name: "roxctl"},
		{name: "scanner-v4"},
		{name: "scanner-v4-db"},
		{name: "collector", job: "retag-collector"},
		{name: "fact", job: "retag-fact"},
		{name: "scanner", job: "retag-scanner", acs4Only: true},
		{name: "scanner-slim", job: "retag-scanner-slim", acs4Only: true},
		{name: "scanner-db", job: "retag-scanner-db", acs4Only: true},
		{name: "scanner-db-slim", job: "retag-scanner-db-slim", acs4Only: true},
		{name: "operator-bundle", tagPrefix: "v"},
		{name: "operator-index", repository: "quay.io/rhacs-eng/stackrox-operator-index", tagPrefix: "v", tagSuffix: "-fast"},
	}
)
