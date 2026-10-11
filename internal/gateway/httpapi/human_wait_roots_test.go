package httpapi

import (
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
)

// Parked-root admission is exercised with real checkpoints and workers in
// TestParkedHumanWaitAdmitsNewWorkInSameWorkspace. This projection test pins
// that an exact answer must reacquire occupied roots rather than bypass them.
func TestHumanAnswerReacquiresRunningRoots(t *testing.T) {
	root := t.TempDir()
	roots := []executionenv.RootBinding{{Path: root, AccessCap: executionenv.RootAccessWrite}}
	for _, kind := range []string{control.LoopCheckpointWaitApproval, control.LoopCheckpointWaitClarification} {
		q := control.QueuedTask{Platform: "cli", Channel: "origin", ExecutionRoots: roots,
			ReplyToRunID: "parked", ApprovalID: "exact-approval", ClarifyID: "exact-question"}
		if !queuedResourcesReady(q, nil) {
			t.Fatalf("%s answer blocked without execution occupancy", kind)
		}
		occupied := []control.RunRootReservation{{RunID: "other", ExecutionRoots: roots}}
		if queuedResourcesReady(q, nil, occupied) {
			t.Fatalf("%s answer bypassed another Run's directory", kind)
		}
		independent := []control.RunRootReservation{{RunID: "other", ExecutionRoots: []executionenv.RootBinding{{Path: t.TempDir(), AccessCap: executionenv.RootAccessWrite}}}}
		if !queuedResourcesReady(q, nil, independent) {
			t.Fatalf("%s answer blocked on independent directory", kind)
		}
	}
}
