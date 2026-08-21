package cliapp

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// trackedInGit reports whether a repo-relative path is committed. The real
// failure this guards against is a Skill that exists on the author's disk but
// never entered the repository, which os.Stat alone cannot see. Skipped when
// the tree has no .git (source archive).
func trackedInGit(t *testing.T, root, relative string) (tracked bool, checked bool) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		return false, false
	}
	cmd := exec.Command("git", "ls-files", "--error-unmatch", "--", relative)
	cmd.Dir = root
	if err := cmd.Run(); err != nil {
		return false, true
	}
	return true, true
}

var developerSkillPathPattern = regexp.MustCompile(`\.agents/skills/[A-Za-z0-9._-]+/SKILL\.md`)

// TestAgentsMdDeveloperSkillPathsExist pins that the repository developer-skill
// table is a usable discovery fallback. AGENTS.md names these paths for agents
// that scan neither skills directory, so a listed path that exists only on one
// machine's disk sends every other agent and every fresh clone to nothing.
func TestAgentsMdDeveloperSkillPathsExist(t *testing.T) {
	root := commandDocsRepoRoot(t)
	content, err := os.ReadFile(filepath.Join(root, "AGENTS.md"))
	if err != nil {
		t.Fatalf("read AGENTS.md: %v", err)
	}
	paths := developerSkillPathPattern.FindAllString(string(content), -1)
	if len(paths) == 0 {
		t.Skip("AGENTS.md lists no developer skills")
	}
	seen := map[string]bool{}
	for _, rel := range paths {
		if seen[rel] {
			continue
		}
		seen[rel] = true
		absolute := filepath.Join(root, filepath.FromSlash(rel))
		info, statErr := os.Stat(absolute)
		if statErr != nil {
			t.Errorf("AGENTS.md lists %s but it is not in the repository: %v", rel, statErr)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("%s is empty", rel)
		}
		if tracked, checked := trackedInGit(t, root, rel); checked && !tracked {
			t.Errorf("AGENTS.md lists %s but it is not tracked in git; a fresh clone follows the table to nothing", rel)
		}
		body, readErr := os.ReadFile(absolute)
		if readErr != nil {
			t.Errorf("read %s: %v", rel, readErr)
			continue
		}
		if !strings.HasPrefix(string(body), "---") {
			t.Errorf("%s has no YAML front matter", rel)
		}
		// The runtime must not serve a development-only Skill as a product asset.
		markerRel := filepath.ToSlash(filepath.Join(filepath.Dir(rel), ".selfmind-developer-only"))
		if _, markerErr := os.Stat(filepath.Join(root, filepath.FromSlash(markerRel))); markerErr != nil {
			t.Errorf("%s has no .selfmind-developer-only marker: %v", rel, markerErr)
		} else if tracked, checked := trackedInGit(t, root, markerRel); checked && !tracked {
			t.Errorf("%s is not tracked in git, so the runtime exclusion marker would be missing from a clone", markerRel)
		}
	}
}

// TestDeveloperSkillCompatibilityEntrypointsRedirect pins that a Claude Code
// compatibility entry stays thin: AGENTS.md requires it to point at the
// canonical body rather than duplicate the workflow, so the two cannot drift.
func TestDeveloperSkillCompatibilityEntrypointsRedirect(t *testing.T) {
	root := commandDocsRepoRoot(t)
	compatRoot := filepath.Join(root, ".claude", "skills")
	entries, err := os.ReadDir(compatRoot)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skip("no Claude Code compatibility entries")
		}
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		compat := filepath.Join(compatRoot, entry.Name(), "SKILL.md")
		body, readErr := os.ReadFile(compat)
		if readErr != nil {
			continue
		}
		canonical := filepath.Join(root, ".agents", "skills", entry.Name(), "SKILL.md")
		if _, statErr := os.Stat(canonical); statErr != nil {
			t.Errorf(".claude/skills/%s has no canonical .agents counterpart: %v", entry.Name(), statErr)
			continue
		}
		text := string(body)
		if !strings.Contains(text, ".agents/skills/"+entry.Name()+"/SKILL.md") {
			t.Errorf(".claude/skills/%s/SKILL.md does not point at its canonical body", entry.Name())
		}
		canonicalBody, err := os.ReadFile(canonical)
		if err != nil {
			t.Fatal(err)
		}
		if len(body) >= len(canonicalBody) {
			t.Errorf(".claude/skills/%s/SKILL.md is %d bytes against a %d-byte canonical body; a compatibility entry must redirect, not duplicate",
				entry.Name(), len(body), len(canonicalBody))
		}
	}
}
