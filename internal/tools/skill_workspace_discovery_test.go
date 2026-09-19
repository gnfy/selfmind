package tools

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// workspaceSkills lists what a run rooted at dir can see. The execution scope
// must still be installed when the caller inspects the result, so the listing
// happens here and the caller receives the resolved slice.
func workspaceSkills(t *testing.T, dir string) []SkillInfo {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SELFMIND_SKILLS_DIR", "")
	t.Setenv("SELFMIND_SKILLS_ROOTS", "")
	t.Chdir(t.TempDir())
	cleanup := SetExecutionScope("p", ExecutionScope{
		TenantID: "default", PersonID: "p", WorkspaceRoot: dir, WorkspaceID: "ws",
	})
	defer cleanup()
	skills, err := ListSkillsForTenant("default", false, map[string]interface{}{"_tenant_id": "p"})
	if err != nil {
		t.Fatalf("list skills: %v", err)
	}
	return skills
}

func skillNames(skills []SkillInfo) []string {
	names := make([]string, 0, len(skills))
	for _, skill := range skills {
		names = append(names, skill.Name)
	}
	sort.Strings(names)
	return names
}

// A workspace is commonly a container of repositories, each carrying its own
// Skill directory one level down. Discovery used to look only at the workspace
// root, so every one of those Skills was invisible and the agent re-derived
// their procedures by reading the files by hand.
//
// The same rule must still cover the single-repository workspace, which is the
// depth-0 case — a separate "layout mode" would be a scenario branch in generic
// discovery code.
func TestWorkspaceDiscoveryFindsRepositorySkillDirectories(t *testing.T) {
	ws := t.TempDir()
	writeSkillPackage(t, filepath.Join(ws, "repo-a", ".agents", "skills", "alpha"), "alpha", "Alpha procedure.")
	writeSkillPackage(t, filepath.Join(ws, "repo-b", ".claude", "skills", "beta"), "beta", "Beta procedure.")
	writeSkillPackage(t, filepath.Join(ws, "repo-c", "ai", "skills", "gamma"), "gamma", "Gamma procedure.")
	// Depth-0: the workspace is itself a repository.
	writeSkillPackage(t, filepath.Join(ws, ".agents", "skills", "root-level"), "root-level", "Root procedure.")
	// Dependency trees can carry vendored Skill directories that are not this
	// person's conventions and must never join the catalog.
	writeSkillPackage(t, filepath.Join(ws, "repo-a", "node_modules", "pkg", ".agents", "skills", "vendored"), "vendored", "Vendored.")

	names := skillNames(workspaceSkills(t, ws))
	found := map[string]bool{}
	for _, name := range names {
		found[name] = true
	}
	for _, want := range []string{"alpha", "beta", "gamma", "root-level"} {
		if !found[want] {
			t.Errorf("%q must be discoverable, got %v", want, names)
		}
	}
	if found["vendored"] {
		t.Errorf("a dependency tree must not contribute Skills, got %v", names)
	}
}

// The two cross-vendor conventions are commonly the same directory seen twice:
// a repository keeps one real directory and symlinks the other. Discovered
// under both roots and keyed lexically, every such Skill answers a bare name
// twice, and matchSkillsByName reports ambiguity rather than resolving it — so
// adding the second convention without resolving symlinks would make every
// Skill in that repository uncallable by its own name.
func TestSymlinkedSkillConventionYieldsOneSkill(t *testing.T) {
	ws := t.TempDir()
	real := filepath.Join(ws, "repo", ".agents", "skills", "release")
	writeSkillPackage(t, real, "release", "Release procedure.")
	linkDir := filepath.Join(ws, "repo", ".claude", "skills")
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", ".agents", "skills", "release"), filepath.Join(linkDir, "release")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	skills := workspaceSkills(t, ws)
	names := skillNames(skills)
	count := 0
	for _, name := range names {
		if name == "release" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("release must appear exactly once, got %d in %v", count, names)
	}
	// The constraint that must change the result: a bare name still resolves.
	if matches := matchSkillsByName(skills, "release"); len(matches) != 1 {
		t.Fatalf("bare name must resolve to one Skill, got %d", len(matches))
	}
}

