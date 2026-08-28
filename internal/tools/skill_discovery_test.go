package tools

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// isolatedSkillRoots points discovery at one external root with no workspace,
// user, or environment root competing.
func isolatedSkillRoots(t *testing.T, external string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SELFMIND_SKILLS_DIR", "")
	t.Setenv("SELFMIND_SKILLS_ROOTS", external)
	t.Chdir(t.TempDir())
}

func writeSkillPackage(t *testing.T, dir, name, description string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	if err := atomicWriteFile(filepath.Join(dir, "SKILL.md"), ensureFrontMatter("Body of "+name+".", name, description)); err != nil {
		t.Fatalf("write %s: %v", dir, err)
	}
}

func listedSkillNames(t *testing.T) []string {
	t.Helper()
	skills, err := ListSkillsForTenant("default", false)
	if err != nil {
		t.Fatalf("list skills: %v", err)
	}
	names := make([]string, 0, len(skills))
	for _, skill := range skills {
		names = append(names, skill.Name)
	}
	sort.Strings(names)
	return names
}

// A read-only root governed by a package manifest yields exactly the packages
// its author published, so an unfinished draft that exists on disk stays out of
// the catalog.
func TestExternalRootHonoursPackageManifest(t *testing.T) {
	pkg := t.TempDir()
	writeSkillPackage(t, filepath.Join(pkg, "skills", "productivity", "published-flow"), "published-flow", "published skill")
	writeSkillPackage(t, filepath.Join(pkg, "skills", "in-progress", "draft-flow"), "draft-flow", "unfinished draft")
	manifestDir := filepath.Join(pkg, ".claude-plugin")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatalf("create manifest dir: %v", err)
	}
	manifest := `{"name":"vendor-pack","skills":["./skills/productivity/published-flow"]}`
	if err := atomicWriteFile(filepath.Join(manifestDir, "plugin.json"), manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	isolatedSkillRoots(t, filepath.Join(pkg, "skills"))

	skills, err := ListSkillsForTenant("default", false)
	if err != nil {
		t.Fatalf("list skills: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected only the manifest-registered package, got %+v", skills)
	}
	got := skills[0]
	if got.Name != "published-flow" {
		t.Fatalf("unexpected package: %+v", got)
	}
	if got.PackageName != "vendor-pack" {
		t.Fatalf("manifest source name not carried: %+v", got)
	}
	if got.Scope != SkillScopeExternal || got.Writable {
		t.Fatalf("unexpected external metadata: %+v", got)
	}
	if want := "vendor-pack:published-flow"; QualifiedSkillName(got) != want {
		t.Fatalf("qualified name = %q, want %q", QualifiedSkillName(got), want)
	}
}

// A two-level layout is discovered without pointing the root at each category,
// which single-level enumeration could not do.
func TestRecursiveDiscoveryFindsNestedCategoryLayout(t *testing.T) {
	root := t.TempDir()
	writeSkillPackage(t, filepath.Join(root, "productivity", "grilling"), "grilling", "productivity skill")
	writeSkillPackage(t, filepath.Join(root, "engineering", "tdd"), "tdd", "engineering skill")

	isolatedSkillRoots(t, root)

	got := listedSkillNames(t)
	if len(got) != 2 || got[0] != "grilling" || got[1] != "tdd" {
		t.Fatalf("nested categories not discovered: %v", got)
	}
}

// A SKILL.md preserved inside a package support directory is documentation
// data, not a second Skill.
func TestSkillPackageResourceIsNotASecondSkill(t *testing.T) {
	root := t.TempDir()
	pkg := filepath.Join(root, "outer-flow")
	writeSkillPackage(t, pkg, "outer-flow", "the real skill")
	writeSkillPackage(t, filepath.Join(pkg, "references", "old-package"), "old-package", "preserved package")

	isolatedSkillRoots(t, root)

	got := listedSkillNames(t)
	if len(got) != 1 || got[0] != "outer-flow" {
		t.Fatalf("support-directory package leaked into the catalog: %v", got)
	}
}

// A directory that merely shares a support-directory name is still a category
// when it holds no SKILL.md of its own.
func TestSupportDirectoryNameStaysUsableAsCategory(t *testing.T) {
	root := t.TempDir()
	writeSkillPackage(t, filepath.Join(root, "scripts", "release-flow"), "release-flow", "categorised skill")

	isolatedSkillRoots(t, root)

	got := listedSkillNames(t)
	if len(got) != 1 || got[0] != "release-flow" {
		t.Fatalf("category named like a support directory was skipped: %v", got)
	}
}

