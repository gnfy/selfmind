package router

import (
	"context"
	"testing"

	"selfmind/internal/kernel"
)

func TestRunIdleTimeoutParsesEnv(t *testing.T) {
	t.Setenv("SELFMIND_RUN_IDLE_TIMEOUT", "")
	if runIdleTimeout() != 0 {
		t.Fatal("unset → disabled (0)")
	}
	t.Setenv("SELFMIND_RUN_IDLE_TIMEOUT", "5m")
	if got := runIdleTimeout(); got.String() != "5m0s" {
		t.Fatalf("5m → %s", got)
	}
	t.Setenv("SELFMIND_RUN_IDLE_TIMEOUT", "garbage")
	if runIdleTimeout() != 0 {
		t.Fatal("invalid → disabled (0)")
	}
}

func TestWorkspaceSerialKey(t *testing.T) {
	if got := workspaceSerialKey(context.Background()); got != "" {
		t.Fatalf("no workspace → key %q, want empty (parallel)", got)
	}
	ctx := kernel.WithWorkspaceContext(context.Background(), kernel.WorkspaceContext{ID: "ws-42", Root: "/tmp/x"})
	if got := workspaceSerialKey(ctx); got != "ws-42" {
		t.Fatalf("workspace key = %q, want ws-42", got)
	}
}

func TestEnableWorkerPoolWiring(t *testing.T) {
	g := &Gateway{agent: &kernel.Agent{}}

	// Empty extra → no pool (default single-agent path stays unchanged).
	g.EnableWorkerPool(nil)
	if g.pool != nil || g.agents != nil {
		t.Fatal("empty extra must leave the pool disabled")
	}

	// Two extra workers → pool of 3 (primary + 2), all checked in.
	g.EnableWorkerPool([]*kernel.Agent{{}, {}})
	if g.pool == nil {
		t.Fatal("expected pool to be enabled")
	}
	if g.pool.Workers() != 3 {
		t.Fatalf("pool workers = %d, want 3", g.pool.Workers())
	}
	if cap(g.agents) != 3 || len(g.agents) != 3 {
		t.Fatalf("agents channel cap/len = %d/%d, want 3/3", cap(g.agents), len(g.agents))
	}
}
