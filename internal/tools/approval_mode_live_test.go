package tools

// Live approval-mode lookup: the mode is resolved PER ASK via
// ExecutionScope.ModeGetter, not frozen into the scope at run start, so a
// /mode change from any endpoint (e.g. IM `/mode smart` mid-run) governs the
// in-flight run's later approval decisions. These tests pin that contract at
// the middleware layer; the gateway closure is pinned in
// httpapi/mode_command_test.go.

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// liveModeJudge is a recording fake for the smart-mode triage step.
type liveModeJudge struct {
	mu     sync.Mutex
	reply  string
	called int
}

func (j *liveModeJudge) Judge(ctx context.Context, prompt string) (string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.called++
	return j.reply, nil
}

func (j *liveModeJudge) calls() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.called
}

// TestModeGetterGovernsEachAsk proves a mid-run mode flip changes behavior on
// the NEXT dangerous op: on-request asks a human, full-auto bypasses, and
// smart engages the LLM triage — all through the same installed scope.
func TestModeGetterGovernsEachAsk(t *testing.T) {
	var (
		modeMu sync.Mutex
		mode   = ApprovalOnRequest
	)
	setMode := func(m ApprovalMode) { modeMu.Lock(); mode = m; modeMu.Unlock() }

	approvalCalls := 0
	judge := &liveModeJudge{reply: "APPROVE"}
	cleanup := SetExecutionScope("person-live", ExecutionScope{
		TenantID: "tenant-live",
		PersonID: "person-live",
		// Static snapshot deliberately DIFFERENT from the live mode so a
		// regression to the snapshot is caught immediately.
		ApprovalMode: ApprovalFullAuto,
		ModeGetter: func() ApprovalMode {
			modeMu.Lock()
			defer modeMu.Unlock()
			return mode
		},
		Judge: judge,
		Approval: func(ctx context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
			approvalCalls++
			return ToolApprovalDecision{Approved: true}, nil
		},
	})
	defer cleanup()

	ran := 0
	exec := SmartApprovalMiddleware("")(func(args map[string]interface{}) (string, error) {
		ran++
		return "ran", nil
	})
	dangerousOp := map[string]interface{}{
		"_tenant_id": "person-live",
		"_tool_name": "terminal",
		"command":    "chmod 777 script.sh",
	}

	// 1. Live mode on-request: the human ask fires despite the full-auto snapshot.
	if _, err := exec(dangerousOp); err != nil {
		t.Fatalf("approved op should run: %v", err)
	}
	if approvalCalls != 1 || ran != 1 || judge.calls() != 0 {
		t.Fatalf("on-request must reach the human ask: approvals=%d ran=%d judge=%d", approvalCalls, ran, judge.calls())
	}

	// 2. Flip to full-auto mid-run: the next op bypasses the ask.
	setMode(ApprovalFullAuto)
	if _, err := exec(dangerousOp); err != nil {
		t.Fatalf("full-auto op should run: %v", err)
	}
	if approvalCalls != 1 || ran != 2 {
		t.Fatalf("full-auto must bypass the ask: approvals=%d ran=%d", approvalCalls, ran)
	}

	// 3. Flip to smart mid-run: the next op goes through the LLM triage
	//    (APPROVE auto-runs) instead of on-request's unconditional human ask.
	setMode(ApprovalSmart)
	if _, err := exec(dangerousOp); err != nil {
		t.Fatalf("triage-approved op should run: %v", err)
	}
	if judge.calls() != 1 || approvalCalls != 1 || ran != 3 {
		t.Fatalf("smart must engage triage on the next ask: judge=%d approvals=%d ran=%d", judge.calls(), approvalCalls, ran)
	}

	// 4. A DENY verdict blocks with the user-rejection contract.
	judge.mu.Lock()
	judge.reply = "DENY"
	judge.mu.Unlock()
	_, err := exec(map[string]interface{}{
		"_tenant_id": "person-live",
		"_tool_name": "terminal",
		"command":    "rm -rf build",
	})
	if err == nil || !strings.Contains(err.Error(), "operation rejected") {
		t.Fatalf("smart DENY must block with the rejection contract, got: %v", err)
	}
}

// TestModeGetterEmptyFallsBackToSnapshot: a getter returning "" defers to the
// static ApprovalMode, and a nil getter keeps the old snapshot behavior.
func TestModeGetterEmptyFallsBackToSnapshot(t *testing.T) {
	cleanup := SetExecutionScope("person-fb", ExecutionScope{
		TenantID:     "tenant-fb",
		PersonID:     "person-fb",
		ApprovalMode: ApprovalFullAuto,
		ModeGetter:   func() ApprovalMode { return "" },
		Approval: func(ctx context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
			t.Fatalf("full-auto fallback must not ask")
			return ToolApprovalDecision{}, nil
		},
	})
	defer cleanup()

	exec := SmartApprovalMiddleware("")(func(args map[string]interface{}) (string, error) { return "ran", nil })
	if _, err := exec(map[string]interface{}{
		"_tenant_id": "person-fb",
		"_tool_name": "terminal",
		"command":    "chmod 777 script.sh",
	}); err != nil {
		t.Fatalf("empty getter must fall back to the full-auto snapshot: %v", err)
	}
}
