package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMigratePersonSkillsToControlDryRunAndApply(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".selfmind")
	person := "person_alice"
	source := SkillsDirForTenant(root, person)
	target := SkillsDirForTenant(root, "default")
	if err := os.MkdirAll(filepath.Join(source, "release-check"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: release-check\n---\nCheck the release.\n"
	if err := os.WriteFile(filepath.Join(source, "release-check", "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveSkillUsageForDir(source, map[string]SkillUsageRecord{
		"release-check": {Name: "release-check", Source: SkillSourceAgentCreated, State: SkillStateActive},
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	if err := appendLearningChange(LearningChange{ID: "learn_skill_1", TenantID: person, Kind: "skill", Target: "release-check", Action: "create", CreatedAt: nowRFC3339()}); err != nil {
		t.Fatal(err)
	}
	if err := appendLearningChange(LearningChange{ID: "learn_memory_1", TenantID: person, Kind: "memory", Target: "user", Action: "add", CreatedAt: nowRFC3339()}); err != nil {
		t.Fatal(err)
	}

	dry, err := MigratePersonSkillsToControl(root, "default", false, 24*time.Hour)
	if err != nil || dry.Migrated != 1 || dry.Applied {
		t.Fatalf("dry=%+v err=%v", dry, err)
	}
	if _, err := os.Stat(filepath.Join(target, "release-check", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("dry-run wrote target: %v", err)
	}

	applied, err := MigratePersonSkillsToControl(root, "default", true, 24*time.Hour)
	if err != nil || applied.Migrated != 1 || !applied.Applied {
		t.Fatalf("apply=%+v err=%v", applied, err)
	}
	data, err := os.ReadFile(filepath.Join(target, "release-check", "SKILL.md"))
	if err != nil || string(data) != content {
		t.Fatalf("target content=%q err=%v", data, err)
	}
	usage, err := loadSkillUsageForDir(target)
	if err != nil || usage["release-check"].MigratedFrom != person || usage["release-check"].Pinned {
		t.Fatalf("usage=%+v err=%v", usage, err)
	}
	controlLog, err := os.ReadFile(filepath.Join(root, "default", "learning", "learning-log.jsonl"))
	if err != nil || !strings.Contains(string(controlLog), "learn_skill_1") || strings.Contains(string(controlLog), "learn_memory_1") {
		t.Fatalf("control learning log=%q err=%v", controlLog, err)
	}
}

func TestMigratePersonSkillsReportsConflictWithoutOverwrite(t *testing.T) {
	root := t.TempDir()
	personDir := SkillsDirForTenant(root, "person_bob")
	controlDir := SkillsDirForTenant(root, "default")
	for _, dir := range []string{filepath.Join(personDir, "same"), filepath.Join(controlDir, "same")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(personDir, "same", "SKILL.md"), []byte("person"), 0o644)
	_ = os.WriteFile(filepath.Join(controlDir, "same", "SKILL.md"), []byte("control"), 0o644)
	report, err := MigratePersonSkillsToControl(root, "default", true, time.Hour)
	if err != nil || report.Conflicts != 1 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	data, _ := os.ReadFile(filepath.Join(controlDir, "same", "SKILL.md"))
	if string(data) != "control" {
		t.Fatalf("control content overwritten: %q", data)
	}
	if _, err := os.Stat(filepath.Join(personDir, "same", "SKILL.md")); err != nil {
		t.Fatalf("conflicting person copy removed: %v", err)
	}
}

func TestMigratePersonSkillsCleansOnlyEmptySkillDirectories(t *testing.T) {
	root := t.TempDir()
	empty := SkillsDirForTenant(root, "person_empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(usageFilePath(empty), []byte(`{"skills":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	dry, err := MigratePersonSkillsToControl(root, "default", false, time.Hour)
	if err != nil || dry.EmptyPartitions != 1 {
		t.Fatalf("dry=%+v err=%v", dry, err)
	}
	if _, err := os.Stat(empty); err != nil {
		t.Fatalf("dry-run removed empty partition: %v", err)
	}
	applied, err := MigratePersonSkillsToControl(root, "default", true, time.Hour)
	if err != nil || applied.EmptyPartitions != 1 {
		t.Fatalf("apply=%+v err=%v", applied, err)
	}
	if _, err := os.Stat(empty); !os.IsNotExist(err) {
		t.Fatalf("empty skill directory was not removed: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(empty)); err != nil {
		t.Fatalf("person partition outside skills must be preserved: %v", err)
	}
}