func TestRecursiveDiscoveryStopsAtDepthBound(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "one", "two", "three", "at-bound")
	writeSkillPackage(t, deep, "at-bound", "deepest discoverable skill")
	writeSkillPackage(t, filepath.Join(root, "one", "two", "three", "four", "past-bound"), "past-bound", "beyond the bound")

	isolatedSkillRoots(t, root)

	got := listedSkillNames(t)
	if len(got) != 1 || got[0] != "at-bound" {
		t.Fatalf("depth bound not honoured: %v", got)
	}
}

func TestVendoredDependencyTreeIsNotScanned(t *testing.T) {
	root := t.TempDir()
	writeSkillPackage(t, filepath.Join(root, "own-flow"), "own-flow", "this root's skill")
	writeSkillPackage(t, filepath.Join(root, "node_modules", "someone-else", "vendored"), "vendored", "another package's skill")

	isolatedSkillRoots(t, root)

	got := listedSkillNames(t)
	if len(got) != 1 || got[0] != "own-flow" {
		t.Fatalf("vendored tree was scanned: %v", got)
	}
}

// A collision is surfaced rather than silently resolved: both entries stay
// visible and a bare name refuses with its qualified candidates.
func TestAmbiguousSkillNameRefusesWithQualifiedCandidates(t *testing.T) {
	pkg := t.TempDir()
	writeSkillPackage(t, filepath.Join(pkg, "shared-flow"), "shared-flow", "external routing description")

	isolatedSkillRoots(t, pkg)

	userDir, err := userSkillsDirForTenant("default")
	if err != nil {
		t.Fatalf("resolve user root: %v", err)
	}
	writeSkillPackage(t, filepath.Join(userDir, "shared-flow"), "shared-flow", "user routing description")

	skills, err := ListSkillsForTenant("default", false)
	if err != nil {
		t.Fatalf("list skills: %v", err)
	}
	if len(skills) != 2 {
		t.Fatalf("collision was dropped instead of listed: %+v", skills)
	}

	_, err = findSkill("default", "shared-flow")
	var ambiguous *skillAmbiguousError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("bare ambiguous name resolved: err=%T %v", err, err)
	}
	joined := strings.Join(ambiguous.Candidates, ",")
	if !strings.Contains(joined, "external:shared-flow") || !strings.Contains(joined, "user:shared-flow") {
		t.Fatalf("qualified candidates not reported: %v", ambiguous.Candidates)
	}

	resolved, err := findSkill("default", "user:shared-flow")
	if err != nil {
		t.Fatalf("qualified lookup failed: %v", err)
	}
	if resolved.Scope != SkillScopeUser {
		t.Fatalf("qualified lookup selected the wrong root: %+v", resolved)
	}

	// An explicitly configured environment root outbids the default user root,
	// so the precedence winner is the external one and model surfaces still see
	// a single entry.
	winner, err := findSkillByPrecedence("default", "shared-flow")
	if err != nil {
		t.Fatalf("precedence lookup failed: %v", err)
	}
	if winner.Scope != SkillScopeExternal {
		t.Fatalf("precedence winner = %+v", winner)
	}
	if primary := primarySkillsByName(skills); len(primary) != 1 {
		t.Fatalf("model surface kept a duplicate: %+v", primary)
	}
}

// The cross-vendor Agent Skills root is visible, and the writable user root
// keeps precedence so an explicit install is never shadowed by it.
func TestUserAgentsRootIsDiscoveredBelowUserRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SELFMIND_SKILLS_DIR", "")
	t.Setenv("SELFMIND_SKILLS_ROOTS", "")
	t.Chdir(t.TempDir())

	writeSkillPackage(t, filepath.Join(home, ".agents", "skills", "agents-flow"), "agents-flow", "cross-vendor skill")

	roots, err := SkillRootsForTenant("default")
	if err != nil {
		t.Fatalf("resolve roots: %v", err)
	}
	agentsRoot := filepath.Join(home, ".agents", "skills")
	var found *SkillRoot
	for i := range roots {
		if roots[i].Path == agentsRoot {
			found = &roots[i]
		}
	}
	if found == nil {
		t.Fatalf("cross-vendor root missing from %+v", roots)
	}
	if found.Writable {
		t.Fatalf("cross-vendor root must stay read-only: %+v", *found)
	}
	userDir, err := userSkillsDirForTenant("default")
	if err != nil {
		t.Fatalf("resolve user root: %v", err)
	}
	for _, root := range roots {
		if root.Path == userDir && root.Priority >= found.Priority {
			t.Fatalf("writable user root lost precedence: user=%d agents=%d", root.Priority, found.Priority)
		}
	}

	if got := listedSkillNames(t); len(got) != 1 || got[0] != "agents-flow" {
		t.Fatalf("cross-vendor skill not listed: %v", got)
	}
}

