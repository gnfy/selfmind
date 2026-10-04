package httpapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
)

func TestMultipleActiveRunsRequireExactControl(t *testing.T) {
	d, _, identity, _, _ := newApprovalTestServer(t)
	coord := d.coordinator()
	coord.activeLimit = 2
	a := &activeRun{TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: "run_alpha", Channel: "cli-a", StartedAt: time.Now()}
	b := &activeRun{TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: "run_beta", Channel: "cli-b", StartedAt: time.Now().Add(time.Second)}
	if !coord.beginActive(identity.PersonID, a) || !coord.beginActive(identity.PersonID, b) {
		t.Fatal("two slots were not admitted")
	}
	defer coord.endActiveRun(identity.PersonID, a)
	defer coord.endActiveRun(identity.PersonID, b)
	if coord.currentActive(identity.PersonID) != nil || coord.stopActive(identity.PersonID) != nil {
		t.Fatal("ambiguous person-wide control selected a run")
	}
	if got := coord.activeForRun(identity.PersonID, "run_alpha"); got == nil || got.Channel != "cli-a" {
		t.Fatalf("exact run lookup=%+v", got)
	}
	if coord.activeForRun(identity.PersonID, "run_missing") != nil || coord.activeForChannel(identity.PersonID, "cli-c") != nil {
		t.Fatal("unknown target fell back to another run")
	}
	if got := coord.activeForChannel(identity.PersonID, "cli-b"); got == nil || got.RunID != "run_beta" {
		t.Fatalf("channel focus lookup=%+v", got)
	}
	status, err := d.statusReply(context.Background(), identity)
	if err != nil || !strings.Contains(status, "2 runs active") || !strings.Contains(status, "run_alpha") || !strings.Contains(status, "run_beta") {
		t.Fatalf("ambiguous status=%q err=%v", status, err)
	}
	coord.endActiveRun(identity.PersonID, a)
	if got := coord.currentActive(identity.PersonID); got == nil || got.RunID != "run_beta" {
		t.Fatalf("ending one slot removed or changed the other: %+v", got)
	}
}

func TestConfigureWorkRunCapacityRequiresMatchingWorkersAndIdleRegistry(t *testing.T) {
	d := &Server{}
	for _, tc := range []struct {
		limit, workers int
		valid          bool
	}{
		{0, 3, false}, {4, 4, false}, {2, 1, false}, {3, 2, false},
		{1, 1, true}, {2, 2, true}, {3, 3, true},
	} {
		err := d.ConfigureWorkRunCapacity(tc.limit, tc.workers)
		if (err == nil) != tc.valid {
			t.Fatalf("capacity %d workers %d: err=%v", tc.limit, tc.workers, err)
		}
		if tc.valid && d.coordinator().activeCapacity() != tc.limit {
			t.Fatalf("capacity = %d, want %d", d.coordinator().activeCapacity(), tc.limit)
		}
	}
	d.coordinator().active["person"] = map[*activeRun]struct{}{}
	d.coordinator().active["person"][&activeRun{}] = struct{}{}
	if err := d.ConfigureWorkRunCapacity(1, 1); err == nil {
		t.Fatal("changed admission capacity while a Run was active")
	}
}

func TestOneCLIChannelCannotOccupyTwoPersonSlots(t *testing.T) {
	d, _, identity, _, _ := newApprovalTestServer(t)
	coord := d.coordinator()
	coord.activeLimit = 3
	a := &activeRun{PersonID: identity.PersonID, Platform: "cli", Channel: "session-a", RunID: "run-a"}
	if !coord.beginActive(identity.PersonID, a) {
		t.Fatal("first session was refused")
	}
	defer coord.endActiveRun(identity.PersonID, a)
	if coord.beginActive(identity.PersonID, &activeRun{PersonID: identity.PersonID, Platform: "cli", Channel: "session-a", RunID: "run-b"}) {
		t.Fatal("same terminal admitted a second foreground Run")
	}
	b := &activeRun{PersonID: identity.PersonID, Platform: "cli", Channel: "session-b", RunID: "run-b"}
	if !coord.beginActive(identity.PersonID, b) {
		t.Fatal("independent terminal did not use another slot")
	}
	defer coord.endActiveRun(identity.PersonID, b)
	if queuedResourcesReady(control.QueuedTask{Platform: "cli", Channel: "session-a", ExecutionRoots: []executionenv.RootBinding{{Path: "/different", AccessCap: executionenv.RootAccessWrite}}}, []*activeRun{a}) {
		t.Fatal("queue considered same CLI session runnable in a second slot")
	}
}
