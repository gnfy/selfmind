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
	for _, want := range []string{"Memory", "SelfMind is managing", "`pinned", "Confirmed by you", "`development", "Development and tools", "`projects", "Projects and workspaces", "/memory category <name>"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list output missing %q:\n%s", want, out)
		}
	}
	for _, hidden := range []string{"Name is Wei.", "Prefers Go over Python.", "Repo builds with GOWORK=off."} {
		if strings.Contains(out, hidden) {
			t.Fatalf("overview should not expand individual memory %q:\n%s", hidden, out)
		}
	}
	if strings.Contains(out, "tenant-list") || strings.Contains(out, "src=") || strings.Contains(out, "conf=") {
		t.Fatalf("default memory view leaked storage metadata:\n%s", out)
	}
}

func TestMemoryToolRejectsTransientRunState(t *testing.T) {
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	mem := memory.NewMemoryManager(provider)
	tool := NewMemoryTool(mem)

	_, err = tool.Execute(map[string]interface{}{
		"action": "add", "target": "memory",
		"content":    "Build ID: cw-prod:0d4a9e81 has been created",
		"_tenant_id": "tenant-transient",
	})
	if err == nil || !strings.Contains(err.Error(), "task handoff") {
		t.Fatalf("expected transient memory rejection, got %v", err)
	}
	rows, err := provider.ListCanonicalMemories(context.Background(), "tenant-transient", memory.CanonicalFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("transient memory reached canonical store: %+v", rows)
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
	if !strings.Contains(out, "Projects and workspaces") {
		t.Fatalf("overview did not expose the project category:\n%s", out)
	}
	if strings.Contains(out, "User has a project called selfmind.") || strings.Contains(out, "用户偏好使用中文讨论技术问题。") {
		t.Fatalf("overview expanded individual memories instead of staying concise:\n%s", out)
	}
	category, err := tool.Execute(map[string]interface{}{
		"action": "category", "category": "projects", "page": 1, "_tenant_id": tenantID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(category, "User has a project") || !strings.Contains(category, "related memories 3") || !strings.Contains(category, "/memory show <ref>") {
		t.Fatalf("category view did not expose grouped, actionable memory:\n%s", category)
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

func TestMemoryViewRepairsLegacyUTF8AndFoldsNearDuplicates(t *testing.T) {
	var broken strings.Builder
	for _, b := range []byte("继续") {
		broken.WriteRune(rune(b))
	}
	if got := displayMemoryText(broken.String()); got != "继续" {
		t.Fatalf("displayMemoryText() = %q, want 继续", got)
	}

	facts := []memory.Fact{
		{Target: "user", Content: "The user dislikes jittery full-screen screen shake as hit feedback and prefers clearer hit-stop, knockback, sparks, and localized impact effects.", Confidence: 0.65},
		{Target: "user", Content: "User prefers realistic combat feedback (hit-stop, recoil, impact effects) over full-screen shake effects.", Confidence: 0.65},
		{Target: "user", Content: "The user prefers realistic fighting-game feedback such as hit-stop, knockback, impact sparks, and camera zoom over full-screen shake.", Confidence: 0.65},
		{Target: "user", Content: "User prefers realistic hit feedback like hit-stop, knockback, and impact effects over full-screen shake.", Confidence: 0.65},
	}
	groups := groupMemoryFacts(facts)
	if len(groups) >= len(facts) {
		t.Fatalf("near-duplicate preferences were not folded for display: groups=%d facts=%d", len(groups), len(facts))
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

func TestMemoryCanonicalDetailExplainsEvidence(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	mem := memory.NewMemoryManager(provider)
	tool := NewMemoryTool(mem)
	tenantID := "tenant-detail"

	if _, err := tool.Execute(map[string]interface{}{
		"action": "add", "target": "user", "content": "User prefers concise technical answers.", "_tenant_id": tenantID,
	}); err != nil {
		t.Fatal(err)
	}
	facts, _ := memory.ReadModelFacts(context.Background(), mem, tenantID)
	if len(facts) != 1 {
		t.Fatalf("facts=%+v", facts)
	}
	detail, err := tool.Execute(map[string]interface{}{
		"action": "show", "ref": shortMemoryRef(facts[0].ID), "_tenant_id": tenantID,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Status: active", "Evidence: 1 record(s)", "Supporting evidence", "agent"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("canonical detail missing %q:\n%s", want, detail)
		}
	}
}

func TestMemoryToolScopesProjectFactsAndPinsExistingCanonical(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	mem := memory.NewMemoryManager(provider)
	tool := NewMemoryTool(mem)
	tenantID := "tenant-scope-pin"

	if _, err := tool.Execute(map[string]interface{}{
		"action": "add", "target": "memory", "content": "This project uses PostgreSQL.",
		"_tenant_id": tenantID, "_workspace_id": "workspace-db",
	}); err != nil {
		t.Fatal(err)
	}
	facts, _ := memory.ReadModelFacts(ctx, mem, tenantID)
	if len(facts) != 1 || facts[0].Scope != "workspace:workspace-db" {
		t.Fatalf("workspace-scoped memory = %+v", facts)
	}
	ref := shortMemoryRef(facts[0].ID)
	if _, err := tool.Execute(map[string]interface{}{"action": "pin", "ref": ref, "_tenant_id": tenantID}); err != nil {
		t.Fatal(err)
	}
	rows, err := provider.ListCanonicalMemories(ctx, tenantID, memory.CanonicalFilter{})
	if err != nil || len(rows) != 1 || !rows[0].Pinned || !rows[0].UserConfirmed {
		t.Fatalf("pinned canonical = %+v err=%v", rows, err)
	}
	if _, err := tool.Execute(map[string]interface{}{"action": "unpin", "ref": ref, "_tenant_id": tenantID}); err != nil {
		t.Fatal(err)
	}
	rows, err = provider.ListCanonicalMemories(ctx, tenantID, memory.CanonicalFilter{})
	if err != nil || len(rows) != 1 || rows[0].Pinned || !rows[0].UserConfirmed {
		t.Fatalf("unpinned canonical = %+v err=%v", rows, err)
	}
	events, err := provider.ListMemoryEvents(ctx, tenantID, 20)
	if err != nil {
		t.Fatal(err)
	}
	var unpinEvent string
	for _, event := range events {
		if event.Action == "unpin" {
			unpinEvent = event.ID
			break
		}
	}
	if unpinEvent == "" {
		t.Fatalf("unpin audit event missing: %+v", events)
	}
	if err := provider.UndoMemoryEvent(ctx, tenantID, unpinEvent, "user"); err != nil {
		t.Fatal(err)
	}
	rows, err = provider.ListCanonicalMemories(ctx, tenantID, memory.CanonicalFilter{})
	if err != nil || len(rows) != 1 || !rows[0].Pinned || !rows[0].UserConfirmed {
		t.Fatalf("undone unpin canonical = %+v err=%v", rows, err)
	}

	diag, err := tool.Execute(map[string]interface{}{"action": "stats", "_tenant_id": tenantID})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Records: active 1", "Visible topics: 1", "user-confirmed 1", "workspace 1"} {
		if !strings.Contains(diag, want) {
			t.Fatalf("memory diagnostics missing %q:\n%s", want, diag)
		}
	}
}

func TestMemoryConflictsAreSeparatedFromOverview(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	mem := memory.NewMemoryManager(provider)
	store, ok := mem.Canonical()
	if !ok {
		t.Fatal("canonical store unavailable")
	}
	if err := store.ApplyIntakeWrite(ctx, "tenant-conflict", memory.IntakeWrite{
		Decision: "ADD", Target: "user", Scope: "global", Source: memory.SourceAgent, Content: "User prefers dark themes.",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyIntakeWrite(ctx, "tenant-conflict", memory.IntakeWrite{
		Decision: "CONFLICT", Target: "user", Scope: "global", Source: memory.SourceAgent,
		Content: "User prefers light themes.", RefContent: "User prefers dark themes.", Confidence: 0.9,
	}); err != nil {
		t.Fatal(err)
	}
	tool := NewMemoryTool(mem)
	overview, err := tool.Execute(map[string]interface{}{"action": "list", "_tenant_id": "tenant-conflict"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(overview, "need review") || !strings.Contains(overview, "/memory conflicts") {
		t.Fatalf("overview did not surface conflict attention:\n%s", overview)
	}
	conflicts, err := tool.Execute(map[string]interface{}{"action": "conflicts", "_tenant_id": "tenant-conflict"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(conflicts, "User prefers light themes.") || !strings.Contains(conflicts, "/memory show <ref>") {
		t.Fatalf("conflict view is not actionable:\n%s", conflicts)
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