// Two roots can share both scope and source, so the qualified form alone cannot
// separate them. The refusal then names each candidate by path, which is what
// the person can type back.
func TestAmbiguityFallsBackToPathWhenQualifiersCollide(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SELFMIND_SKILLS_DIR", "")
	t.Setenv("SELFMIND_SKILLS_ROOTS", "")
	workspace := t.TempDir()
	t.Chdir(workspace)

	writable := filepath.Join(workspace, ".selfmind", "skills", "shared-flow")
	readOnly := filepath.Join(workspace, "skills", "shared-flow")
	writeSkillPackage(t, writable, "shared-flow", "writable workspace skill")
	writeSkillPackage(t, readOnly, "shared-flow", "read-only workspace skill")

	_, err := findSkill("default", "shared-flow")
	var ambiguous *skillAmbiguousError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("collision resolved silently: err=%T %v", err, err)
	}
	if len(ambiguous.Candidates) != 2 {
		t.Fatalf("unexpected candidates: %v", ambiguous.Candidates)
	}
	for _, candidate := range ambiguous.Candidates {
		if !strings.Contains(candidate, "shared-flow") || !strings.Contains(candidate, "(") {
			t.Fatalf("candidate is not typeable back: %q", candidate)
		}
	}

	resolved, err := findSkill("default", readOnly)
	if err != nil {
		t.Fatalf("path lookup failed: %v", err)
	}
	if resolved.Writable {
		t.Fatalf("path lookup selected the wrong root: %+v", resolved)
	}
}

// The listing renders a colliding short name in its qualified form, so a
// collision is discoverable rather than looking like one duplicated entry.
func TestSkillsListLabelsCollisionsWithQualifiedNames(t *testing.T) {
	pkg := t.TempDir()
	writeSkillPackage(t, filepath.Join(pkg, "shared-flow"), "shared-flow", "external copy")
	writeSkillPackage(t, filepath.Join(pkg, "unique-flow"), "unique-flow", "no collision")

	isolatedSkillRoots(t, pkg)

	userDir, err := userSkillsDirForTenant("default")
	if err != nil {
		t.Fatalf("resolve user root: %v", err)
	}
	writeSkillPackage(t, filepath.Join(userDir, "shared-flow"), "shared-flow", "user copy")

	skills, err := ListSkillsForTenant("default", false)
	if err != nil {
		t.Fatalf("list skills: %v", err)
	}
	rendered := formatSkillsList(skills)
	if !strings.Contains(rendered, "external:shared-flow") || !strings.Contains(rendered, "user:shared-flow") {
		t.Fatalf("collision not qualified in listing:\n%s", rendered)
	}
	if !strings.Contains(rendered, "- unique-flow [") {
		t.Fatalf("uncontested name should stay bare:\n%s", rendered)
	}
}

// The /<skill-name> form obeys the same rule and accepts the qualified name the
// refusal offered.
func TestSlashInvocationRefusesAmbiguityAndAcceptsQualifiedName(t *testing.T) {
	pkg := t.TempDir()
	writeSkillPackage(t, filepath.Join(pkg, "shared-flow"), "shared-flow", "external copy")

	isolatedSkillRoots(t, pkg)

	userDir, err := userSkillsDirForTenant("default")
	if err != nil {
		t.Fatalf("resolve user root: %v", err)
	}
	writeSkillPackage(t, filepath.Join(userDir, "shared-flow"), "shared-flow", "user copy")

	_, _, _, err = ResolveSkillInvocationForTenant("default", "/shared-flow", "inspect it")
	var ambiguous *skillAmbiguousError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("slash invocation resolved an ambiguous name: err=%T %v", err, err)
	}
	if !strings.Contains(err.Error(), "user:shared-flow") {
		t.Fatalf("refusal did not offer the qualified form: %v", err)
	}

	prompt, display, ok, err := ResolveSkillInvocationForTenant("default", "/user:shared-flow", "inspect it")
	if err != nil || !ok {
		t.Fatalf("qualified slash invocation failed: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(prompt, "inspect it") {
		t.Fatalf("instruction lost: %s", prompt)
	}
	if display == "" {
		t.Fatalf("empty display label")
	}
}

