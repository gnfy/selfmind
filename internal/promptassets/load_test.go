package promptassets

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMissingWorkspaceUsesDeterministicDefaults(t *testing.T) {
	root := filepath.Join(t.TempDir(), "prompts")
	loaded, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	empty := Empty(root)
	if loaded.Hash() != empty.Hash() {
		t.Fatalf("missing workspace hash = %s, Empty hash = %s", loaded.Hash(), empty.Hash())
	}
	if got := len(loaded.Files()); got != len(Catalog()) {
		t.Fatalf("files = %d, want %d", got, len(Catalog()))
	}
	if loaded.Value(FileAgent, SectionPersona).Mode != ModeDefault {
		t.Fatal("missing persona must inherit the configured agent soul")
	}
}

func TestLoadCustomAndOffSections(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "agent.md")
	content := `# SelfMind Agent

## Persona

回答时优先使用中文。

## Progress Updates

off
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Custom(FileAgent, SectionPersona); got != "回答时优先使用中文。" {
		t.Fatalf("persona = %q", got)
	}
	if !snapshot.Off(FileAgent, SectionProgressUpdates) {
		t.Fatal("progress section should be disabled")
	}
	if snapshot.Off(FileAgent, SectionWorkingStyle) {
		t.Fatal("omitted section must use its default")
	}
}

func TestMarkedSectionAllowsMarkdownLevelTwoHeadings(t *testing.T) {
	root := t.TempDir()
	content := `# Memory Extract

<!-- selfmind:section Post-run Analysis -->
## Post-run Analysis

Prefer concise decisions.

## Domain-specific checks

- Preserve identifiers.
<!-- selfmind:end -->
`
	path := filepath.Join(root, "background", "memory_extract.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	got := snapshot.Custom(FileMemoryExtract, SectionPostRunAnalysis)
	if !strings.Contains(got, "## Domain-specific checks") {
		t.Fatalf("custom Markdown heading was not preserved:\n%s", got)
	}
}

func TestLegacyBodyMentionDoesNotSelectMarkedGrammar(t *testing.T) {
	root := t.TempDir()
	content := `## Persona

Explain the literal <!-- selfmind:section Example --> token when asked.
`
	if err := os.WriteFile(filepath.Join(root, "agent.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Custom(FileAgent, SectionPersona); !strings.Contains(got, "selfmind:section Example") {
		t.Fatalf("legacy content=%q", got)
	}
}

func TestMarkedSectionAllowsMarkerExamplesInsideMarkdownFence(t *testing.T) {
	root := t.TempDir()
	content := `<!-- selfmind:section Persona -->
## Persona

Example:
` + "```markdown\n<!-- selfmind:section Example -->\n<!-- selfmind:end -->\n```" + `
<!-- selfmind:end -->
`
	if err := os.WriteFile(filepath.Join(root, "agent.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Custom(FileAgent, SectionPersona); !strings.Contains(got, "<!-- selfmind:end -->") {
		t.Fatalf("fenced marker example was not preserved:\n%s", got)
	}
}

func TestLoadRejectsMalformedSectionMarkerPrecisely(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "agent.md"), []byte("<!-- selfmind:section Persona-->\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(root)
	if err == nil || !strings.Contains(err.Error(), "malformed selfmind section marker at line 1") {
		t.Fatalf("error=%v", err)
	}
}

func TestMigrateLegacyContentPreservesBodyHeadings(t *testing.T) {
	legacy := `# SelfMind Agent

## Persona

Prefer concise Chinese.

## Examples

- Keep identifiers unchanged.

## Progress Updates

