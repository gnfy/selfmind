package kernel

// Deterministic coverage for SkillStore.Prune low-value pruning: a metric with
// call_count < 3 AND idle beyond the threshold is removed; a frequently-used
// (call_count >= 3) or recently-used metric is kept. The SQLite provider only
// ever writes CURRENT_TIMESTAMP into last_used, so the test backdates idle
// skills through a direct connection to the same memory.db after closing the
// provider (the provider's single worker owns the connection while open).

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"selfmind/internal/kernel/memory"

	_ "modernc.org/sqlite"
)

func TestSkillStorePruneRemovesLowValueKeepsUsedAndRecent(t *testing.T) {
	baseDir := t.TempDir()
	tenant := "default"
	ctx := context.Background()

	provider, err := memory.NewSQLiteProvider(baseDir)
	if err != nil {
		t.Fatalf("sqlite provider: %v", err)
	}
	store := NewSkillStore(memory.NewMemoryManager(provider))

	// low-idle: 1 call (below the <3 floor) — will be backdated past the threshold.
	if err := store.RecordCall(ctx, tenant, "low-idle"); err != nil {
		t.Fatalf("record low-idle: %v", err)
	}
	// frequent-idle: 3 calls — idle too, but call_count >= 3 must protect it.
	for i := 0; i < 3; i++ {
		if err := store.RecordCall(ctx, tenant, "frequent-idle"); err != nil {
			t.Fatalf("record frequent-idle: %v", err)
		}
	}
	// recent-low: 1 call, last_used now — recency must protect it.
	if err := store.RecordCall(ctx, tenant, "recent-low"); err != nil {
		t.Fatalf("record recent-low: %v", err)
	}
	if err := provider.Close(); err != nil {
		t.Fatalf("close provider: %v", err)
	}

	db, err := sql.Open("sqlite", filepath.Join(baseDir, tenant, "memory.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	old := time.Now().UTC().AddDate(0, 0, -60).Format("2006-01-02 15:04:05")
	if _, err := db.Exec(`UPDATE skill_metrics SET last_used = ? WHERE skill_name IN ('low-idle', 'frequent-idle')`, old); err != nil {
		t.Fatalf("backdate metrics: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	provider2, err := memory.NewSQLiteProvider(baseDir)
	if err != nil {
		t.Fatalf("reopen provider: %v", err)
	}
	t.Cleanup(func() { _ = provider2.Close() })
	store2 := NewSkillStore(memory.NewMemoryManager(provider2))

	pruned, err := store2.PruneWithDefaults(ctx, tenant)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("pruned = %d, want 1 (only low-idle)", pruned)
	}

	metrics, err := store2.GetStats(ctx, tenant)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	remaining := map[string]bool{}
	for _, m := range metrics {
		remaining[m.SkillName] = true
	}
	if remaining["low-idle"] {
		t.Fatal("low-value idle skill should have been pruned")
	}
	if !remaining["frequent-idle"] {
		t.Fatal("frequently-used skill must survive pruning even when idle")
	}
	if !remaining["recent-low"] {
		t.Fatal("recently-used skill must survive pruning even with low call count")
	}
}
