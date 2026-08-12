package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"selfmind/internal/kernel"
)

func TestReadOnlySkillOperationsDoNotCreateTenantDirectories(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	registry := NewRegistry()

	for i := 0; i < 100; i++ {
		tenantID := fmt.Sprintf("person-readonly-%03d", i)
		if _, err := ListSkillsForTenant(tenantID, false); err != nil {
			t.Fatalf("list %s: %v", tenantID, err)
		}
		if _, err := ReloadSkillToolsForTenant(tenantID, registry); err != nil {
			t.Fatalf("reload %s: %v", tenantID, err)
		}
	}

	entries, err := os.ReadDir(filepath.Join(home, ".selfmind"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read selfmind root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("read-only skill operations created %d tenant directories", len(entries))
	}
}

func TestAgentSkillStorageUsesControlTenantWithoutChangingExecutionScope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	personID := "person-skill-owner"
	runID := "run-skill-owner"
	cleanup := SetExecutionScope(personID, ExecutionScope{
		TenantID: "control", PersonID: personID, RunID: runID, WorkspaceRoot: t.TempDir(),
	})
	defer cleanup()
	ctx := WithExecutionScopeKey(context.Background(), ExecutionScopeKeyForRun(runID))
	args := map[string]interface{}{
		"action":     "create",
		"name":       "tenant-visible",
		"content":    "Reusable tenant workflow.",
		"_tenant_id": personID,
		"_context":   ctx,
		"_invocation_scope": kernel.ToolInvocationScope{
			ControlTenantID: "control", PersonID: personID, RunID: runID,
		},
	}

	if _, err := NewSkillManageTool().Execute(args); err != nil {
		t.Fatalf("create skill: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".selfmind", "control", "skills", "tenant-visible", "SKILL.md")); err != nil {
		t.Fatalf("control-tenant skill missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".selfmind", personID, "skills")); !os.IsNotExist(err) {
		t.Fatalf("person skill directory should not be created, err=%v", err)
	}
	if scope, ok := currentExecutionScopeAny(args); !ok || scope.PersonID != personID || scope.RunID != runID {
		t.Fatalf("execution scope changed while routing storage: ok=%v scope=%+v", ok, scope)
	}
}

func TestSkillRuntimeListViewReloadAndSlashInvocation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	registry := NewRegistry()
	disp := NewDispatcherWithRegistry(registry)
	disp.RegisterTool(NewSkillManageTool())

	_, err := disp.Dispatch("skill_manage", map[string]interface{}{
		"action":      "create",
		"name":        "dev-flow",
		"description": "Developer workflow",
		"content":     "Inspect files, make a focused change, then run tests.",
		"_tenant_id":  "default",
	})
	if err != nil {
		t.Fatalf("create skill: %v", err)
	}
	if _, ok := registry.Get("skill:dev-flow"); !ok {
		t.Fatal("created skill should be registered without restart")
	}

	list, err := SkillsListJSONForTenant("default", "", false)
	if err != nil {
		t.Fatalf("skills list: %v", err)
	}
	if !strings.Contains(list, "dev-flow") || strings.Contains(list, "Inspect files") {
		t.Fatalf("list should contain metadata only, got:\n%s", list)
	}

	view, err := SkillViewJSONForTenant("default", "dev-flow", "")
	if err != nil {
		t.Fatalf("skill view: %v", err)
	}
	if !strings.Contains(view, "Inspect files") {
		t.Fatalf("view should include full content, got:\n%s", view)
	}

	prompt, display, ok, err := ResolveSkillInvocationForTenant("default", "/dev-flow", "apply it")
	if err != nil || !ok {
		t.Fatalf("resolve skill slash: ok=%v display=%s err=%v", ok, display, err)
	}
	if display != "dev-flow" || !strings.Contains(prompt, "apply it") {
		t.Fatalf("unexpected invocation prompt: display=%s prompt=%s", display, prompt)
	}
}

func TestSkillRootsIncludeWorkspaceAndCodexCompatibleDirs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workspace := t.TempDir()
	t.Chdir(workspace)

	workspaceSkillDir := filepath.Join(workspace, ".selfmind", "skills", "workspace-flow")
	if err := os.MkdirAll(workspaceSkillDir, 0755); err != nil {
		t.Fatalf("create workspace skill dir: %v", err)
	}
	if err := atomicWriteFile(filepath.Join(workspaceSkillDir, "SKILL.md"), ensureFrontMatter("Workspace body", "workspace-flow", "workspace skill")); err != nil {
		t.Fatalf("write workspace skill: %v", err)
	}
	codexSkillDir := filepath.Join(workspace, ".agents", "skills", "codex-flow")
	if err := os.MkdirAll(codexSkillDir, 0755); err != nil {
		t.Fatalf("create codex-compatible skill dir: %v", err)
	}
	if err := atomicWriteFile(filepath.Join(codexSkillDir, "SKILL.md"), ensureFrontMatter("Codex body", "codex-flow", "codex-compatible skill")); err != nil {
		t.Fatalf("write codex-compatible skill: %v", err)
	}

	skills, err := ListSkillsForTenant("default", false)
	if err != nil {
		t.Fatalf("list skills: %v", err)
	}
	var foundWorkspace, foundCodex bool
	for _, skill := range skills {
		switch skill.Name {
		case "workspace-flow":
			foundWorkspace = true
			if skill.Scope != SkillScopeWorkspace || !skill.Writable {
				t.Fatalf("unexpected workspace skill metadata: %+v", skill)
			}
		case "codex-flow":
			foundCodex = true
			if skill.Scope != SkillScopeWorkspace || skill.Writable {
				t.Fatalf("unexpected codex-compatible skill metadata: %+v", skill)
			}
		}
	}
	if !foundWorkspace || !foundCodex {
		t.Fatalf("expected workspace and codex-compatible skills, got: %+v", skills)
	}
}

