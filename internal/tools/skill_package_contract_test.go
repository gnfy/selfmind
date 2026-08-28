package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRawSkill(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	if err := atomicWriteFile(filepath.Join(dir, "SKILL.md"), content); err != nil {
		t.Fatalf("write %s: %v", dir, err)
	}
}

func packageIdentityOf(t *testing.T, skillDir string) (string, []string) {
	t.Helper()
	main, err := os.ReadFile(filepath.Join(skillDir, "SKILL.md"))
	if err != nil {
		t.Fatalf("read main: %v", err)
	}
	files := listSupportFiles(skillDir)
	resources := make(map[string]string, len(files))
	for _, file := range files {
		target, err := safeSupportPath(skillDir, file)
		if err != nil {
			t.Fatalf("resolve %s: %v", file, err)
		}
		data, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		resources[file] = string(data)
	}
	_, packageHash, _ := BuildSkillPackageIdentity(string(main), resources)
	return packageHash, files
}

// A vendored tree nested inside a support directory must not enter package
// identity: otherwise every upstream dependency change registers as package
// drift, and repeated drift fails closed. The exclusion must remove only that,
// leaving a package with legitimate resources hashing exactly as before.
func TestVendoredTreeInsideSupportDirectoryStaysOutOfPackageIdentity(t *testing.T) {
	root := t.TempDir()
	clean := filepath.Join(root, "clean-flow")
	vendored := filepath.Join(root, "vendored-flow")
	for _, dir := range []string{clean, vendored} {
		writeSkillPackage(t, dir, "same-flow", "identical main")
		if err := os.MkdirAll(filepath.Join(dir, "references"), 0o755); err != nil {
			t.Fatalf("create references: %v", err)
		}
		if err := atomicWriteFile(filepath.Join(dir, "references", "detail.md"), "# Detail\n"); err != nil {
			t.Fatalf("write resource: %v", err)
		}
	}
	nested := filepath.Join(vendored, "references", "node_modules", "left-pad")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("create vendored tree: %v", err)
	}
	if err := atomicWriteFile(filepath.Join(nested, "index.js"), "module.exports = 1\n"); err != nil {
		t.Fatalf("write vendored file: %v", err)
	}

	cleanHash, cleanFiles := packageIdentityOf(t, clean)
	vendoredHash, vendoredFiles := packageIdentityOf(t, vendored)
	if cleanHash != vendoredHash {
		t.Fatalf("vendored tree changed package identity: %s vs %s", cleanHash, vendoredHash)
	}
	if len(cleanFiles) != 1 || cleanFiles[0] != "references/detail.md" {
		t.Fatalf("legitimate resource lost: %v", cleanFiles)
	}
	if strings.Join(vendoredFiles, ",") != strings.Join(cleanFiles, ",") {
		t.Fatalf("support files diverged: %v vs %v", vendoredFiles, cleanFiles)
	}

	lockHash, lockFiles, err := hashSkillDirectory(vendored)
	if err != nil {
		t.Fatalf("hash skill directory: %v", err)
	}
	for _, file := range lockFiles {
		if strings.Contains(file, "node_modules") {
			t.Fatalf("install lock hashed a vendored file: %v", lockFiles)
		}
	}
	if lockHash == "" {
		t.Fatalf("empty lock hash")
	}
}

// An agent definition shipped inside an external package must stay inert. This
// is already true because agents/ is not an allowed support directory; the test
// pins it so widening that set cannot quietly let an untrusted asset declare
// execution authority.
func TestExternalAgentDefinitionStaysOutOfThePackage(t *testing.T) {
	root := t.TempDir()
	pkg := filepath.Join(root, "agent-carrying-flow")
	writeSkillPackage(t, pkg, "agent-carrying-flow", "package shipping an agent definition")
	if err := os.MkdirAll(filepath.Join(pkg, "agents"), 0o755); err != nil {
		t.Fatalf("create agents dir: %v", err)
	}
	if err := atomicWriteFile(filepath.Join(pkg, "agents", "openai.yaml"), "model: gpt-4\n"); err != nil {
		t.Fatalf("write agent definition: %v", err)
	}

	hash, files := packageIdentityOf(t, pkg)
	for _, file := range files {
		if strings.HasPrefix(file, "agents/") {
			t.Fatalf("agent definition entered the resource manifest: %v", files)
		}
	}
	bare := filepath.Join(root, "bare-flow")
	writeSkillPackage(t, bare, "agent-carrying-flow", "package shipping an agent definition")
	bareHash, _ := packageIdentityOf(t, bare)
	if hash != bareHash {
		t.Fatalf("agent definition changed package identity: %s vs %s", hash, bareHash)
	}

	if _, err := safeSupportPath(pkg, "agents/openai.yaml"); err == nil {
		t.Fatal("agents/ was accepted as a support path")
	}
}

