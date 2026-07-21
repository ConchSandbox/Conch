package version

import "fmt"

var (
	Version = "unknown"
	Commit  = "unknown"
)

func String() string {
	commit := Commit
	if commit == "" {
		commit = "unknown"
	}
	return fmt.Sprintf("conch version %s, build %s-%s", Version, Version, shortCommit(commit))
}

func shortCommit(commit string) string {
	if len(commit) <= 8 {
		return commit
	}
	return commit[:8]
}
