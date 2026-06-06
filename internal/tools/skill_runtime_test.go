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