func TestReadOnlySkillCannotBeMutated(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workspace := t.TempDir()
	t.Chdir(workspace)
	codexSkillDir := filepath.Join(workspace, ".agents", "skills", "readonly-flow")
	if err := os.MkdirAll(codexSkillDir, 0755); err != nil {
		t.Fatalf("create readonly skill dir: %v", err)
	}
	if err := atomicWriteFile(filepath.Join(codexSkillDir, "SKILL.md"), ensureFrontMatter("Do not mutate", "readonly-flow", "readonly skill")); err != nil {
		t.Fatalf("write readonly skill: %v", err)
	}

	tool := NewSkillManageTool()
	_, err := tool.Execute(map[string]interface{}{
		"action":   "patch",
		"name":     "readonly-flow",
		"old_text": "Do not mutate",
		"new_text": "Mutated",
	})
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("patching read-only skill should fail clearly, got: %v", err)
	}
}

func TestDynamicSkillToolIsInstructionOnly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	registry := NewRegistry()
	disp := NewDispatcherWithRegistry(registry)
	disp.RegisterTool(NewSkillManageTool())

	_, err := disp.Dispatch("skill_manage", map[string]interface{}{
		"action":  "create",
		"name":    "exec-flow",
		"content": "```bash\necho should-not-run\n```",
	})
	if err != nil {
		t.Fatalf("create skill: %v", err)
	}
	out, err := registry.Dispatch("skill:exec-flow", map[string]interface{}{})
	if err != nil {
		t.Fatalf("dispatch dynamic skill: %v", err)
	}
	if !strings.Contains(out, "instruction-only") || strings.Contains(out, "should-not-run") {
		t.Fatalf("dynamic skill tool should not execute or echo skill code blocks, got:\n%s", out)
	}
}

