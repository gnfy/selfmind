package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestLegacyFactImport pins the migration contract (docs/memory-governance
// §6): opening a tenant DB imports legacy facts as immutable observations,
// folds same-statement duplicates into one canonical memory with the
// repetition carried in evidence counters, marks pinned rows as protected,
// skips profile residue, and is idempotent across reopens.
func TestLegacyFactImport(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	p1, err := NewSQLiteProvider(dir)
	if err != nil {
		t.Fatal(err)
	}
	seed := []Fact{
		{ID: "fact-a", Target: "user", Content: "User communicates in Chinese.", Source: SourceFactExtractor, Scope: "global", Confidence: 0.65, LastVerifiedAt: time.Now()},
		{ID: "fact-b", Target: "user", Content: "user communicates in chinese", Source: SourceFactExtractor, Scope: "global", Confidence: 0.65, LastVerifiedAt: time.Now()},
		{ID: "fact-c", Target: "memory", Content: "The repo builds with GOWORK=off go test", Source: "", Scope: "", Confidence: 0},
	}
	for _, f := range seed {
		if err := p1.AddFactMeta(ctx, "tenant", f); err != nil {
			t.Fatal(err)
		}
	}
	if err := p1.AddFact(ctx, "tenant", "pinned", "Owner name is Zero"); err != nil {
		t.Fatal(err)
	}
	if err := p1.AddFact(ctx, "tenant", "profile", `{"summary":"stale profile residue"}`); err != nil {
		t.Fatal(err)
	}
	if err := p1.Close(); err != nil {
		t.Fatal(err)
	}

	verify := func(p *SQLiteProvider, pass string) {
		t.Helper()
		all, err := p.ListCanonicalMemories(ctx, "tenant", CanonicalFilter{})
		if err != nil {
			t.Fatalf("%s: list canonicals: %v", pass, err)
		}
		if len(all) != 3 { // dup pair folded + memory fact + pinned fact
			t.Fatalf("%s: canonical count = %d, want 3: %+v", pass, len(all), all)
		}
		var dup, pinned *CanonicalMemory
		for i := range all {
			if all[i].Content == "The repo builds with GOWORK=off go test" {
				continue
			}
			if all[i].Pinned {
				pinned = &all[i]
			} else {
				dup = &all[i]
			}
			if all[i].Content == `{"summary":"stale profile residue"}` {
				t.Fatalf("%s: profile residue must not be imported", pass)
			}
		}
		if dup == nil || dup.EvidenceCount != 2 || dup.Occurrences != 2 {
			t.Fatalf("%s: duplicate pair must fold into one canonical with 2 evidence: %+v", pass, dup)
		}
		if dup.Confidence <= 0.65 {
			t.Fatalf("%s: folded duplicate must gain corroboration confidence: %v", pass, dup.Confidence)
		}
		if pinned == nil || !pinned.UserConfirmed {
			t.Fatalf("%s: pinned fact must import as protected: %+v", pass, pinned)
		}
		obs, err := p.ObservationsForMemory(ctx, "tenant", dup.ID)
		if err != nil || len(obs) != 2 {
			t.Fatalf("%s: observations for folded canonical = %d err=%v", pass, len(obs), err)
		}
		for _, o := range obs {
			if o.Source != SourceFactExtractor {
				t.Fatalf("%s: imported observation must keep its source: %+v", pass, o)
			}
		}
		events, err := p.ListMemoryEvents(ctx, "tenant", 10)
		if err != nil || len(events) == 0 {
			t.Fatalf("%s: import must write an audit event: %v err=%v", pass, events, err)
		}
	}

	p2, err := NewSQLiteProvider(dir)
	if err != nil {
		t.Fatal(err)
	}
	verify(p2, "first import")
	if err := p2.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: the import must be idempotent — same counts, no double evidence.
	p3, err := NewSQLiteProvider(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer p3.Close()
	verify(p3, "idempotent reopen")
}

func TestCanonicalIntakeIsScopedAndReplayIdempotent(t *testing.T) {
	ctx := context.Background()
	p, err := NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	base := IntakeWrite{Decision: "ADD", Target: "memory", Source: SourceFactExtractor, Content: "Service uses port 8080"}
	for _, scope := range []string{"workspace:a", "workspace:b"} {
		w := base
		w.Scope = scope
		w.WorkspaceID = strings.TrimPrefix(scope, "workspace:")
		w.RunID = "seed-" + scope
		w.AnalyzerVersion = 1
		if err := p.ApplyIntakeWrite(ctx, "tenant", w); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := p.ListCanonicalMemories(ctx, "tenant", CanonicalFilter{})
	if err != nil || len(rows) != 2 {
		t.Fatalf("same statement in two workspaces must remain distinct: rows=%+v err=%v", rows, err)
	}

	reinforce := IntakeWrite{
		Decision: "REINFORCE", Target: "memory", Scope: "workspace:a", Source: SourceFactExtractor,
		Content: "Service uses port 8080", RefContent: "Service uses port 8080",
		RunID: "run-reinforce", AnalyzerVersion: 1, DecisionKey: "item-0",
	}
	if err := p.ApplyIntakeWrite(ctx, "tenant", reinforce); err != nil {
		t.Fatal(err)
	}
	if err := p.ApplyIntakeWrite(ctx, "tenant", reinforce); err != nil {
		t.Fatal(err)
	}
	rows, _ = p.ListCanonicalMemories(ctx, "tenant", CanonicalFilter{})
	for _, row := range rows {
		switch row.Scope {
		case "workspace:a":
			if row.Occurrences != 2 {
				t.Fatalf("same proposal replay counted twice in workspace a: %+v", row)
			}
		case "workspace:b":
			if row.Occurrences != 1 {
				t.Fatalf("workspace b was reinforced by workspace a: %+v", row)
			}
		}
	}
}

func TestCanonicalStatusByHashIsScoped(t *testing.T) {
	ctx := context.Background()
	p, err := NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, scope := range []string{"workspace:a", "workspace:b"} {
		if err := p.ApplyIntakeWrite(ctx, "tenant", IntakeWrite{
			Decision: "ADD", Target: "memory", Scope: scope, Content: "Uses PostgreSQL",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.SetCanonicalStatusByHash(ctx, "tenant", "memory", "workspace:a", "Uses PostgreSQL", CanonicalForgotten, "test"); err != nil {
		t.Fatal(err)
	}
	rows, err := p.ListCanonicalMemories(ctx, "tenant", CanonicalFilter{Statuses: []string{CanonicalActive, CanonicalForgotten}})
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	for _, row := range rows {
		if row.Scope == "workspace:a" && row.Status != CanonicalForgotten {
			t.Fatalf("workspace a status=%s", row.Status)
		}
		if row.Scope == "workspace:b" && row.Status != CanonicalActive {
			t.Fatalf("workspace b was mutated across scope: %+v", row)
		}
	}
}

func TestMergeEventUndoRestoresMembersAndEvidence(t *testing.T) {
	ctx := context.Background()
	p, err := NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for i, content := range []string{"User prefers concise Go examples", "User likes concise examples in Go"} {
		if err := p.ApplyIntakeWrite(ctx, "tenant", IntakeWrite{
			Decision: "ADD", Target: "user", Scope: "global", Content: content,
			RunID: fmt.Sprintf("seed-%d", i), AnalyzerVersion: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := p.ListCanonicalMemories(ctx, "tenant", CanonicalFilter{})
	if len(before) != 2 {
		t.Fatalf("seed rows=%+v", before)
	}
	ids := []string{before[0].ID, before[1].ID}
	if err := p.ApplyMerge(ctx, "tenant", MergeWrite{
		MemberIDs: ids, Canonical: "User prefers concise Go examples", Target: "user", Scope: "global", Actor: "consolidator",
	}); err != nil {
		t.Fatal(err)
	}
	events, _ := p.ListMemoryEvents(ctx, "tenant", 20)
	var mergeID string
	for _, event := range events {
		if event.Action == "merge" {
			mergeID = event.ID
			break
		}
	}
	if mergeID == "" {
		t.Fatal("merge event not recorded")
	}
	if err := p.UndoMemoryEvent(ctx, "tenant", mergeID, "user"); err != nil {
		t.Fatal(err)
	}
	active, _ := p.ListCanonicalMemories(ctx, "tenant", CanonicalFilter{})
	if len(active) != 2 {
		t.Fatalf("undo active rows=%+v", active)
	}
	for _, id := range ids {
		obs, err := p.ObservationsForMemory(ctx, "tenant", id)
		if err != nil || len(obs) != 1 {
			t.Fatalf("member %s evidence not restored: obs=%+v err=%v", id, obs, err)
		}
	}
}
