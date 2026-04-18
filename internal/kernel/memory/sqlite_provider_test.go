package memory

import (
	"os"
	"path/filepath"
	"testing"
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