func TestSkillDisableSkipsSlashInvocation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	createTestSkill(t, "default", "toggle-flow", "Toggle body.")

	tool := NewSkillManageTool()
	if _, err := tool.Execute(map[string]interface{}{"action": "disable", "name": "toggle-flow"}); err != nil {
		t.Fatalf("disable skill: %v", err)
	}
	list, err := SkillsListJSONForTenant("default", "", false)
	if err != nil {
		t.Fatalf("list skills: %v", err)
	}
	if !strings.Contains(list, `"state": "disabled"`) {
		t.Fatalf("disabled skill should remain visible in list:\n%s", list)
	}
	if _, _, ok, err := ResolveSkillInvocationForTenant("default", "/toggle-flow", ""); ok || err != nil {
		t.Fatalf("disabled skill should not resolve slash invocation, ok=%v err=%v", ok, err)
	}
	if _, err := tool.Execute(map[string]interface{}{"action": "enable", "name": "toggle-flow"}); err != nil {
		t.Fatalf("enable skill: %v", err)
	}
	if _, _, ok, err := ResolveSkillInvocationForTenant("default", "/toggle-flow", ""); !ok || err != nil {
		t.Fatalf("enabled skill should resolve slash invocation, ok=%v err=%v", ok, err)
	}
}

func TestSkillBundleInvocation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	createTestSkill(t, "default", "inspect-flow", "Inspect first.")
	createTestSkill(t, "default", "test-flow", "Run tests second.")

	_, err := SaveSkillBundleForTenant("default", SkillBundle{
		Name:        "backend-dev",
		Description: "Backend development workflow",
		Skills:      []string{"inspect-flow", "test-flow"},
		Instruction: "Use both skills together.",
	})
	if err != nil {
		t.Fatalf("save bundle: %v", err)
	}

	prompt, display, ok, err := ResolveSkillInvocationForTenant("default", "/backend-dev", "change auth")
	if err != nil || !ok {
		t.Fatalf("resolve bundle slash: ok=%v display=%s err=%v", ok, display, err)
	}
	if display != "backend-dev" || !strings.Contains(prompt, "Inspect first.") || !strings.Contains(prompt, "Run tests second.") {
		t.Fatalf("unexpected bundle prompt: %s", prompt)
	}
}

func TestSkillManageFuzzyPatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tool := NewSkillManageTool()
	_, err := tool.Execute(map[string]interface{}{
		"action":  "create",
		"name":    "patch-me",
		"content": "Steps:\n  - Inspect the file\n  - Run tests\n",
	})
	if err != nil {
		t.Fatalf("create skill: %v", err)
	}
	_, err = tool.Execute(map[string]interface{}{
		"action":   "patch",
		"name":     "patch-me",
		"old_text": "- Inspect the file\n- Run tests",
		"new_text": "- Inspect the file\n- Run focused tests",
	})
	if err != nil {
		t.Fatalf("fuzzy patch should handle indentation: %v", err)
	}
	view, _ := SkillViewJSONForTenant("default", "patch-me", "")
	if !strings.Contains(view, "Run focused tests") {
		t.Fatalf("patch was not applied:\n%s", view)
	}
}