off
`
	migrated, changed, err := MigrateLegacyContent(FileAgent, legacy)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if !strings.Contains(migrated, "## Examples") || !strings.Contains(migrated, "<!-- selfmind:section Persona -->") {
		t.Fatalf("migrated content:\n%s", migrated)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "agent.md"), []byte(migrated), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(snapshot.Custom(FileAgent, SectionPersona), "## Examples") || !snapshot.Off(FileAgent, SectionProgressUpdates) {
		t.Fatalf("migration lost values: persona=%q progress=%+v", snapshot.Custom(FileAgent, SectionPersona), snapshot.Value(FileAgent, SectionProgressUpdates))
	}
}

func TestLoadRejectsUnknownAndLockedOffSections(t *testing.T) {
	tests := []struct {
		name, content, want string
	}{
		{name: "unknown", content: "## Mystery\n\nhello\n", want: "unknown section"},
		{name: "locked-off", content: "## Working Style\n\noff\n", want: "cannot be disabled"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "agent.md"), []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(root)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestLoadRejectsGroupWritablePrompt(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "agent.md")
	if err := os.WriteFile(path, []byte("## Persona\n\nhello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o660); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "group- or world-writable") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadRejectsUnsafePromptDirectories(t *testing.T) {
	t.Run("root symlink", func(t *testing.T) {
		parent := t.TempDir()
		target := filepath.Join(parent, "target")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		root := filepath.Join(parent, "prompts")
		if err := os.Symlink(target, root); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "invalid prompt root") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("writable root", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Chmod(root, 0o770); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "prompt root") || !strings.Contains(err.Error(), "group- or world-writable") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("nested symlink", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(t.TempDir(), "background")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(root, "background")); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestTemplateRoundTrips(t *testing.T) {
	for _, id := range IDs() {
		spec, _ := Spec(id)
		content, err := Template(id)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(content, "Custom text replaces only sections marked replaceable") {
			t.Fatalf("template %s does not explain edit policy", id)
		}
		root := t.TempDir()
		path := filepath.Join(root, filepath.FromSlash(spec.RelativePath))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(root); err != nil {
			t.Fatalf("template %s: %v", id, err)
		}
		loaded, err := Load(root)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Hash() != Empty(root).Hash() {
			t.Fatalf("all-default template %s changed the semantic snapshot hash", id)
		}
	}
}

func TestPromptRootFollowsConfigLocation(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "profile", "config.yaml")
	want := filepath.Join(filepath.Dir(configPath), "prompts")
	if got := PromptRoot(configPath, "/ignored/data"); got != want {
		t.Fatalf("root = %q, want %q", got, want)
	}
}

func TestPromptRevisionRoundTrip(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "agent.md"), []byte("## Persona\n\nCustom persona.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveRevision(snapshot); err != nil {
		t.Fatal(err)
	}
	restored, err := LoadRevision(root, snapshot.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if restored.Hash() != snapshot.Hash() || restored.Custom(FileAgent, SectionPersona) != "Custom persona." {
		t.Fatalf("restored snapshot = %#v", restored)
	}
}

func TestSaveRevisionRepairsCorruptCurrentCache(t *testing.T) {
	root := t.TempDir()
	snapshot, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveRevision(snapshot); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".revisions", snapshot.Hash()+".json")
	if err := os.WriteFile(path, []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveRevision(snapshot); err != nil {
		t.Fatalf("repair current revision: %v", err)
	}
	if _, err := LoadRevision(root, snapshot.Hash()); err != nil {
		t.Fatalf("repaired revision is invalid: %v", err)
	}
}

// TestLoadRevisionRejectsMismatchedHash pins the revision integrity check to a
// SINGLE accepted hash. A second accepted hash was carried for "snapshots
// written by the first prompt-workspace build", but no released binary ever
// wrote one — SaveRevision always names the file from hashSnapshot — so the
// only effect was widening what a hand-crafted revision file could pass.
func TestLoadRevisionRejectsMismatchedHash(t *testing.T) {
	root := t.TempDir()
	snapshot, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	files := make(map[string]FileState)
	for _, state := range snapshot.Files() {
		state.Path = ""
		files[state.ID] = state
	}
	const forged = "0000000000000000000000000000000000000000000000000000000000000000"
	data, err := json.Marshal(revisionFile{CatalogVersion: CatalogVersion, SnapshotHash: forged, Files: files})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ".revisions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, forged+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRevision(root, forged); err == nil {
		t.Fatal("a revision whose contents do not hash to its name must be rejected")
	}

	// The genuine hash still loads.
	if err := SaveRevision(snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRevision(root, snapshot.Hash()); err != nil {
		t.Fatalf("current revision must load: %v", err)
	}
}

func TestSnapshotHashUsesResolvedSectionsNotRawFormatting(t *testing.T) {
	rootA, rootB := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(rootA, "agent.md"), []byte("## Persona\n\nConcise.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootB, "agent.md"), []byte("# Agent\n\n<!-- note -->\n\n## Persona\n\nConcise.\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := Load(rootA)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Load(rootB)
	if err != nil {
		t.Fatal(err)
	}
	if a.Hash() != b.Hash() {
		t.Fatalf("semantic-equivalent workspaces differ: %s != %s", a.Hash(), b.Hash())
	}
	if a.SectionHash(FileAgent, SectionPersona) != b.SectionHash(FileAgent, SectionPersona) {
		t.Fatal("equivalent section values produced different hashes")
	}
}

func TestAgentPromptCachePlacementIsCatalogued(t *testing.T) {
	spec, _ := Spec(FileAgent)
	stable := map[string]bool{}
	for _, section := range spec.Sections {
		stable[section.Name] = section.Stable
	}
	if !stable[SectionProgressUpdates] || !stable[SectionFrontendUI] || stable[SectionLearningPreferences] {
		t.Fatalf("cache placement changed: progress=%v frontend=%v learning=%v", stable[SectionProgressUpdates], stable[SectionFrontendUI], stable[SectionLearningPreferences])
	}
}

func TestComposeUsesCatalogReplaceAndAppendPolicies(t *testing.T) {
	root := t.TempDir()
	content := `## Persona

Custom persona.

## Working Style

Prefer reversible edits.
`
	if err := os.WriteFile(filepath.Join(root, "agent.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	persona := snapshot.Compose(FileAgent, "Built-in persona.", SectionPersona)
	if persona != "Custom persona." {
		t.Fatalf("replace policy result=%q", persona)
	}
	quality := snapshot.Compose(FileAgent, "Locked quality floor.", SectionWorkingStyle)
	if !strings.Contains(quality, "Locked quality floor.") || !strings.Contains(quality, "Prefer reversible edits.") {
		t.Fatalf("append policy result=%q", quality)
	}
	emptyBase := snapshot.Compose(FileAgent, "", SectionWorkingStyle)
	if !strings.Contains(emptyBase, "Operator-configured guidance:") || strings.Contains(emptyBase, "policy above") {
		t.Fatalf("empty-base append must not refer to a missing locked contract: %q", emptyBase)
	}
}

// TestSaveRevisionFailsOnUnwritableRoot documents the trigger for the runner's
// degradation path: the revision cache lives under the prompt root, so a
// read-only or root-owned config directory makes persistence impossible. This
// must stay a recoverable warning at startup — the revision cache only pins
// prompts for durable background jobs, and a job that cannot load its pinned
// revision parks visibly instead of guessing.
func TestSaveRevisionFailsOnUnwritableRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "prompts")
	if err := os.MkdirAll(root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })

	snapshot, err := Load(root)
	if err != nil {
		t.Fatalf("an unwritable root with no prompt files must still load defaults: %v", err)
	}
	if err := SaveRevision(snapshot); err == nil {
		t.Skip("filesystem does not enforce directory permissions for this user")
	}
}
