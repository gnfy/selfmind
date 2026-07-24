package buildinfo

import (
	"fmt"
	"strings"
)

var (
	// Version, Commit, and BuiltAt are populated by release builds through
	// -ldflags. They deliberately remain readable defaults for local `go test`
	// and `go run`.
	Version = "v0.1.0-dev"
	Commit  = "dev"
	BuiltAt = ""
)

type Info struct {
	Version     string
	Commit      string
	BuiltAt     string
	Fingerprint string
}

func Current() Info {
	return Info{
		Version:     Version,
		Commit:      normalizedCommit(),
		BuiltAt:     strings.TrimSpace(BuiltAt),
		Fingerprint: Fingerprint(),
	}
}

func Fingerprint() string {
	commit := normalizedCommit()
	builtAt := strings.TrimSpace(BuiltAt)
	if commit == "dev" && builtAt == "" {
		return Version
	}
	if builtAt == "" {
		return Version + "+" + commit
	}
	return Version + "+" + commit + "@" + builtAt
}

func Display() string {
	commit := normalizedCommit()
	builtAt := strings.TrimSpace(BuiltAt)
	if commit == "dev" && builtAt == "" {
		return Version
	}
	if builtAt == "" {
		return fmt.Sprintf("%s (%s)", Version, commit)
	}
	return fmt.Sprintf("%s (%s, built %s)", Version, commit, builtAt)
}

func normalizedCommit() string {
	commit := strings.TrimSpace(Commit)
	if commit == "" {
		return "dev"
	}
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