// An author who marks a Skill user-only keeps it out of the model's discovery
// surface while both slash forms still resolve it.
func TestModelInvocationOptOutHidesFromCatalogButKeepsSlashForms(t *testing.T) {
	root := t.TempDir()
	writeRawSkill(t, filepath.Join(root, "user-only-flow"), strings.Join([]string{
		"---",
		"name: user-only-flow",
		"description: A relentless interview the model must not start on its own",
		"disable-model-invocation: true",
		"---",
		"",
		"# Procedure",
		"",
		"Interview the person.",
	}, "\n"))
	writeSkillPackage(t, filepath.Join(root, "open-flow"), "open-flow", "model may select this")

	isolatedSkillRoots(t, root)

	listed := listedSkillNames(t)
	if len(listed) != 2 {
		t.Fatalf("both Skills should remain discoverable: %v", listed)
	}

	catalog, err := CatalogSkillCandidatesForTenant("default", "interview")
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	for _, info := range catalog {
		if info.Name == "user-only-flow" {
			t.Fatalf("opted-out Skill reached the model catalog: %+v", info)
		}
	}
	ranked, err := RankSkillCandidatesForTenant("default", "relentless interview", 3)
	if err != nil {
		t.Fatalf("rank: %v", err)
	}
	for _, info := range ranked {
		if info.Name == "user-only-flow" {
			t.Fatalf("opted-out Skill reached candidate ranking: %+v", info)
		}
	}

	if _, _, ok, err := ResolveSkillInvocationForTenant("default", "/user-only-flow", "grill me"); err != nil || !ok {
		t.Fatalf("slash form lost: ok=%v err=%v", ok, err)
	}
	if _, err := findSkill("default", "user-only-flow"); err != nil {
		t.Fatalf("explicit lookup lost: %v", err)
	}
}

// The cross-vendor root is owned by this person but authored elsewhere, so it
// carries user scope and external provenance, and its qualified name says
// external rather than claiming to be a first-party user asset.
func TestCrossVendorRootCarriesExternalProvenance(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SELFMIND_SKILLS_DIR", "")
	t.Setenv("SELFMIND_SKILLS_ROOTS", "")
	t.Chdir(t.TempDir())

	writeSkillPackage(t, filepath.Join(home, ".agents", "skills", "agents-flow"), "agents-flow", "cross-vendor skill")

	skills, err := ListSkillsForTenant("default", false)
	if err != nil {
		t.Fatalf("list skills: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("unexpected inventory: %+v", skills)
	}
	got := skills[0]
	if got.Scope != SkillScopeUser {
		t.Fatalf("scope should describe ownership: %+v", got)
	}
	if got.Provenance != SkillProvenanceExternal {
		t.Fatalf("provenance should describe authorship: %+v", got)
	}
	if want := "external:agents-flow"; QualifiedSkillName(got) != want {
		t.Fatalf("qualified name = %q, want %q", QualifiedSkillName(got), want)
	}
}

// A key this runtime does not model stays ignored, but Doctor names the file and
// the key so an author-declared constraint cannot vanish silently.
func TestUnknownFrontMatterKeyIsReportedWithItsFile(t *testing.T) {
	root := t.TempDir()
	pkg := filepath.Join(root, "wider-vocabulary-flow")
	writeRawSkill(t, pkg, strings.Join([]string{
		"---",
		"name: wider-vocabulary-flow",
		"description: Authored for an agent with a wider front-matter vocabulary",
		"allowed-tools: Read",
		"license: MIT",
		"---",
		"",
		"# Procedure",
		"",
		"Proceed.",
	}, "\n"))
	writeSkillPackage(t, filepath.Join(root, "plain-flow"), "plain-flow", "nothing unusual")

	isolatedSkillRoots(t, root)

	issues, err := InspectSkillFrontMatterForTenant("default")
	if err != nil {
		t.Fatalf("inspect front matter: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("expected exactly the one asset with unmodelled keys: %+v", issues)
	}
	issue := issues[0]
	if issue.Name != "wider-vocabulary-flow" {
		t.Fatalf("wrong asset reported: %+v", issue)
	}
	if issue.Path != filepath.Join(pkg, "SKILL.md") {
		t.Fatalf("diagnostic must name the owning file, got %q", issue.Path)
	}
	if strings.Join(issue.Keys, ",") != "allowed-tools,license" {
		t.Fatalf("unexpected keys: %v", issue.Keys)
	}
	if issue.Provenance != SkillProvenanceExternal {
		t.Fatalf("provenance not carried: %+v", issue)
	}
}
