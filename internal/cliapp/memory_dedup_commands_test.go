package cliapp

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"selfmind/internal/kernel/memory"

	_ "modernc.org/sqlite"
)

// dedupTestState is the snapshot the assertions compare across dry-run and
// apply passes.
type dedupTestState struct {
	observations  int
	edges         int
	confidence    float64
	evidenceCount int
	occurrences   int
	dedupEvents   int
	survivorID    string
}

// TestMaintenanceMemoryDedup seeds the exact duplicate shape the legacy
// import produced on a real machine — one deterministic `obs_` observation
// from live intake plus one bare-uuid import twin for the same
// (run, scope, statement), both edged to the same canonical whose counters
// were inflated to 2 — then pins: dry run mutates nothing; --apply keeps the
// deterministic row, removes the import twin and its edge, corrects the
// counters, and leaves one audit event; a second --apply is a no-op.
func TestMaintenanceMemoryDedup(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()

	// Live intake writes the canonical + the deterministic obs_ observation.
	provider, err := memory.NewSQLiteProvider(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.ApplyIntakeWrite(ctx, "person_test", memory.IntakeWrite{
		Decision: "ADD", Target: "memory", Scope: "workspace:ws",
		Source: memory.SourceFactExtractor, Content: "Deploy uses blue-green rollout",
		RunID: "run-1", AnalyzerVersion: 1, DecisionKey: "item-0",
	}); err != nil {
		t.Fatal(err)
	}
	provider.Close()

	// Mimic the measured defect state with direct SQL: the pre-fix legacy
	// import re-materialized the same evidence under the fact's own uuid and
	// reinforced the canonical, inflating both counters to 2.
	dbPath := filepath.Join(dataDir, "person_test", "memory.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	var memID, hash string
	if err := db.QueryRow(`SELECT id, normalized_hash FROM canonical_memories`).Scan(&memID, &hash); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Unix()
	if _, err := db.Exec(`INSERT INTO memory_observations
		(id, run_id, workspace_id, target, scope, source, content, normalized_hash, confidence_prior, status, created_at)
		VALUES ('11111111-dup', 'run-1', '', 'memory', 'workspace:ws', 'legacy',
			'Deploy uses blue-green rollout', ?, 0.6, 'accepted', ?)`, hash, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO facts
		(id, target, content, source, scope, confidence, created_from_run, last_verified_at)
		VALUES ('11111111-dup', 'memory', 'Deploy uses blue-green rollout', 'legacy',
			'workspace:ws', 0.6, 'run-1', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO memory_evidence (memory_id, observation_id, relation, created_at)
		VALUES (?, '11111111-dup', 'supports', ?)`, memID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE canonical_memories SET confidence = 0.75, evidence_count = 2, occurrences = 2 WHERE id = ?`, memID); err != nil {
		t.Fatal(err)
	}
	db.Close()

	newApp := func(args ...string) (*App, *bytes.Buffer) {
		out := &bytes.Buffer{}
		return &App{ctx: ctx, args: append([]string{"selfmind", "maintenance"}, args...), stdout: out, stderr: out}, out
	}
	readState := func() dedupTestState {
		t.Helper()
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		db.SetMaxOpenConns(1)
		var s dedupTestState
		if err := db.QueryRow(`SELECT COUNT(*) FROM memory_observations`).Scan(&s.observations); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM memory_evidence WHERE memory_id = ?`, memID).Scan(&s.edges); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT confidence, evidence_count, occurrences FROM canonical_memories WHERE id = ?`, memID).
			Scan(&s.confidence, &s.evidenceCount, &s.occurrences); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM memory_events WHERE actor = 'memory-dedup' AND action = 'dedup'`).
			Scan(&s.dedupEvents); err != nil {
			t.Fatal(err)
		}
		_ = db.QueryRow(`SELECT o.id FROM memory_observations o
			JOIN memory_evidence e ON e.observation_id = o.id
			WHERE e.memory_id = ?`, memID).Scan(&s.survivorID)
		return s
	}

	// Dry run: report the group, change nothing.
	app, out := newApp("memory-dedup", "--data-dir", dataDir)
	if handled, code := app.runMaintenanceCommandIfRequested(); !handled || code != 0 {
		t.Fatalf("dry run handled=%v code=%d output=%s", handled, code, out.String())
	}
	if !strings.Contains(out.String(), "11111111-dup") || !strings.Contains(out.String(), "Dry-run only") {
		t.Fatalf("dry run must report the redundant import row:\n%s", out.String())
	}
	if s := readState(); s.observations != 2 || s.edges != 2 || s.evidenceCount != 2 || s.occurrences != 2 || s.dedupEvents != 0 {
		t.Fatalf("dry run mutated the store: %+v", s)
	}

	// Apply: the deterministic obs_ row survives, the import twin and its
	// edge go, counters are corrected, and one audit event is appended.
	app, out = newApp("memory-dedup", "--data-dir", dataDir, "--apply")
	if handled, code := app.runMaintenanceCommandIfRequested(); !handled || code != 0 {
		t.Fatalf("apply handled=%v code=%d output=%s", handled, code, out.String())
	}
	s := readState()
	if s.observations != 1 || s.edges != 1 || s.evidenceCount != 1 || s.occurrences != 1 || s.confidence != 0.65 {
		t.Fatalf("apply left wrong state: %+v output=%s", s, out.String())
	}
	if !strings.HasPrefix(s.survivorID, "obs_") {
		t.Fatalf("keeper must be the deterministic intake observation, got %q", s.survivorID)
	}
	if s.dedupEvents != 1 {
		t.Fatalf("apply must append exactly one audit event, got %d", s.dedupEvents)
	}
	var eventID, snapshot string
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT id, snapshot FROM memory_events
		WHERE actor = 'memory-dedup' AND action = 'dedup' LIMIT 1`).Scan(&eventID, &snapshot); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	if strings.TrimSpace(snapshot) == "" {
		t.Fatal("dedup event must carry a reversible snapshot")
	}

	// Second apply: nothing left to remove, no new audit event.
	app, out = newApp("memory-dedup", "--data-dir", dataDir, "--apply")
	if handled, code := app.runMaintenanceCommandIfRequested(); !handled || code != 0 {
		t.Fatalf("second apply handled=%v code=%d output=%s", handled, code, out.String())
	}
	if !strings.Contains(out.String(), "No duplicate evidence groups found.") {
		t.Fatalf("second apply must be a no-op:\n%s", out.String())
	}
	if s := readState(); s.observations != 1 || s.edges != 1 || s.evidenceCount != 1 || s.occurrences != 1 || s.dedupEvents != 1 {
		t.Fatalf("second apply changed state: %+v", s)
	}

	// The audit event is a real safety boundary, not just a log entry: undo
	// restores the removed immutable evidence and its original derived stats.
	provider, err = memory.NewSQLiteProvider(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.UndoMemoryEvent(ctx, "person_test", eventID, "test"); err != nil {
		provider.Close()
		t.Fatal(err)
	}
	provider.Close()
	if s := readState(); s.observations != 2 || s.edges != 2 || s.evidenceCount != 2 || s.occurrences != 2 || s.confidence != 0.75 {
		t.Fatalf("undo did not restore the pre-dedup state: %+v", s)
	}
}

func TestMemoryDedupIgnoresUnprovenSameRunObservation(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	provider, err := memory.NewSQLiteProvider(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.ApplyIntakeWrite(ctx, "person_test", memory.IntakeWrite{
		Decision: "ADD", Target: "memory", Scope: "global", Source: memory.SourceFactExtractor,
		Content: "Independent evidence", RunID: "run-1", AnalyzerVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	provider.Close()

	dbPath := filepath.Join(dataDir, "person_test", "memory.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var memID, hash string
	if err := db.QueryRow(`SELECT id, normalized_hash FROM canonical_memories`).Scan(&memID, &hash); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Unix()
	if _, err := db.Exec(`INSERT INTO memory_observations
		(id, run_id, target, scope, source, content, normalized_hash, confidence_prior, status, created_at)
		VALUES ('independent-uuid', 'run-1', 'memory', 'global', 'agent',
			'Independent evidence', ?, 0.8, 'accepted', ?)`, hash, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO memory_evidence
		(memory_id, observation_id, relation, created_at) VALUES (?, 'independent-uuid', 'supports', ?)`, memID, now); err != nil {
		t.Fatal(err)
	}
	db.Close()

	out := &bytes.Buffer{}
	app := &App{ctx: ctx, args: []string{"selfmind", "maintenance", "memory-dedup", "--data-dir", dataDir, "--apply"}, stdout: out, stderr: out}
	if handled, code := app.runMaintenanceCommandIfRequested(); !handled || code != 0 {
		t.Fatalf("handled=%v code=%d output=%s", handled, code, out.String())
	}
	if !strings.Contains(out.String(), "No duplicate evidence groups found.") {
		t.Fatalf("unproven UUID observation must not be deleted:\n%s", out.String())
	}
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var observations, edges int
	_ = db.QueryRow(`SELECT COUNT(*) FROM memory_observations`).Scan(&observations)
	_ = db.QueryRow(`SELECT COUNT(*) FROM memory_evidence WHERE memory_id = ?`, memID).Scan(&edges)
	if observations != 2 || edges != 2 {
		t.Fatalf("unproven evidence was changed: observations=%d edges=%d", observations, edges)
	}
}
