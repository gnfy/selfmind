package httpapi

import (
	"context"
	"testing"

	"selfmind/internal/control/controltest"
	"selfmind/internal/executionenv"
)

func TestParallelApprovalContextIncludesActiveReadOnlyDirectories(t *testing.T) {
	store := controltest.NewStore(t)
	ctx := context.Background()
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "parallel-review", "Person")
	if err != nil {
		t.Fatal(err)
	}
	daemon := &Server{Control: store}
	c := daemon.coordinator()
	readRoot, writeRoot := t.TempDir(), t.TempDir()
	active := &activeRun{TenantID: owner.TenantID, PersonID: owner.PersonID, RunID: "reading-run", ExecutionRoots: []executionenv.RootBinding{{Path: readRoot, AccessCap: executionenv.RootAccessRead}, {Path: writeRoot, AccessCap: executionenv.RootAccessWrite}}}
	if !c.beginActive(owner.PersonID, active) {
		t.Fatal("test run was not active")
	}
	defer c.endActiveRun(owner.PersonID, active)
	facts, err := c.parallelExecutionContext(ctx, owner, "host-review-run")
	if err != nil || len(facts) != 1 || len(facts[0].Directories) != 2 || facts[0].Directories[0] != readRoot || facts[0].Directories[1] != writeRoot {
		t.Fatalf("judge context lost actual active directories: %+v %v", facts, err)
	}
	// The caller's own paths are not another Run's parallel risk facts.
	own, err := c.parallelExecutionContext(ctx, owner, active.RunID)
	if err != nil || len(own) != 0 {
		t.Fatalf("own Run was described as another: %+v %v", own, err)
	}
}
