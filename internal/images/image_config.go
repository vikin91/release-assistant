// Image naming configuration.
// Edit this file when upstream changes the expected images, repository names,
// or tag suffixes. Rebuild the assistant to apply the changes.
package images

const (
	ciFamily      = "ci"
	konfluxFamily = "konflux"
)

var (
	imageFamilies = [...]struct {
		family, repositoryPrefix, tagSuffix string
	}{
		{family: ciFamily, repositoryPrefix: "quay.io/rhacs-eng/"},
		{family: konfluxFamily, repositoryPrefix: "quay.io/rhacs-eng/release-", tagSuffix: "-fast"},
	}

	componentImageNames = map[string][]string{
		"fact":      {"fact"},
		"collector": {"collector"},
		"scanner":   {"scanner", "scanner-db", "scanner-slim", "scanner-db-slim"},
	}

	rc0ImageNames = map[int][]string{
		4: {"main", "roxctl", "central-db", "scanner-v4", "scanner-v4-db"},
		5: {"main", "roxctl", "scanner-v4"},
	}
)
