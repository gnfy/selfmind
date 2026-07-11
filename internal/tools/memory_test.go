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
	for _, want := range []string{"## Memory", "Confirmed by you", "Name is Wei.", "Development and tools", "Prefers Go over Python.", "Projects and workspaces", "Repo builds with GOWORK=off."} {
		if !strings.Contains(out, want) {
			t.Fatalf("list output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "tenant-list") || strings.Contains(out, "src=") || strings.Contains(out, "conf=") {
		t.Fatalf("default memory view leaked storage metadata:\n%s", out)
	}
}

func TestMemoryToolOverviewGroupsEvidenceWithoutDeletingIt(t *testing.T) {
	if got := namedProjectName("user refers to 'selfmind' as a project/codebase to analyze."); got != "selfmind" {
		t.Fatalf("named project extraction = %q, want selfmind", got)
	}
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	mem := memory.NewMemoryManager(provider)
	tool := NewMemoryTool(mem)
	tenantID := "tenant-grouped"

	for _, fact := range []memory.Fact{
		{Target: "user", Content: "User has a project called selfmind.", Source: memory.SourceTurnExtractor, Confidence: 0.5},
		{Target: "user", Content: "User has a project named selfmind.", Source: memory.SourceFactExtractor, Confidence: 0.65},
		{Target: "user", Content: "User refers to 'selfmind' as a project/codebase to analyze.", Source: memory.SourceTurnExtractor, Confidence: 0.5},
		{Target: "user", Content: "用户偏好使用中文讨论技术问题。", Source: memory.SourceUser, Confidence: 0.9},
	} {
		if err := mem.AddFactMeta(ctx, tenantID, fact); err != nil {
			t.Fatalf("add fact: %v", err)
		}
	}

	out, err := tool.Execute(map[string]interface{}{"action": "list", "_tenant_id": tenantID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Projects and workspaces") || !strings.Contains(out, "(3 related records)") {
		t.Fatalf("overview did not group related project evidence:\n%s", out)
	}
	if !strings.Contains(out, "用户偏好使用中文讨论技术问题。") {
		t.Fatalf("overview lost UTF-8 memory content:\n%s", out)
	}
	facts, err := mem.GetFacts(ctx, tenantID, "user")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 4 {
		t.Fatalf("read model changed raw evidence, facts=%+v", facts)
	}
	for _, fact := range facts {
		if strings.Contains(out, fact.ID) {
			t.Fatalf("overview leaked raw UUID %q:\n%s", fact.ID, out)
		}
	}
}

func TestMemoryToolSearchShowCorrectAndForget(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	mem := memory.NewMemoryManager(provider)
	tool := NewMemoryTool(mem)
	tenantID := "tenant-manage"

	original := memory.Fact{
		Target:     "user",
		Content:    "User prefers very long answers.",
		Source:     memory.SourceFactExtractor,
		Scope:      "global",
		Confidence: 0.65,
	}
	if err := mem.AddFactMeta(ctx, tenantID, original); err != nil {
		t.Fatal(err)
	}
	facts, err := mem.GetFacts(ctx, tenantID, "user")
	if err != nil || len(facts) != 1 {
		t.Fatalf("load original fact: facts=%+v err=%v", facts, err)
	}
	originalID := facts[0].ID
	ref := shortMemoryRef(originalID)

	search, err := tool.Execute(map[string]interface{}{"action": "search", "query": "long answers", "_tenant_id": tenantID})
	if err != nil || !strings.Contains(search, ref) {
		t.Fatalf("search missing ref %q: out=%q err=%v", ref, search, err)
	}
	detail, err := tool.Execute(map[string]interface{}{"action": "show", "ref": ref, "_tenant_id": tenantID})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Source: fact_extractor", "Scope: global", "Confidence: 65%"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detail missing %q:\n%s", want, detail)
		}
	}

	if _, err := tool.Execute(map[string]interface{}{
		"action": "correct", "ref": ref, "content": "User prefers concise, structured answers.", "_tenant_id": tenantID,
	}); err != nil {
		t.Fatalf("correct: %v", err)
	}
	facts, err = mem.GetFacts(ctx, tenantID, "user")
	if err != nil || len(facts) != 1 {
		t.Fatalf("load corrected fact: facts=%+v err=%v", facts, err)
	}
	if facts[0].ID != originalID || facts[0].Content != "User prefers concise, structured answers." || facts[0].Source != memory.SourceUser || facts[0].Confidence != memory.BaseConfidence(memory.SourceUser) {
		t.Fatalf("unexpected corrected fact: %+v", facts[0])
	}

	if _, err := tool.Execute(map[string]interface{}{"action": "forget", "ref": ref, "_tenant_id": tenantID}); err != nil {
		t.Fatalf("forget: %v", err)
	}
	facts, err = mem.GetFacts(ctx, tenantID, "user")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 0 {
		t.Fatalf("forgotten fact still exists: %+v", facts)
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
	visible, _ := memory.ReadModelFacts(ctx, mem, tenantID)
	for _, fact := range visible {
		if fact.Content == "Prefers concise answers." {
			t.Fatalf("undone add still visible through canonical read model: %+v", fact)
		}
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
	visible, _ = memory.ReadModelFacts(ctx, mem, tenantID)
	found := false
	for _, fact := range visible {
		found = found || fact.Content == "Project uses PowerShell for local commands."
	}
	if !found {
		t.Fatalf("undo remove did not restore canonical read model: %+v", visible)
	}
}
