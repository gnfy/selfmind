package tools

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/kernel/memory"
)

func TestMemoryToolListAction(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	mem := memory.NewMemoryManager(provider)
	tool := NewMemoryTool(mem)
	tenantID := "tenant-list"

	for _, c := range []struct{ target, content string }{
		{"user", "Prefers Go over Python."},
		{"memory", "Repo builds with GOWORK=off."},
		{"pinned", "Name is Wei."},
	} {
		if _, err := tool.Execute(map[string]interface{}{"action": "add", "target": c.target, "content": c.content, "_tenant_id": tenantID}); err != nil {
			t.Fatalf("add %s: %v", c.target, err)
		}
	}

	out, err := tool.Execute(map[string]interface{}{"action": "list", "_tenant_id": tenantID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []string{"## Memory", "### User", "Prefers Go over Python.", "Project / Environment", "Repo builds with GOWORK=off.", "Pinned", "Name is Wei."} {
		if !strings.Contains(out, want) {
			t.Fatalf("list output missing %q:\n%s", want, out)
		}
	}
}

func TestMemoryToolHistoryAndUndo(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	mem := memory.NewMemoryManager(provider)
	tool := NewMemoryTool(mem)
	tenantID := "tenant-a"

	if _, err := tool.Execute(map[string]interface{}{
		"action":     "add",
		"target":     "user",
		"content":    "Prefers concise answers.",
		"_tenant_id": tenantID,
	}); err != nil {
		t.Fatalf("add failed: %v", err)
	}
	changes, err := ListMemoryLearningChanges(tenantID, "user", 10)
	if err != nil {
		t.Fatalf("history failed: %v", err)
	}
	if len(changes) == 0 || changes[0].Action != "add" || changes[0].After != "Prefers concise answers." {
		t.Fatalf("unexpected add history: %+v", changes)
	}
	history, err := tool.Execute(map[string]interface{}{
		"action":     "history",
		"target":     "user",
		"_tenant_id": tenantID,
	})
	if err != nil {
		t.Fatalf("history action failed: %v", err)
	}
	if !strings.Contains(history, changes[0].ID) {
		t.Fatalf("history output missing change id %s:\n%s", changes[0].ID, history)
	}
	if _, err := tool.Execute(map[string]interface{}{
		"action":     "undo",
		"change_id":  changes[0].ID,
		"_tenant_id": tenantID,
	}); err != nil {
		t.Fatalf("undo add failed: %v", err)
	}
	facts, err := mem.GetFacts(ctx, tenantID, "user")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 0 {
		t.Fatalf("expected add to be undone, facts=%+v", facts)
	}

	if _, err := tool.Execute(map[string]interface{}{
		"action":     "add",
		"target":     "memory",
		"content":    "Project uses PowerShell for local commands.",
		"_tenant_id": tenantID,
	}); err != nil {
		t.Fatalf("second add failed: %v", err)
	}
	if _, err := tool.Execute(map[string]interface{}{
		"action":     "remove",
		"target":     "memory",
		"old_text":   "PowerShell",
		"_tenant_id": tenantID,
	}); err != nil {
		t.Fatalf("remove failed: %v", err)
	}
	changes, err = ListMemoryLearningChanges(tenantID, "memory", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) == 0 || changes[0].Action != "remove" || changes[0].Before != "Project uses PowerShell for local commands." {
		t.Fatalf("unexpected remove history: %+v", changes)
	}
	if _, err := tool.Execute(map[string]interface{}{
		"action":     "undo",
		"change_id":  changes[0].ID,
		"_tenant_id": tenantID,
	}); err != nil {
		t.Fatalf("undo remove failed: %v", err)
	}
	facts, err = mem.GetFacts(ctx, tenantID, "memory")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || facts[0].Content != "Project uses PowerShell for local commands." {
		t.Fatalf("expected removed memory to be restored, facts=%+v", facts)
	}
}
