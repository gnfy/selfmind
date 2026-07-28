package tools

import (
	"context"
	"strings"
	"testing"
)

// TestHardlineToolCall pins the unbypassable deny set. These patterns must be
// caught regardless of approval mode; the middleware test below proves the
// full-auto bypass cannot reach them.
func TestHardlineToolCall(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	cases := []struct {
		name string
		cmd  string
		want bool
	}{
		{"rm rf root", "rm -rf /", true},
		{"rm rf root glob", "rm -rf /*", true},
		{"rm rf home tree", "rm -rf /home", true},
		{"rm rf HOME var", "rm -rf $HOME", true},
		{"rm rf expanded home", "rm -rf /home/tester", true},
		{"rm rf etc", "rm -r /etc", true},
		{"rm rf usr chained", "make clean && rm -rf /usr", true},
		{"rm no-preserve-root", "rm --no-preserve-root -rf /", true},
		{"mkfs", "mkfs.ext4 /dev/sda1", true},
		{"dd to disk", "dd if=/dev/zero of=/dev/sda bs=1M", true},
		{"dd to nvme", "dd if=/dev/zero of=/dev/nvme0n1", true},
		{"fork bomb", ":(){ :|:& };:", true},
		{"fork bomb spaced", ": ( ) { :|:& };:", true},
		{"shutdown", "shutdown -h now", true},
		{"reboot", "reboot", true},
		{"halt", "/sbin/halt", true},
		{"init 0", "init 0", true},
		{"init 6", "init 6", true},
		{"redirect to device", "echo x > /dev/sda", true},
		// Not hardline: dangerous but legitimate; stays behind normal approval.
		{"rm project subdir", "rm -rf build", false},
		{"rm relative dist", "rm -rf ./dist", false},
		{"rm non-recursive root file", "rm /etc/hosts", false},
		{"safe build", "go build ./...", false},
		{"init service", "init restart", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := hardlineToolCall("", "terminal", map[string]interface{}{"command": tc.cmd})
			if got != tc.want {
				t.Fatalf("hardlineToolCall(%q) = %v (reason %q), want %v", tc.cmd, got, reason, tc.want)
			}
		})
	}
}

