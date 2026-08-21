package eval

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/kernel/memory"
)

func TestApplyStateSeedsStoresCanonicalMemoryInPersonPartition(t *testing.T) {
	ctx := context.Background()
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()

	manager := memory.NewMemoryManager(provider)
	identity := &control.IdentityContext{
		TenantID: "tenant",
		PersonID: "person",
	}
	setup := &Setup{Memory: []SeedFact{{
		Target:    "user",
		Content:   "Release summaries should use one item per line.",
		Scope:     "global",
		Canonical: true,
	}}}

	if err := applyStateSeeds(ctx, nil, manager, identity, "workspace", t.TempDir(), "cli", setup); err != nil {
		t.Fatal(err)
	}

	personRows, err := provider.ListCanonicalMemories(ctx, identity.PersonID, memory.CanonicalFilter{
		Statuses: []string{memory.CanonicalActive},
		Limit:    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(personRows) != 1 {
		t.Fatalf("person canonical rows=%d want 1", len(personRows))
	}
	tenantRows, err := provider.ListCanonicalMemories(ctx, identity.TenantID, memory.CanonicalFilter{
		Statuses: []string{memory.CanonicalActive},
		Limit:    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tenantRows) != 0 {
		t.Fatalf("tenant canonical rows=%d want 0", len(tenantRows))
	}
}

func TestApplyStateSeedsCanBindWorkspaceSkill(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "eval", "skill-seed", "Skill Seed")
	if err != nil {
		t.Fatal(err)
	}
	workspaceRoot := t.TempDir()
	skillPath := filepath.Join(workspaceRoot, ".selfmind", "skills", "release-check", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillPath), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: release-check\ndescription: 检查发布版本\n---\n\nRead release.txt.\n"
	if err := os.WriteFile(skillPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	setup := &Setup{Task: &SeedTask{Title: "release", DefaultSkill: "release-check"}}
	if err := applyStateSeeds(ctx, store, nil, identity, "workspace-id", workspaceRoot, "cli", setup); err != nil {
		t.Fatal(err)
	}
	task, err := store.CurrentTask(ctx, identity.TenantID, identity.PersonID)
	if err != nil || task == nil {
		t.Fatalf("current task=%+v err=%v", task, err)
	}
	binding, err := store.GetTaskSkillBinding(ctx, identity.TenantID, identity.PersonID, task.ID)
	if err != nil || binding == nil || binding.SkillName != "release-check" || binding.State != control.TaskSkillBindingActive {
		t.Fatalf("binding=%+v err=%v", binding, err)
	}
}
