package cliapp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// retiredNames are user-facing names this repository has removed. Each entry
// stays here permanently: the point is not to remember that it once existed but
// to keep it from being invoked again from a file the compiler never reads.
//
// Three times in one change set a retired name kept a live caller — `selfmind ws
// 2` forwarded to a deleted slash command, and a release smoke script invoked
// `selfmind tasks`, which only CI caught. Prose already said to remove obsolete
// artifacts; a guard is what actually removes them, because it does not depend
// on anyone remembering to sweep.
var retiredNames = []string{
	"selfmind tasks",
	"selfmind task ",
	"selfmind workspace",
	"selfmind workspaces",
	"/tasks",
	"/task ",
	"/workspace ",
	"/workspaces",
	"/diag tasks",
	"ws use",
	"ws list",
}

// sweptDirs are the trees the Go compiler cannot check for us. Everything a
// retired name can hide in and still run: shell scripts, CI and release
// workflows, and eval case inputs.
var sweptDirs = []string{"scripts", ".github", "evalcases"}

// retiredProbeMarker exempts a file that invokes a retired name ON PURPOSE, to
// prove it is rejected rather than silently doing something else.
const retiredProbeMarker = "retired-name-probe: intentional"

func TestRetiredNamesHaveNoInvocationSites(t *testing.T) {
	root := commandDocsRepoRoot(t)
	for _, dir := range sweptDirs {
		base := filepath.Join(root, dir)
		err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			switch strings.ToLower(filepath.Ext(path)) {
			case ".sh", ".yml", ".yaml", ".bash":
			default:
				return nil
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			rel, _ := filepath.Rel(root, path)
			text := string(data)
			// A file that deliberately exercises a retired name — asserting it now
			// answers "Unknown command" — declares that here. An exemption has to
			// be stated in the file, never inferred from its path.
			if strings.Contains(text, retiredProbeMarker) {
				return nil
			}
			for lineNo, line := range strings.Split(text, "\n") {
				// A comment may legitimately explain that a name was retired.
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
					continue
				}
				for _, name := range retiredNames {
					if strings.Contains(line, name) {
						t.Errorf("%s:%d invokes the retired name %q:\n  %s",
							rel, lineNo+1, name, trimmed)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
}