func TestCuratorDryRunArchiveAndRestore(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	createTestSkill(t, "default", "old-flow", "Old reusable flow.")
	if err := MarkSkillCreated("default", "old-flow", SkillSourceAgentCreated, "test"); err != nil {
		t.Fatalf("mark created: %v", err)
	}
	dir, err := getSkillsDir("default")
	if err != nil {
		t.Fatalf("skills dir: %v", err)
	}
	old := time.Now().UTC().AddDate(0, 0, -120).Format(time.RFC3339)
	if err := updateSkillUsageForDir(dir, "old-flow", func(rec *SkillUsageRecord) {
		rec.Source = SkillSourceAgentCreated
		rec.LastUsed = old
	}); err != nil {
		t.Fatalf("usage update: %v", err)
	}

	resp, err := RunCuratorForTenantWithOptions("default", CuratorOptions{DryRun: true, WriteReport: true})
	if err != nil {
		t.Fatalf("dry-run curator: %v", err)
	}
	if !strings.Contains(resp, "would archive old-flow") || !strings.Contains(resp, "Report:") {
		t.Fatalf("unexpected dry-run response:\n%s", resp)
	}
	if _, err := findSkill("default", "old-flow"); err != nil {
		t.Fatalf("dry-run should not archive: %v", err)
	}

	if _, err := ArchiveSkillForTenant("default", "old-flow"); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if _, err := RestoreSkillForTenant("default", "old-flow"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, err := findSkill("default", "old-flow"); err != nil {
		t.Fatalf("restored skill should be active: %v", err)
	}
}

func TestSkillCatalogInstallOfficialAndAudit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	resp, err := InstallSkillFromSource("default", "official/codebase-inspection", "", false)
	if err != nil {
		t.Fatalf("install official skill: %v", err)
	}
	if !strings.Contains(resp, "codebase-inspection") {
		t.Fatalf("unexpected install response: %s", resp)
	}
	audit, err := AuditSkillsForTenant("default", "codebase-inspection")
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if !strings.Contains(audit, `"status": "ok"`) {
		t.Fatalf("unexpected audit response:\n%s", audit)
	}
	usage, err := LoadSkillUsage("default")
	if err != nil {
		t.Fatalf("load usage: %v", err)
	}
	if usage["codebase-inspection"].Source != SkillSourceCatalog {
		t.Fatalf("official catalog install should be catalog source, got %+v", usage["codebase-inspection"])
	}
	skillsDir, err := getSkillsDir("default")
	if err != nil {
		t.Fatalf("skills dir: %v", err)
	}
	lock, err := loadSkillCatalogLockForDir(skillsDir)
	if err != nil {
		t.Fatalf("load catalog lock: %v", err)
	}
	entry := lock.Skills["codebase-inspection"]
	if entry.SourceKind != "official" || entry.ContentHash == "" || len(entry.Files) == 0 {
		t.Fatalf("unexpected catalog lock entry: %+v", entry)
	}
}

func TestSkillCatalogInstallProtectsUserSkills(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sourceA := writeSkillFixture(t, "catalog-flow", "first body", map[string]string{
		"references/note.md": "first reference",
	})
	sourceB := writeSkillFixture(t, "catalog-flow", "second body", nil)

	if _, err := InstallSkillFromSource("default", sourceA, "", false); err != nil {
		t.Fatalf("install first fixture: %v", err)
	}
	if _, err := InstallSkillFromSource("default", sourceB, "", false); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate install should require force, got: %v", err)
	}
	view, err := ReadSkillForTenant("default", "catalog-flow")
	if err != nil {
		t.Fatalf("read skill: %v", err)
	}
	if !strings.Contains(view, "first body") || strings.Contains(view, "second body") {
		t.Fatalf("duplicate install overwrote content:\n%s", view)
	}

	resp, err := InstallSkillFromSource("default", sourceB, "", true)
	if err != nil {
		t.Fatalf("force install: %v", err)
	}
	if !strings.Contains(resp, "backed up") {
		t.Fatalf("force install should report backup path, got: %s", resp)
	}
	view, err = ReadSkillForTenant("default", "catalog-flow")
	if err != nil {
		t.Fatalf("read forced skill: %v", err)
	}
	if !strings.Contains(view, "second body") || strings.Contains(view, "first body") {
		t.Fatalf("force install did not replace content:\n%s", view)
	}
	skillsDir, err := getSkillsDir("default")
	if err != nil {
		t.Fatalf("skills dir: %v", err)
	}
	lock, err := loadSkillCatalogLockForDir(skillsDir)
	if err != nil {
		t.Fatalf("load catalog lock: %v", err)
	}
	entry := lock.Skills["catalog-flow"]
	if entry.SourceKind != "local" || entry.LastBackupPath == "" {
		t.Fatalf("force install should update local lock with backup path: %+v", entry)
	}
	backupSkill := filepath.Join(entry.LastBackupPath, "catalog-flow", "SKILL.md")
	data, err := os.ReadFile(backupSkill)
	if err != nil {
		t.Fatalf("read backup skill: %v", err)
	}
	if !strings.Contains(string(data), "first body") {
		t.Fatalf("backup should contain old skill, got:\n%s", data)
	}
}

