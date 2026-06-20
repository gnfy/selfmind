package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
