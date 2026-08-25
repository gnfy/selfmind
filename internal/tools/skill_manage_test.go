package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/kernel"
)

func directSkillMutationArgs(args map[string]interface{}) map[string]interface{} {
	scope, _ := args["_invocation_scope"].(kernel.ToolInvocationScope)
	if scope.ControlTenantID == "" {
		scope.ControlTenantID = "default"
	}
	scope.SkillMutationMode = kernel.SkillMutationDirect
	args["_invocation_scope"] = scope
	return args
}

func TestSkillManageTool_EnforcesTrustedMutationMode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tool := NewSkillManageTool()
	if _, err := tool.Execute(map[string]interface{}{
		"action": "create", "name": "missing-scope", "content": "must fail closed",
	}); err == nil || !strings.Contains(err.Error(), "mode=none") {
		t.Fatalf("missing trusted scope did not fail closed: %v", err)
	}

	blockedScope := kernel.ToolInvocationScope{
		ControlTenantID:   "default",
		SkillMutationMode: kernel.SkillMutationNone,
	}
	_, err := tool.Execute(map[string]interface{}{
		"action":              "create",
		"name":                "blocked-skill",
		"content":             "Safe reusable steps.",
		"skill_mutation_mode": kernel.SkillMutationDirect,
		"_invocation_scope":   blockedScope,
	})
	if err == nil || !strings.Contains(err.Error(), "mode=none") {
		t.Fatalf("untrusted mutation mode bypassed guard: %v", err)
	}

	if _, err := tool.Execute(map[string]interface{}{
		"action":            "list",
		"_invocation_scope": blockedScope,
	}); err != nil {
		t.Fatalf("read action should remain available: %v", err)
	}

	candidateScope := blockedScope
	candidateScope.SkillMutationMode = kernel.SkillMutationCandidateOnly
	_, err = tool.Execute(map[string]interface{}{
		"action":            "create",
		"name":              "candidate-cannot-overwrite-active",
		"content":           "Safe reusable steps.",
		"_invocation_scope": candidateScope,
	})
	if err == nil || !strings.Contains(err.Error(), "mode=candidate_only") {
		t.Fatalf("candidate mode unexpectedly wrote active skill: %v", err)
	}

	directScope := blockedScope
	directScope.SkillMutationMode = kernel.SkillMutationDirect
	if _, err := tool.Execute(map[string]interface{}{
		"action":            "create",
		"name":              "direct-skill",
		"content":           "Safe reusable steps.",
		"_invocation_scope": directScope,
	}); err != nil {
		t.Fatalf("explicit direct mutation failed: %v", err)
	}
}

func TestSkillManageTool_CreateUpdateDelete(t *testing.T) {
	// Use a temporary skills directory
	tmpDir := t.TempDir()
	originalHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", originalHome)

	tool := NewSkillManageTool()
	execute := func(args map[string]interface{}) (string, error) { return tool.Execute(directSkillMutationArgs(args)) }

	// 1. Create
	result, err := execute(map[string]interface{}{
		"action":      "create",
		"name":        "docker-debug",
		"content":     "Use `docker logs -f <container>` to stream logs. Use `docker exec -it <container> sh` for shell access.",
		"description": "Docker debugging workflow",
	})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if !strings.Contains(result, "created") {
		t.Errorf("Expected success message, got: %s", result)
	}

	// Verify file exists and has front matter
	skillPath := filepath.Join(tmpDir, ".selfmind", "default", "skills", "docker-debug", "SKILL.md")
	data, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("skill file not found: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "---") {
		t.Error("Expected YAML front matter")
	}
	if !strings.Contains(content, "docker-debug") {
		t.Error("Expected skill name in content")
	}

	// 2. Update
	result, err = execute(map[string]interface{}{
		"action":  "update",
		"name":    "docker-debug",
		"content": "Updated: Always check `docker ps` first, then use `docker logs -f <container>`.",
	})
	if err != nil {
		t.Fatalf("update failed: %v", err)
	}
	if !strings.Contains(result, "edited") {
		t.Errorf("Expected update success, got: %s", result)
	}

	// 2b. Patch
	result, err = execute(map[string]interface{}{
		"action":   "patch",
		"name":     "docker-debug",
		"old_text": "Always check",
		"new_text": "First check",
	})
	if err != nil {
		t.Fatalf("patch failed: %v", err)
	}
	if !strings.Contains(result, "patched") {
		t.Errorf("Expected patch success, got: %s", result)
	}

	history, err := execute(map[string]interface{}{
		"action": "history",
		"name":   "docker-debug",
	})
	if err != nil {
		t.Fatalf("history failed: %v", err)
	}
	if !strings.Contains(history, "create docker-debug") || !strings.Contains(history, "patch docker-debug") {
		t.Fatalf("expected create and patch history, got:\n%s", history)
	}
	changes, err := ListSkillLearningChanges("default", "docker-debug", 10)
	if err != nil {
		t.Fatalf("ListSkillLearningChanges failed: %v", err)
	}
	patchID := ""
	for _, change := range changes {
		if change.Action == "patch" {
			patchID = change.ID
			break
		}
	}
	if patchID == "" {
		t.Fatalf("patch history id not found: %+v", changes)
	}
	result, err = execute(map[string]interface{}{
		"action":    "undo",
		"change_id": patchID,
	})
	if err != nil {
		t.Fatalf("undo patch failed: %v", err)
	}
	if !strings.Contains(result, "Undid skill patch") {
		t.Fatalf("unexpected undo result: %s", result)
	}
	data, err = os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("read skill after undo failed: %v", err)
	}
	if !strings.Contains(string(data), "Always check") {
		t.Fatalf("expected patch undo to restore old text, got:\n%s", data)
	}

	// 2c. Support file
	result, err = execute(map[string]interface{}{
		"action":       "write_file",
		"name":         "docker-debug",
		"file_path":    "references/example.md",
		"file_content": "example",
	})
	if err != nil {
		t.Fatalf("write_file failed: %v", err)
	}
	supportPath := filepath.Join(tmpDir, ".selfmind", "default", "skills", "docker-debug", "references", "example.md")
	if _, err := os.Stat(supportPath); err != nil {
		t.Fatalf("support file not found: %v", err)
	}

	// 3. Delete
	result, err = execute(map[string]interface{}{
		"action": "delete",
		"name":   "docker-debug",
	})
	if err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	if !strings.Contains(result, "deleted") {
		t.Errorf("Expected delete success, got: %s", result)
	}

	// Verify file removed
	skillDir := filepath.Dir(skillPath)
	if _, err := os.Stat(skillDir); !os.IsNotExist(err) {
		t.Error("Expected skill directory to be deleted")
	}
}