// The `$` completion writes a discovery path when a qualified name cannot
// separate two roots. The slash form keeps its own leading slash, so an absolute
// path arrives intact and resolves to exactly one package.
func TestSlashInvocationAcceptsAPathReference(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SELFMIND_SKILLS_DIR", "")
	t.Setenv("SELFMIND_SKILLS_ROOTS", "")
	workspace := t.TempDir()
	t.Chdir(workspace)

	writable := filepath.Join(workspace, ".selfmind", "skills", "shared-flow")
	readOnly := filepath.Join(workspace, "skills", "shared-flow")
	writeSkillPackage(t, writable, "shared-flow", "writable copy")
	writeSkillPackage(t, readOnly, "shared-flow", "read-only copy")

	if _, _, _, err := ResolveSkillInvocationForTenant("default", "/shared-flow", "inspect it"); err == nil {
		t.Fatal("the colliding bare name should still be refused")
	}

	// Each path selects its own package, which is the whole reason completion
	// falls back to a path when the qualified name cannot separate them.
	fromReadOnly, _, _, _, ok, err := ResolveTypedSkillInvocationForTenant("default", "/"+readOnly, "inspect it")
	if err != nil || !ok {
		t.Fatalf("read-only path did not resolve: ok=%v err=%v", ok, err)
	}
	fromWritable, prompt, display, _, ok, err := ResolveTypedSkillInvocationForTenant("default", "/"+writable, "inspect it")
	if err != nil || !ok {
		t.Fatalf("writable path did not resolve: ok=%v err=%v", ok, err)
	}
	if fromReadOnly.SkillKey == "" || fromWritable.SkillKey == "" {
		t.Fatalf("missing resolved identity: %+v %+v", fromReadOnly, fromWritable)
	}
	if fromReadOnly.SkillKey == fromWritable.SkillKey {
		t.Fatalf("both paths resolved to the same package: %s", fromReadOnly.SkillKey)
	}
	if !strings.Contains(prompt, "inspect it") {
		t.Fatalf("instruction lost: %s", prompt)
	}
	if display == "" {
		t.Fatal("empty display label")
	}
}

// Completion keeps every discovered Skill, including one its author kept out of
// the model catalog, and writes a reference that resolves to exactly one
// package.
func TestSkillCompletionCandidatesCoverTheWholeInventory(t *testing.T) {
	root := t.TempDir()
	writeSkillPackage(t, filepath.Join(root, "alpha-flow"), "alpha-flow", "first")
	writeSkillPackage(t, filepath.Join(root, "beta-flow"), "beta-flow", "second")

	isolatedSkillRoots(t, root)

	skills, err := ListSkillsForTenant("default", false)
	if err != nil {
		t.Fatal(err)
	}
	candidates := BuildSkillCompletionCandidates(skills)
	if len(candidates) != 2 {
		t.Fatalf("unexpected candidates: %+v", candidates)
	}
	for _, candidate := range candidates {
		if candidate.Label != candidate.Name {
			t.Fatalf("an uncontested name should render bare: %+v", candidate)
		}
		if candidate.Reference != candidate.Qualified {
			t.Fatalf("a unique qualified name should be the reference: %+v", candidate)
		}
		if strings.ContainsAny(candidate.Reference, " \t") {
			t.Fatalf("reference must survive whitespace tokenization: %q", candidate.Reference)
		}
	}
}

// When two roots share scope and source the qualified name cannot separate them,
// so the discovery path carries the choice and the row renders qualified.
func TestSkillCompletionCandidatesFallBackToPathOnCollision(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SELFMIND_SKILLS_DIR", "")
	t.Setenv("SELFMIND_SKILLS_ROOTS", "")
	workspace := t.TempDir()
	t.Chdir(workspace)

	writable := filepath.Join(workspace, ".selfmind", "skills", "shared-flow")
	readOnly := filepath.Join(workspace, "skills", "shared-flow")
	writeSkillPackage(t, writable, "shared-flow", "writable copy")
	writeSkillPackage(t, readOnly, "shared-flow", "read-only copy")

	skills, err := ListSkillsForTenant("default", false)
	if err != nil {
		t.Fatal(err)
	}
	references := map[string]bool{}
	for _, candidate := range BuildSkillCompletionCandidates(skills) {
		if candidate.Label != "workspace:shared-flow" {
			t.Fatalf("colliding row should render qualified: %+v", candidate)
		}
		references[candidate.Reference] = true
	}
	if !references[writable] || !references[readOnly] {
		t.Fatalf("references did not carry the distinguishing paths: %v", references)
	}
}

// Completion matching reuses the metadata ranker, so it inherits CJK bigram
// behaviour a prefix test cannot provide.
func TestSkillCompletionRankingMatchesCJK(t *testing.T) {
	candidates := []SkillCompletionCandidate{
		{Name: "release-flow", Qualified: "user:release-flow", Label: "release-flow", Description: "发布流程检查"},
		{Name: "grilling", Qualified: "user:grilling", Label: "grilling", Description: "stress-test a plan"},
	}
	ranked := RankSkillCompletionCandidates(candidates, "发布流程")
	if len(ranked) != 1 || ranked[0].Name != "release-flow" {
		t.Fatalf("CJK query ranked = %+v", ranked)
	}
	if empty := RankSkillCompletionCandidates(nil, "anything"); len(empty) != 0 {
		t.Fatalf("empty inventory ranked = %+v", empty)
	}
}
