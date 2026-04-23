package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSkillManageTool_CreateUpdateDelete(t *testing.T) {
	// Use a temporary skills directory
	tmpDir := t.TempDir()
	originalHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", originalHome)

	tool := NewSkillManageTool()

	// 1. Create
	result, err := tool.Execute(map[string]interface{}{
		"action":  "create",
		"name":    "docker-debug",
		"content": "Use `docker logs -f <container>` to stream logs. Use `docker exec -it <container> sh` for shell access.",
		"description": "Docker debugging workflow",
	})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if !strings.Contains(result, "created successfully") {
		t.Errorf("Expected success message, got: %s", result)
	}

	// Verify file exists and has front matter
	skillPath := filepath.Join(tmpDir, ".selfmind", "default", "skills", "docker-debug.md")
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
	result, err = tool.Execute(map[string]interface{}{
		"action":  "update",
		"name":    "docker-debug",
		"content": "Updated: Always check `docker ps` first, then use `docker logs -f <container>`.",
	})
	if err != nil {
		t.Fatalf("update failed: %v", err)
	}
	if !strings.Contains(result, "updated successfully") {
		t.Errorf("Expected update success, got: %s", result)
	}

	// 3. Delete
	result, err = tool.Execute(map[string]interface{}{
		"action": "delete",
		"name":   "docker-debug",
	})
	if err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	if !strings.Contains(result, "deleted successfully") {
		t.Errorf("Expected delete success, got: %s", result)
	}

	// Verify file removed
	if _, err := os.Stat(skillPath); !os.IsNotExist(err) {
		t.Error("Expected skill file to be deleted")
	}
}

func TestSkillManageTool_DuplicateCreate(t *testing.T) {
	tmpDir := t.TempDir()
	originalHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", originalHome)

	tool := NewSkillManageTool()

	_, _ = tool.Execute(map[string]interface{}{
		"action":  "create",
		"name":    "my-skill",
		"content": "content",
	})

	_, err := tool.Execute(map[string]interface{}{
		"action":  "create",
		"name":    "my-skill",
		"content": "content2",
	})
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

	_, err := tool.Execute(map[string]interface{}{
		"action":  "create",
		"name":    "dangerous",
		"content": "Run rm -rf / to clean up",
	})
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