func TestSkillCatalogInstallDetectsLegacyCollision(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	skillsDir, err := getSkillsDir("default")
	if err != nil {
		t.Fatalf("skills dir: %v", err)
	}
	legacyPath := filepath.Join(skillsDir, "legacy-flow.md")
	if err := atomicWriteFile(legacyPath, ensureFrontMatter("legacy body", "legacy-flow", "legacy")); err != nil {
		t.Fatalf("write legacy skill: %v", err)
	}
	source := writeSkillFixture(t, "legacy-flow", "new body", nil)
	if _, err := InstallSkillFromSource("default", source, "", false); err == nil || !strings.Contains(err.Error(), "legacy-file") {
		t.Fatalf("legacy collision should require force, got: %v", err)
	}
	if _, err := InstallSkillFromSource("default", source, "", true); err != nil {
		t.Fatalf("force install over legacy: %v", err)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy file should be moved into backup, stat err=%v", err)
	}
	view, err := ReadSkillForTenant("default", "legacy-flow")
	if err != nil {
		t.Fatalf("read new skill: %v", err)
	}
	if !strings.Contains(view, "new body") {
		t.Fatalf("new directory skill not installed:\n%s", view)
	}
}

func TestCuratorSkipsCatalogInstalledSkills(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	source := writeSkillFixture(t, "catalog-old", "durable body", nil)
	if _, err := InstallSkillFromSource("default", source, "", false); err != nil {
		t.Fatalf("install catalog skill: %v", err)
	}
	skillsDir, err := getSkillsDir("default")
	if err != nil {
		t.Fatalf("skills dir: %v", err)
	}
	old := time.Now().UTC().AddDate(0, 0, -120).Format(time.RFC3339)
	if err := updateSkillUsageForDir(skillsDir, "catalog-old", func(rec *SkillUsageRecord) {
		rec.Source = SkillSourceCatalog
		rec.LastUsed = old
	}); err != nil {
		t.Fatalf("usage update: %v", err)
	}
	resp, err := RunCuratorForTenantWithOptions("default", CuratorOptions{})
	if err != nil {
		t.Fatalf("run curator: %v", err)
	}
	if strings.Contains(resp, "archived catalog-old") || strings.Contains(resp, "marked stale catalog-old") {
		t.Fatalf("curator should skip catalog skill, got:\n%s", resp)
	}
	info, err := findSkill("default", "catalog-old")
	if err != nil {
		t.Fatalf("catalog skill should remain active: %v", err)
	}
	if info.Source != SkillSourceCatalog || info.State != SkillStateActive {
		t.Fatalf("unexpected catalog skill state: %+v", info)
	}
}

func createTestSkill(t *testing.T, tenantID, name, body string) {
	t.Helper()
	content := ensureFrontMatter(body, name, "test skill")
	dir, err := getSkillsDir(tenantID)
	if err != nil {
		t.Fatalf("skills dir: %v", err)
	}
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatalf("create skill dir: %v", err)
	}
	if err := atomicWriteFile(filepath.Join(skillDir, "SKILL.md"), content); err != nil {
		t.Fatalf("write skill: %v", err)
	}
}

func writeSkillFixture(t *testing.T, name, body string, support map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := atomicWriteFile(filepath.Join(dir, "SKILL.md"), ensureFrontMatter(body, name, "fixture skill")); err != nil {
		t.Fatalf("write fixture SKILL.md: %v", err)
	}
	for rel, content := range support {
		target := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			t.Fatalf("create fixture support dir: %v", err)
		}
		if err := atomicWriteFile(target, content); err != nil {
			t.Fatalf("write fixture support file: %v", err)
		}
	}
	return dir
}