// Two genuinely different Skills that happen to share a name are still
// ambiguous: resolving symlinks must not collapse distinct packages.
func TestDistinctSkillsSharingANameStayAmbiguous(t *testing.T) {
	ws := t.TempDir()
	writeSkillPackage(t, filepath.Join(ws, "repo-a", ".agents", "skills", "deploy"), "deploy", "Deploy A.")
	writeSkillPackage(t, filepath.Join(ws, "repo-b", ".agents", "skills", "deploy"), "deploy", "Deploy B.")

	skills := workspaceSkills(t, ws)
	if matches := matchSkillsByName(skills, "deploy"); len(matches) != 2 {
		t.Fatalf("two distinct packages must both match the bare name, got %d", len(matches))
	}
}

// A repository that withholds a Skill from the product runtime marks the
// canonical body. It commonly also publishes a thin compatibility entrypoint
// for another coding agent under the other convention, and that entrypoint
// carries no marker of its own — this repository does exactly that with
// selfmind-daily-driver-audit.
//
// Once discovery enumerates both conventions, a per-directory marker check
// exposes the entrypoint, and through it the instructions the marker withheld.
// The marker is a statement about a Skill NAME by the repository that owns it.
func TestDeveloperOnlyMarkerCoversTheCompatibilityEntrypoint(t *testing.T) {
	ws := t.TempDir()
	canonical := filepath.Join(ws, "repo", ".agents", "skills", "internal-audit")
	writeSkillPackage(t, canonical, "internal-audit", "Developer-only audit.")
	if err := os.WriteFile(filepath.Join(canonical, ".selfmind-developer-only"), []byte("developer only\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The thin entrypoint: a real directory, no marker of its own.
	writeSkillPackage(t, filepath.Join(ws, "repo", ".claude", "skills", "internal-audit"), "internal-audit", "Read the canonical body.")
	// A Skill the same repository does publish must stay visible.
	writeSkillPackage(t, filepath.Join(ws, "repo", ".claude", "skills", "release"), "release", "Release procedure.")

	names := skillNames(workspaceSkills(t, ws))
	for _, name := range names {
		if name == "internal-audit" {
			t.Fatalf("a developer-only Skill leaked through the compatibility entrypoint: %v", names)
		}
	}
	found := false
	for _, name := range names {
		if name == "release" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the marker must withhold one Skill, not the root: %v", names)
	}
}

// A directory package declares itself by containing SKILL.md. The bare Markdown
// form has no such declaration, so admitting any .md by filename put a README in
// the catalog and let a document shadow a real Skill: one repository keeps
// `ai/skills/README.md` describing the directory and `ai/skills/x.md` beside a
// real `.agents/skills/x/` package, and both entered the catalog under names
// that then resolved ambiguously.
func TestBareMarkdownIsASkillOnlyWhenItDeclaresItself(t *testing.T) {
	ws := t.TempDir()
	root := filepath.Join(ws, "repo", "skills")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# AI Skill Entry Points\n\nThis directory holds…\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.md"), []byte("# Notes\n\nA document kept beside the packages.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	declared := ensureFrontMatter("Do the thing.", "quick-check", "A declared bare-Markdown Skill.")
	if err := os.WriteFile(filepath.Join(root, "quick-check.md"), []byte(declared), 0o644); err != nil {
		t.Fatal(err)
	}

	names := skillNames(workspaceSkills(t, ws))
	for _, unwanted := range []string{"README", "notes"} {
		for _, name := range names {
			if name == unwanted {
				t.Errorf("%q is a document, not a Skill: %v", unwanted, names)
			}
		}
	}
	// The constraint that must change the result: the declared one stays.
	found := false
	for _, name := range names {
		if name == "quick-check" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a bare Markdown Skill that declares itself must still be discovered: %v", names)
	}
}
