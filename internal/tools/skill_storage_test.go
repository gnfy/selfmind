package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInjectedSkillStorageKeepsReadsSideEffectFree(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	base := filepath.Join(t.TempDir(), "assets")
	storage, err := NewSkillStorage(base)
	if err != nil {
		t.Fatal(err)
	}
	args := WithSkillStorage(nil, storage)

	if _, err := LoadSkillUsage("person_eval", args); err != nil {
		t.Fatalf("load usage: %v", err)
	}
	if _, err := ListMemoryLearningChanges("person_eval", "", 20, args); err != nil {
		t.Fatalf("list learning: %v", err)
	}
	for _, path := range []string{
		filepath.Join(base, "person_eval"),
		filepath.Join(home, ".selfmind", "person_eval"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("read created %s: %v", path, err)
		}
	}
}

func TestSkillInvocationResolverUsesInjectedStorage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	base := filepath.Join(t.TempDir(), "assets")
	storage, err := NewSkillStorage(base)
	if err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(base, "default", "skills", "custom-flow")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: custom-flow\ndescription: custom root\n---\n\nFollow the custom root."), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := NewSkillInvocationResolveTool().Execute(WithSkillStorage(map[string]interface{}{
		"command": "/custom-flow", "instruction": "do it", "_tenant_id": "default",
	}, storage))
	if err != nil {
		t.Fatal(err)
	}
	var resolved struct {
		Found       bool   `json:"found"`
		Kind        string `json:"kind"`
		Prompt      string `json:"prompt"`
		Name        string `json:"name"`
		SkillKey    string `json:"skill_key"`
		VersionHash string `json:"version_hash"`
		PackageHash string `json:"package_hash"`
	}
	if err := json.Unmarshal([]byte(result), &resolved); err != nil {
		t.Fatal(err)
	}
	if !resolved.Found || resolved.Kind != "skill" || resolved.Name != "custom-flow" || resolved.SkillKey == "" ||
		resolved.VersionHash == "" || resolved.PackageHash == "" || resolved.Prompt != "do it" {
		t.Fatalf("resolution = %s", result)
	}
	if strings.Contains(resolved.Prompt, "Follow the custom root") {
		t.Fatalf("resolver eagerly injected Skill body: %s", result)
	}
	if _, err := os.Stat(filepath.Join(home, ".selfmind", "default")); !os.IsNotExist(err) {
		t.Fatalf("resolver touched HOME: %v", err)
	}
}

func TestInjectedSkillStorageContainsWritesWithoutCreatingSiblingSkills(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	base := filepath.Join(t.TempDir(), "assets")
	storage, err := NewSkillStorage(base)
	if err != nil {
		t.Fatal(err)
	}

	RecordMemoryLearningChangeScopedWithStorage(storage, "person_eval", "user", "global", "add", "", "prefers concise output", "test")
	if _, err := os.Stat(filepath.Join(base, "person_eval", "learning")); err != nil {
		t.Fatalf("learning write missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "person_eval", "skills")); !os.IsNotExist(err) {
		t.Fatalf("learning write created sibling skills directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".selfmind", "person_eval")); !os.IsNotExist(err) {
		t.Fatalf("write escaped injected storage: %v", err)
	}
}
