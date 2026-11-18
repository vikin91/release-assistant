package release

import "strings"

// RC0Target identifies the first candidate for the configured ACS patch.
func (c Config) RC0Target() Target {
	if _, _, _, ok := Version(c.Version); !ok {
		return Target{}
	}
	return Target{Repo: c.ACS().Repo, Tag: strings.TrimPrefix(c.Version, "v") + "-rc.0"}
}