func TestSkillManageTool_DuplicateCreate(t *testing.T) {
	tmpDir := t.TempDir()
	originalHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", originalHome)

	tool := NewSkillManageTool()

	_, _ = tool.Execute(directSkillMutationArgs(map[string]interface{}{
		"action":  "create",
		"name":    "my-skill",
		"content": "content",
	}))

	_, err := tool.Execute(directSkillMutationArgs(map[string]interface{}{
		"action":  "create",
		"name":    "my-skill",
		"content": "content2",
	}))
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("Expected duplicate error, got: %v", err)
	}
}

func TestSkillManageTool_SecurityScan(t *testing.T) {
	tmpDir := t.TempDir()
	originalHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", originalHome)

	tool := NewSkillManageTool()

	_, err := tool.Execute(directSkillMutationArgs(map[string]interface{}{
		"action":  "create",
		"name":    "dangerous",
		"content": "Run rm -rf / to clean up",
	}))
	if err == nil || !strings.Contains(err.Error(), "security scan failed") {
		t.Errorf("Expected security scan error, got: %v", err)
	}
}

func TestEnsureFrontMatter(t *testing.T) {
	// Already has front matter — should not wrap
	content := "---\nname: test\n---\nbody"
	result := ensureFrontMatter(content, "test", "desc")
	if result != content {
		t.Errorf("Expected no change when front matter exists, got:\n%s", result)
	}

	// No front matter — should wrap
	content = "Just body text"
	result = ensureFrontMatter(content, "my-skill", "does things")
	if !strings.HasPrefix(result, "---") {
		t.Errorf("Expected front matter prefix, got:\n%s", result)
	}
	if !strings.Contains(result, "name: my-skill") {
		t.Errorf("Expected name in front matter, got:\n%s", result)
	}
}

func TestManagedSkillDescriptionLimitsCharactersAndBytes(t *testing.T) {
	valid := ensureFrontMatter("body", "valid", strings.Repeat("界", SkillDescriptionMaxChars))
	if err := ValidateManagedSkillDescription(valid); err != nil {
		t.Fatalf("valid description rejected: %v", err)
	}
	tooManyChars := ensureFrontMatter("body", "long-chars", strings.Repeat("a", SkillDescriptionMaxChars+1))
	if err := ValidateManagedSkillDescription(tooManyChars); err == nil || !strings.Contains(err.Error(), "characters") {
		t.Fatalf("character ceiling not enforced: %v", err)
	}
	tooManyBytes := ensureFrontMatter("body", "long-bytes", strings.Repeat("界", SkillDescriptionMaxBytes/3+1))
	if err := ValidateManagedSkillDescription(tooManyBytes); err == nil || !strings.Contains(err.Error(), "UTF-8 bytes") {
		t.Fatalf("byte ceiling not enforced: %v", err)
	}
}
