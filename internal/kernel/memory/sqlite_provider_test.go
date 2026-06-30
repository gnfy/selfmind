package memory

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSQLiteProvider_FTS5(t *testing.T) {
	dir := t.TempDir()
	p, err := NewSQLiteProvider(dir)
	if err != nil {
		t.Fatalf("NewSQLiteProvider: %v", err)
	}
	defer p.Close()

	tenantID := "test-user"
	traj := []byte(`{"messages":[{"role":"user","content":"hello world"},{"role":"assistant","content":"hi there"}]}`)

	if err := p.SaveTrajectory(nil, tenantID, "cli", traj); err != nil {
		t.Fatalf("SaveTrajectory: %v", err)
	}
	if err := p.IndexMessagesFromTrajectory(nil, tenantID, "cli", "sess-001", traj); err != nil {
		t.Fatalf("IndexMessagesFromTrajectory: %v", err)
	}

	sessions, err := p.SearchSessions(tenantID, "hello", 5)
	if err != nil {
		t.Fatalf("SearchSessions: %v", err)
	}
	if len(sessions) == 0 {
		t.Fatal("expected at least 1 result for 'hello'")
	}
	if sessions[0].SessionID != "sess-001" {
		t.Errorf("expected session_id sess-001, got %s", sessions[0].SessionID)
	}

	recent, err := p.ListRecentSessions(tenantID, 5)
	if err != nil {
		t.Fatalf("ListRecentSessions: %v", err)
	}
	if len(recent) == 0 || recent[0].SessionID != "sess-001" {
		t.Fatalf("expected recent session sess-001, got %#v", recent)
	}

	messages, err := p.GetSessionMessages(tenantID, "sess-001", 1, 1)
	if err != nil {
		t.Fatalf("GetSessionMessages: %v", err)
	}
	if len(messages) == 0 || messages[0].Role != "user" || messages[0].MessageID != 1 {
		t.Fatalf("expected indexed user message, got %#v", messages)
	}

	sessions, err = p.SearchSessions(tenantID, "xyznonexistent", 5)
	if err != nil {
		t.Fatalf("SearchSessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected 0 results for 'xyznonexistent', got %d", len(sessions))
	}
}

func TestSQLiteProvider_MultiTenantIsolation(t *testing.T) {
	dir := t.TempDir()
	p, err := NewSQLiteProvider(dir)
	if err != nil {
		t.Fatalf("NewSQLiteProvider: %v", err)
	}
	defer p.Close()

	itraj := []byte(`{"messages":[{"role":"user","content":"secret data for alice"}]}`)
	ibtraj := []byte(`{"messages":[{"role":"user","content":"secret data for bob"}]}`)

	p.IndexMessagesFromTrajectory(nil, "alice", "cli", "alice-sess", itraj)
	p.IndexMessagesFromTrajectory(nil, "bob", "cli", "bob-sess", ibtraj)

	results, _ := p.SearchSessions("alice", "alice", 5)
	if len(results) == 0 {
		t.Error("alice should find her own session")
	}

	results, _ = p.SearchSessions("alice", "bob", 5)
	if len(results) != 0 {
		t.Error("alice should not find bob's session")
	}
}

func TestSQLiteProvider_TenantScopedPath(t *testing.T) {
	path := TenantScopedPath("/data", "user1", "memory.db")
	expected := "/data/user1/memory.db"
	if path != expected {
		t.Errorf("expected %s, got %s", expected, path)
	}
}

func TestSQLiteProvider_DBDirCreation(t *testing.T) {
	dir := t.TempDir()
	p, err := NewSQLiteProvider(dir)
	if err != nil {
		t.Fatalf("NewSQLiteProvider: %v", err)
	}
	defer p.Close()

	err = p.SaveTrajectory(nil, "brand-new-tenant", "cli", []byte(`{}`))
	if err != nil {
		t.Fatalf("SaveTrajectory: %v", err)
	}

	expectedPath := filepath.Join(dir, "brand-new-tenant", "memory.db")
	if _, err := os.Stat(expectedPath); err != nil {
		t.Errorf("expected DB file at %s: %v", expectedPath, err)
	}
}

func TestFactMetadataRoundTrip(t *testing.T) {
	p, err := NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatalf("NewSQLiteProvider: %v", err)
	}
	defer p.Close()

	verified := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	want := Fact{
		Target: "user", Content: "prefers Go", Source: "user",
		Scope: "global", Confidence: 0.9, CreatedFromRun: "run-1", LastVerifiedAt: verified,
	}
	if err := p.AddFactMeta(nil, "t1", want); err != nil {
		t.Fatalf("AddFactMeta: %v", err)
	}
	// Legacy metadata-less write must still work and read back with zero metadata.
	if err := p.AddFact(nil, "t1", "user", "plain fact"); err != nil {
		t.Fatalf("AddFact: %v", err)
	}

	facts, err := p.GetFacts(nil, "t1", "user")
	if err != nil {
		t.Fatalf("GetFacts: %v", err)
	}
	var meta, plain *Fact
	for i := range facts {
		switch facts[i].Content {
		case "prefers Go":
			meta = &facts[i]
		case "plain fact":
			plain = &facts[i]
		}
	}
	if meta == nil || plain == nil {
		t.Fatalf("expected both facts, got %+v", facts)
	}
	if meta.Source != "user" || meta.Scope != "global" || meta.Confidence != 0.9 || meta.CreatedFromRun != "run-1" {
		t.Fatalf("metadata not persisted: %+v", *meta)
	}
	if !meta.LastVerifiedAt.Equal(verified) {
		t.Fatalf("last_verified_at = %v, want %v", meta.LastVerifiedAt, verified)
	}
	if plain.Source != "" || plain.Confidence != 0 {
		t.Fatalf("plain fact should have zero metadata: %+v", *plain)
	}
}

func TestAddMissingColumnsBackwardCompat(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	// Simulate a pre-migration facts table + row.
	if _, err := db.Exec(`CREATE TABLE facts (id TEXT PRIMARY KEY, target TEXT, content TEXT, created_at DATETIME DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("create old table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO facts (id, target, content) VALUES ('1','user','legacy fact')`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	cols := []columnDef{
		{"source", "TEXT DEFAULT ''"},
		{"scope", "TEXT DEFAULT ''"},
		{"confidence", "REAL DEFAULT 0"},
		{"created_from_run", "TEXT DEFAULT ''"},
		{"last_verified_at", "DATETIME"},
	}
	if err := addMissingColumns(db, "facts", cols); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Idempotent: running again must not error.
	if err := addMissingColumns(db, "facts", cols); err != nil {
		t.Fatalf("migrate (idempotent): %v", err)
	}
	// The legacy row reads back with default metadata.
	var content, source string
	var confidence float64
	if err := db.QueryRow(`SELECT content, COALESCE(source,''), COALESCE(confidence,0) FROM facts WHERE id='1'`).Scan(&content, &source, &confidence); err != nil {
		t.Fatalf("select migrated: %v", err)
	}
	if content != "legacy fact" || source != "" || confidence != 0 {
		t.Fatalf("legacy row = %q/%q/%v", content, source, confidence)
	}
}
