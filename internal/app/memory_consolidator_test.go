package app

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/config"
)

func seedParaphrases(t *testing.T, mem *memory.MemoryManager, tenantID string) {
	t.Helper()
	ctx := context.Background()
	store, ok := mem.Canonical()
	if !ok {
		t.Fatal("canonical store required")
	}
	for _, content := range []string{
		"User prefers viewing code inline rather than written to files",
		"The user prefers code displayed inline rather than written to files",
		"User prefers code examples inline rather than written into files",
	} {
		if err := store.ApplyIntakeWrite(ctx, tenantID, memory.IntakeWrite{
			Decision: "ADD", Target: "memory", Scope: "global",
			Source: memory.SourceFactExtractor, Content: content,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func activeCount(t *testing.T, mem *memory.MemoryManager, tenantID string) (active, archived int) {
	t.Helper()
	store, _ := mem.Canonical()
	all, err := store.ListCanonicalMemories(context.Background(), tenantID, memory.CanonicalFilter{
		Statuses: []string{memory.CanonicalActive, memory.CanonicalArchived},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range all {
		if c.Status == memory.CanonicalActive {
			active++
		} else {
			archived++
		}
	}
	return active, archived
}

// TestConsolidatorShadowNeverWrites: shadow mode judges, checkpoints, and
// reports — and applies NOTHING, however confident the judge is.
func TestConsolidatorShadowNeverWrites(t *testing.T) {
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	mgr := memory.NewMemoryManager(provider)
	seedParaphrases(t, mgr, "person")

	judge := &capturingProviderStub{content: `{"action":"MERGE","canonical":"User prefers code shown inline rather than written to files","confidence":0.99}`}
	c := &MemoryConsolidator{provider: judge, mem: mgr, gov: config.MemoryGovernanceConfig{Enabled: true, Mode: "shadow"}, reportDir: t.TempDir()}
	if err := c.RunOnce(context.Background(), "person"); err != nil {
		t.Fatal(err)
	}
	active, archived := activeCount(t, mgr, "person")
	if active != 3 || archived != 0 {
		t.Fatalf("shadow mode must not apply: active=%d archived=%d", active, archived)
	}
	store, _ := mgr.Canonical()
	judged, err := store.ListJudgedClusterIDs(context.Background(), "person")
	if err != nil || len(judged) == 0 {
		t.Fatalf("shadow judgement must be checkpointed: %v err=%v", judged, err)
	}

	// Second pass: the checkpoint suppresses re-judging the same cluster.
	before := judge.calls()
	if err := c.RunOnce(context.Background(), "person"); err != nil {
		t.Fatal(err)
	}
	if judge.calls() != before {
		t.Fatalf("already-judged cluster was re-judged: %d -> %d", before, judge.calls())
	}
}

// TestConsolidatorMergeApply: merge-only mode applies a high-confidence MERGE
// — one new active canonical, members archived (never deleted), audit event.
func TestConsolidatorMergeApply(t *testing.T) {
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	mgr := memory.NewMemoryManager(provider)
	seedParaphrases(t, mgr, "person")

	judge := &capturingProviderStub{content: `{"action":"MERGE","canonical":"User prefers code shown inline rather than written to files","confidence":0.97}`}
	c := &MemoryConsolidator{provider: judge, mem: mgr, gov: config.MemoryGovernanceConfig{Enabled: true, Mode: "merge-only"}, reportDir: t.TempDir()}
	if err := c.RunOnce(context.Background(), "person"); err != nil {
		t.Fatal(err)
	}
	active, archived := activeCount(t, mgr, "person")
	if active != 1 || archived != 3 {
		t.Fatalf("merge apply expected 1 active + 3 archived, got active=%d archived=%d", active, archived)
	}
	store, _ := mgr.Canonical()
	actives, _ := store.ListCanonicalMemories(context.Background(), "person", memory.CanonicalFilter{})
	if len(actives) != 1 || actives[0].EvidenceCount != 3 {
		t.Fatalf("merged canonical must carry member evidence: %+v", actives)
	}
	events, _ := store.ListMemoryEvents(context.Background(), "person", 20)
	var sawMerge bool
	for _, e := range events {
		if e.Action == "merge" && e.Snapshot != "" {
			sawMerge = true
		}
	}
	if !sawMerge {
		t.Fatal("merge must write a snapshot-bearing audit event")
	}
}

// TestConsolidatorMergeGateRejectsLowConfidence: below auto_merge_confidence
// nothing is applied even in merge-only mode.
func TestConsolidatorMergeGateRejectsLowConfidence(t *testing.T) {
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	mgr := memory.NewMemoryManager(provider)
	seedParaphrases(t, mgr, "person")

	judge := &capturingProviderStub{content: `{"action":"MERGE","canonical":"User prefers code shown inline rather than written to files","confidence":0.80}`}
	c := &MemoryConsolidator{provider: judge, mem: mgr, gov: config.MemoryGovernanceConfig{Enabled: true, Mode: "merge-only"}, reportDir: t.TempDir()}
	if err := c.RunOnce(context.Background(), "person"); err != nil {
		t.Fatal(err)
	}
	if active, archived := activeCount(t, mgr, "person"); active != 3 || archived != 0 {
		t.Fatalf("under-confident merge must be kept, got active=%d archived=%d", active, archived)
	}
}

// TestConsolidatorCaps: full mode archives the weakest beyond
// max_active_global; pinned/user-confirmed rows are immune.
func TestConsolidatorCaps(t *testing.T) {
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	mgr := memory.NewMemoryManager(provider)
	ctx := context.Background()
	store, _ := mgr.Canonical()
	for _, seed := range []struct {
		content string
		source  string
	}{
		{"Repo builds with Go modules", memory.SourceFactExtractor},
		{"Deploy target runs Ubuntu server", memory.SourceFactExtractor},
		{"Owner name is Zero", memory.SourceUser}, // user-confirmed: immune
	} {
		if err := store.ApplyIntakeWrite(ctx, "person", memory.IntakeWrite{
			Decision: "ADD", Target: "memory", Scope: "global", Source: seed.source, Content: seed.content,
		}); err != nil {
			t.Fatal(err)
		}
	}

	judge := &capturingProviderStub{content: `{"action":"KEEP","confidence":0.9}`}
	c := &MemoryConsolidator{provider: judge, mem: mgr, gov: config.MemoryGovernanceConfig{
		Enabled: true, Mode: "full", MaxActiveGlobal: 1, ArchiveAfter: "8760h",
	}, reportDir: t.TempDir()}
	if err := c.RunOnce(ctx, "person"); err != nil {
		t.Fatal(err)
	}
	actives, _ := store.ListCanonicalMemories(ctx, "person", memory.CanonicalFilter{})
	var confirmedSurvives bool
	for _, a := range actives {
		if strings.Contains(a.Content, "Owner name is Zero") {
			confirmedSurvives = true
		}
	}
	if !confirmedSurvives {
		t.Fatalf("user-confirmed memory must never be auto-archived: %+v", actives)
	}
	if len(actives) > 2 { // 1 allowed + the immune confirmed row
		t.Fatalf("caps did not archive overflow: %d actives", len(actives))
	}
}

func TestConsolidatorWorkspaceCapAndPauseDefault(t *testing.T) {
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	mgr := memory.NewMemoryManager(provider)
	ctx := context.Background()
	store, _ := mgr.Canonical()
	for i := 0; i < 3; i++ {
		if err := store.ApplyIntakeWrite(ctx, "person", memory.IntakeWrite{
			Decision: "ADD", Target: "memory", Scope: "workspace:alpha",
			Content: fmt.Sprintf("Alpha project convention %d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ApplyIntakeWrite(ctx, "person", memory.IntakeWrite{
		Decision: "ADD", Target: "memory", Scope: "workspace:beta", Content: "Beta project convention",
	}); err != nil {
		t.Fatal(err)
	}
	c := &MemoryConsolidator{provider: &capturingProviderStub{content: `{"action":"KEEP","confidence":0.9}`}, mem: mgr, gov: config.MemoryGovernanceConfig{
		Enabled: true, Mode: "full", MaxActiveGlobal: 20, MaxActivePerWorkspace: 2, ArchiveAfter: "8760h",
	}, reportDir: t.TempDir()}
	if !c.PauseWhileRunActive() {
		t.Fatal("foreground pause must default to true")
	}
	if err := c.RunOnce(ctx, "person"); err != nil {
		t.Fatal(err)
	}
	active, _ := store.ListCanonicalMemories(ctx, "person", memory.CanonicalFilter{})
	counts := map[string]int{}
	for _, item := range active {
		counts[item.Scope]++
	}
	if counts["workspace:alpha"] != 2 || counts["workspace:beta"] != 1 {
		t.Fatalf("workspace caps not enforced independently: %+v", counts)
	}
	pause := false
	c.gov.PauseWhileRunActive = &pause
	if c.PauseWhileRunActive() {
		t.Fatal("explicit pause_while_run_active=false ignored")
	}
}