// TestHardlineBlocksEvenFullAuto proves layer 1 fires before the mode bypass:
// full-auto never asks for approval, yet a hard-floor command must still be
// blocked and the tool must never run.
func TestHardlineBlocksEvenFullAuto(t *testing.T) {
	cleanup := SetExecutionScope("person-hl", ExecutionScope{
		TenantID:     "tenant-hl",
		PersonID:     "person-hl",
		ApprovalMode: ApprovalFullAuto,
		Approval: func(ctx context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
			t.Fatalf("approval must not be consulted for a hard-floor op")
			return ToolApprovalDecision{}, nil
		},
	})
	defer cleanup()

	called := false
	exec := SmartApprovalMiddleware("")(func(args map[string]interface{}) (string, error) {
		called = true
		return "ran", nil
	})
	_, err := exec(map[string]interface{}{
		"_tenant_id": "person-hl",
		"_tool_name": "terminal",
		"command":    "rm -rf /",
	})
	if err == nil {
		t.Fatalf("hard-floor op must be blocked even under full-auto")
	}
	if called {
		t.Fatalf("tool ran despite hard-floor block")
	}
	// Distinct wording from the user-rejection contract: a hard block, not a
	// user decision (kernel isUserRejectionErr must not match — asserted in a
	// kernel test).
	if !strings.Contains(err.Error(), "blocked by safety policy") {
		t.Fatalf("error should read as a safety-policy block, got: %v", err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "operation rejected") ||
		strings.Contains(strings.ToLower(err.Error()), "operation cancelled by user") {
		t.Fatalf("hard block must not reuse the user-rejection contract strings: %v", err)
	}
}

// TestApprovalPatternKeyStability pins the key point of the funnel: two
// different chmod commands are the SAME class, so approving one covers the
// other; a different dangerous binary (rm) is a different class.
func TestApprovalPatternKeyStability(t *testing.T) {
	_, r1 := dangerousToolCall("", "terminal", map[string]interface{}{"command": "chmod 777 a.sh"})
	_, r2 := dangerousToolCall("", "terminal", map[string]interface{}{"command": "chmod +x /some/other/file"})
	k1 := approvalPatternKey("terminal", map[string]interface{}{"command": "chmod 777 a.sh"}, r1)
	k2 := approvalPatternKey("terminal", map[string]interface{}{"command": "chmod +x /some/other/file"}, r2)
	if k1 != k2 {
		t.Fatalf("two chmod commands must key to the same class: %q vs %q", k1, k2)
	}
	if k1 != "exec:invokes dangerous command: chmod" {
		t.Fatalf("unexpected chmod class key: %q", k1)
	}

	_, rr := dangerousToolCall("", "terminal", map[string]interface{}{"command": "rm -rf build"})
	kr := approvalPatternKey("terminal", map[string]interface{}{"command": "rm -rf build"}, rr)
	if kr == k1 {
		t.Fatalf("rm and chmod must be different classes, both %q", kr)
	}

	// write/path tools bucket by tool + reason class (path stripped).
	kw1 := approvalPatternKey("write_file", nil, "accesses restricted path: /etc/hosts")
	kw2 := approvalPatternKey("write_file", nil, "accesses restricted path: /etc/passwd")
	if kw1 != kw2 {
		t.Fatalf("same restricted-path class must key equally: %q vs %q", kw1, kw2)
	}
}

func TestHostApprovalPatternIsScopedToWorkspaceAndCommandFamily(t *testing.T) {
	args := map[string]interface{}{"command": "curl https://example.com", "sandbox": "host"}
	_, reason := dangerousToolCall("", "terminal", args)
	scopeA := ExecutionScope{WorkspaceID: "ws-a", WorkspaceRoot: "/work/a"}
	scopeB := ExecutionScope{WorkspaceID: "ws-b", WorkspaceRoot: "/work/b"}

	keyA := approvalPatternKeyForScope("terminal", args, reason, scopeA, true)
	keyARepeat := approvalPatternKeyForScope("terminal", args, reason, scopeA, true)
	keyB := approvalPatternKeyForScope("terminal", args, reason, scopeB, true)
	if keyA == "" || keyA != keyARepeat {
		t.Fatalf("workspace host key must be stable, got %q and %q", keyA, keyARepeat)
	}
	if keyA == keyB {
		t.Fatalf("host approval leaked across workspaces: %q", keyA)
	}
	if !strings.Contains(keyA, "command:curl") {
		t.Fatalf("host key must retain the effective command family: %q", keyA)
	}
	if got := approvalPatternKeyForScope("terminal", args, reason, ExecutionScope{}, false); got != "" {
		t.Fatalf("host request without a durable scope must not be remembered: %q", got)
	}
}

func TestHostClassificationDoesNotHideInnerDangerousClass(t *testing.T) {
	args := map[string]interface{}{"command": "chmod +x release.sh", "sandbox": "host"}
	dangerous, reason := dangerousToolCall("", "terminal", args)
	if !dangerous || reason != "invokes dangerous command: chmod" {
		t.Fatalf("host classification hid inner command: dangerous=%v reason=%q", dangerous, reason)
	}
}

// fakeGrantStore is an in-memory tools.ApprovalGrantStore for the grant tests.
type fakeGrantStore struct {
	granted map[string]bool
}

func newFakeGrantStore() *fakeGrantStore { return &fakeGrantStore{granted: map[string]bool{}} }

func (f *fakeGrantStore) key(kind, scopeID, pk string) string { return kind + "|" + scopeID + "|" + pk }

func (f *fakeGrantStore) IsApprovalGranted(ctx context.Context, tenantID, personID, taskID, patternKey string) (bool, error) {
	if f.granted[f.key("person", personID, patternKey)] {
		return true, nil
	}
	if taskID != "" && f.granted[f.key("task", taskID, patternKey)] {
		return true, nil
	}
	return false, nil
}

func (f *fakeGrantStore) GrantApproval(ctx context.Context, scopeKind, tenantID, personID, scopeID, patternKey string) error {
	f.granted[f.key(scopeKind, scopeID, patternKey)] = true
	return nil
}

// TestTaskGrantSuppressesSameClassWithinTaskOnly: a "remember for this task"
// approval suppresses the next same-class ask in the SAME task, but a different
// task still asks.
func TestTaskGrantSuppressesSameClassWithinTaskOnly(t *testing.T) {
	withExecSandboxPolicy(t, true, true, false)
	store := newFakeGrantStore()
	asks := 0
	handler := func(ctx context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
		asks++
		return ToolApprovalDecision{Approved: true, ApprovalID: "apr", Scope: "task"}, nil
	}
	install := func(taskID string) func() {
		return SetExecutionScope("person-g", ExecutionScope{
			TenantID: "tenant-g", PersonID: "person-g", TaskID: taskID,
			ApprovalMode: ApprovalOnRequest, Approval: handler, Grants: store,
		})
	}
	run := func(cmd string) {
		exec := SmartApprovalMiddleware("")(func(args map[string]interface{}) (string, error) { return "ok", nil })
		if _, err := exec(map[string]interface{}{"_tenant_id": "person-g", "_tool_name": "terminal", "command": cmd}); err != nil {
			t.Fatalf("exec %q: %v", cmd, err)
		}
	}

	cleanup := install("task-1")
	run("chmod 777 a") // asks -> 1, grants task-1
	run("chmod +x b")  // same class, task-1 granted -> no ask
	if asks != 1 {
		t.Fatalf("task grant should suppress the 2nd same-class ask in the same task; asks=%d", asks)
	}
	cleanup()

	cleanup = install("task-2")
	run("chmod 700 c") // different task -> not granted -> asks -> 2
	if asks != 2 {
		t.Fatalf("task grant must NOT carry to a different task; asks=%d", asks)
	}
	cleanup()
}

// TestPersonGrantSuppressesAcrossTasks: a "remember for me" approval suppresses
// the same class in a different task too.
func TestPersonGrantSuppressesAcrossTasks(t *testing.T) {
	withExecSandboxPolicy(t, true, true, false)
	store := newFakeGrantStore()
	asks := 0
	handler := func(ctx context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
		asks++
		return ToolApprovalDecision{Approved: true, ApprovalID: "apr", Scope: "person"}, nil
	}
	install := func(taskID string) func() {
		return SetExecutionScope("person-p", ExecutionScope{
			TenantID: "tenant-p", PersonID: "person-p", TaskID: taskID,
			ApprovalMode: ApprovalOnRequest, Approval: handler, Grants: store,
		})
	}
	run := func(cmd string) {
		exec := SmartApprovalMiddleware("")(func(args map[string]interface{}) (string, error) { return "ok", nil })
		if _, err := exec(map[string]interface{}{"_tenant_id": "person-p", "_tool_name": "terminal", "command": cmd}); err != nil {
			t.Fatalf("exec %q: %v", cmd, err)
		}
	}

	cleanup := install("task-1")
	run("chmod 777 a") // asks -> 1, grants person
	cleanup()

	cleanup = install("task-2")
	run("chmod +x b") // person grant covers all tasks -> no ask
	if asks != 1 {
		t.Fatalf("person grant should suppress the same class across tasks; asks=%d", asks)
	}
	cleanup()
}

// TestOnceApprovalGrantsNothing: a bare approve (Scope "") must not record any
// grant, so the next same-class call asks again.
func TestOnceApprovalGrantsNothing(t *testing.T) {
	store := newFakeGrantStore()
	asks := 0
	handler := func(ctx context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
		asks++
		return ToolApprovalDecision{Approved: true, ApprovalID: "apr", Scope: ""},
			nil
	}
	cleanup := SetExecutionScope("person-o", ExecutionScope{
		TenantID: "tenant-o", PersonID: "person-o", TaskID: "task-1",
		ApprovalMode: ApprovalOnRequest, Approval: handler, Grants: store,
	})
	defer cleanup()
	run := func(cmd string) {
		exec := SmartApprovalMiddleware("")(func(args map[string]interface{}) (string, error) { return "ok", nil })
		if _, err := exec(map[string]interface{}{"_tenant_id": "person-o", "_tool_name": "terminal", "command": cmd}); err != nil {
			t.Fatalf("exec %q: %v", cmd, err)
		}
	}
	run("chmod 777 a")
	run("chmod +x b")
	if asks != 2 {
		t.Fatalf("a once-only approval must not be remembered; asks=%d", asks)
	}
	if len(store.granted) != 0 {
		t.Fatalf("once-only approval must record no grant, got %d", len(store.granted))
	}
}
