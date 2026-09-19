package cliapp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/doccheck"
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
	// A retired STATUS WORD, not a command. `/status` and `/watchers` labelled
	// the current work "Task:" until the domain object stopped being one. The
	// eval corpus asserted that label and only the full gate caught it, because
	// the case that pins cross-endpoint continuity is one of the slow ones the
	// fast profile skips.
	"Task: ",
	"Tasks: open",
}

// sweptDirs are the trees the Go compiler cannot check for us. Everything a
// retired name can hide in and still run: shell scripts, CI and release
// workflows, and eval case inputs.
var sweptDirs = []string{"scripts", ".github", "evalcases"}

// docsDir is swept by a different rule. A script either invokes a name or it
// does not; prose mentions one constantly in passing — "run/task", "the task
// queue", the `/v1/tasks/events` endpoint — and matching those would bury the
// one case that matters: documentation still telling a reader to TYPE a command
// that no longer exists.
//
// The rule is the backtick. In this repository a backticked span beginning with
// a command is an instruction to type it, so that is what a retired name may
// not appear as. Prose keeps the name unformatted, which is also how a sentence
// that reports the removal should read: a command you cannot type is not code.
const docsDir = "docs"

// docsExemptClasses are records of what was decided or planned at a past time.
// They describe a world where the name still worked, so they are read as
// history and not followed as instructions. The lifecycle comes from the
// manifest, which owns it, rather than from a path list that a moved file would
// silently escape.
var docsExemptClasses = map[string]bool{"plan": true, "decision": true, "archive": true}

func docsExemptFromRetiredNames(t *testing.T, root string) map[string]bool {
	t.Helper()
	manifest, err := doccheck.LoadManifest(root)
	if err != nil {
		t.Fatalf("load docs manifest: %v", err)
	}
	exempt := map[string]bool{}
	// An excluded document is outside the published contract and declares in
	// the manifest what it is; the ones here are private migration records.
	// They describe the world they were written in, like an archived plan.
	for _, doc := range manifest.ExcludedDocuments {
		exempt[filepath.FromSlash(doc.Path)] = true
	}
	for _, doc := range manifest.Documents {
		if docsExemptClasses[strings.ToLower(strings.TrimSpace(doc.Class))] ||
			strings.EqualFold(strings.TrimSpace(doc.State), "archived") {
			exempt[filepath.FromSlash(doc.Path)] = true
		}
	}
	return exempt
}

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

// Documentation is the other tree the compiler never reads, and the one a
// person actually follows. A product document describing a removed command as
// the way to do something is worse than a stale script: the script fails
// loudly, the document sends the reader to a dead end.
func TestRetiredNamesAreNotDocumentedAsCommands(t *testing.T) {
	root := commandDocsRepoRoot(t)
	base := filepath.Join(root, docsDir)
	exempt := docsExemptFromRetiredNames(t, root)
	err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if !strings.EqualFold(filepath.Ext(path), ".md") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if exempt[rel] {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		text := string(data)
		if strings.Contains(text, retiredProbeMarker) {
			return nil
		}
		for lineNo, line := range strings.Split(text, "\n") {
			for _, name := range retiredNames {
				// "`" + name: a backticked span that BEGINS with the retired
				// name. `/v1/tasks/events` begins with /v1 and is a live
				// endpoint, not an invocation of /tasks.
				if strings.Contains(line, "`"+strings.TrimSpace(name)) {
					t.Errorf("%s:%d documents the retired name %q as something to type:\n  %s",
						rel, lineNo+1, strings.TrimSpace(name), strings.TrimSpace(line))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", docsDir, err)
	}
}
