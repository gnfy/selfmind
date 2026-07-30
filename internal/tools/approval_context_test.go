package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestApprovalRequestCarriesExecutionContext pins the decision context that the
// human ask publishes: WHERE the operation would run (scope root + bound
// environment) and HOW LARGE the write is. Without these the person is asked to
// authorize a command whose location they cannot see.
func TestApprovalRequestCarriesExecutionContext(t *testing.T) {
	var got ToolApprovalRequest
	cleanup := SetExecutionScope("person-ctx", ExecutionScope{
		TenantID: "tenant-ctx", PersonID: "person-ctx", TaskID: "task-ctx",
		WorkspaceRoot:         "/workspace/app",
		EnvironmentSnapshotID: "envsnap_1_abcdef",
		ApprovalMode:          ApprovalReadOnly,
		Approval: func(ctx context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
			got = req
			return ToolApprovalDecision{Approved: true, ApprovalID: "apr_ctx"}, nil
		},
	})
	defer cleanup()

	exec := SmartApprovalMiddleware("")(func(args map[string]interface{}) (string, error) {
		return "ok", nil
	})
	if _, err := exec(map[string]interface{}{
		"_tenant_id": "person-ctx",
		"_tool_name": "write_file",
		"path":       "report.html",
		"content":    strings.Repeat("line\n", 30),
	}); err != nil {
		t.Fatalf("approved write_file should run: %v", err)
	}

	if got.Cwd != "/workspace/app" {
		t.Fatalf("Cwd = %q, want the scope root", got.Cwd)
	}
	if got.Environment != "envsnap_1_abcdef" {
		t.Fatalf("Environment = %q, want the bound snapshot id", got.Environment)
	}
	if got.ChangeSummary != "30 lines, 150 B" {
		t.Fatalf("ChangeSummary = %q", got.ChangeSummary)
	}
	// read-only mode asks by policy, not because triage failed.
	if got.TriageState != "" {
		t.Fatalf("TriageState = %q, want empty outside smart mode", got.TriageState)
	}
}

// TestApprovalDisplayCwdIgnoresArgs proves the displayed location comes from the
// authorizing scope, never from the model's own args: a call that could name its
// own cwd on the panel could make an out-of-workspace write look routine.
func TestApprovalDisplayCwdIgnoresArgs(t *testing.T) {
	scope := ExecutionScope{WorkspaceRoot: "/workspace/app"}
	if got := approvalDisplayCwd(scope); got != "/workspace/app" {
		t.Fatalf("approvalDisplayCwd = %q", got)
	}
	fallbackOnly := ExecutionScope{AllowedRoots: []string{"/srv/other/"}}
	if got := approvalDisplayCwd(fallbackOnly); got != "/srv/other" {
		t.Fatalf("approvalDisplayCwd(allowed roots) = %q", got)
	}
	if got := approvalDisplayCwd(ExecutionScope{}); got != "" {
		t.Fatalf("approvalDisplayCwd(empty) = %q, want empty", got)
	}
}

// TestSmartTriageUnavailableIsDistinctFromEscalation is the core of the "why am
// I being asked so much" fix: a judge that ERRORS and a judge that deliberately
// escalates both reach the human, but only the first means triage is broken.
func TestSmartTriageUnavailableIsDistinctFromEscalation(t *testing.T) {
	resetTriageTelemetryForTest(t)

	cases := []struct {
		name       string
		judge      ApprovalJudge
		wantState  string
		wantCounts TriageStats
	}{
		{
			name:       "judge errors",
			judge:      &fakeJudge{err: errors.New("provider unreachable")},
			wantState:  TriageStateUnavailable,
			wantCounts: TriageStats{Unavailable: 1},
		},
		{
			name:       "judge escalates",
			judge:      &fakeJudge{reply: "ESCALATE"},
			wantState:  TriageStateEscalated,
			wantCounts: TriageStats{Escalated: 1},
		},
		{
			name:       "no judge configured",
			judge:      nil,
			wantState:  TriageStateUnavailable,
			wantCounts: TriageStats{Unavailable: 1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetTriageTelemetryForTest(t)
			var got ToolApprovalRequest
			cleanup := SetExecutionScope("person-tri", ExecutionScope{
				TenantID: "tenant-tri", PersonID: "person-tri", TaskID: "task-tri",
				ApprovalMode: ApprovalSmart, Grants: newFakeGrantStore(), Judge: tc.judge,
				Approval: func(ctx context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
					got = req
					return ToolApprovalDecision{Approved: true, ApprovalID: "apr_tri"}, nil
				},
			})
			defer cleanup()

			exec := SmartApprovalMiddleware("")(func(args map[string]interface{}) (string, error) {
				return "ok", nil
			})
			if _, err := exec(map[string]interface{}{
				"_tenant_id": "person-tri",
				"_tool_name": "terminal",
				"command":    "ls -la",
			}); err != nil {
				t.Fatalf("approved terminal call should run: %v", err)
			}
			if got.TriageState != tc.wantState {
				t.Fatalf("TriageState = %q, want %q", got.TriageState, tc.wantState)
			}
			stats := TriageDiagnostics("tenant-tri", "person-tri")
			if stats.Approved != tc.wantCounts.Approved || stats.Denied != tc.wantCounts.Denied ||
				stats.Escalated != tc.wantCounts.Escalated || stats.Unavailable != tc.wantCounts.Unavailable {
				t.Fatalf("TriageDiagnostics = %+v, want %+v", stats, tc.wantCounts)
			}
			if tc.wantState == TriageStateUnavailable && tc.judge != nil && stats.LastError == "" {
				t.Fatal("a judge failure must be reportable in diagnostics")
			}
		})
	}
}

// TestTriageDiagnosticsPartitionsAndAges proves counts never leak across people
// and never report outside the window.
func TestTriageDiagnosticsPartitionsAndAges(t *testing.T) {
	resetTriageTelemetryForTest(t)
	now := time.Now()
	triageRecords.nowFn = func() time.Time { return now }

	RecordTriageOutcome("tenant-a", "person-a", TriageOutcomeApproved, nil)
	RecordTriageOutcome("tenant-b", "person-b", TriageOutcomeDenied, nil)
	if got := TriageDiagnostics("tenant-a", "person-a"); got.Approved != 1 || got.Denied != 0 {
		t.Fatalf("person-a stats = %+v, want only its own approval", got)
	}
	if got := TriageDiagnostics("tenant-b", "person-b"); got.Denied != 1 || got.Approved != 0 {
		t.Fatalf("person-b stats = %+v", got)
	}

	// Age the recorded entries past the window.
	triageRecords.nowFn = func() time.Time { return now.Add(triageWindow + time.Minute) }
	if got := TriageDiagnostics("tenant-a", "person-a"); got.Total() != 0 {
		t.Fatalf("aged-out entries must not report: %+v", got)
	}
}

// TestApprovalChangeSummaryCountsWithoutContent pins that the summary carries
// counts only — never patch or file content, which approval payloads redact.
func TestApprovalChangeSummaryCountsWithoutContent(t *testing.T) {
	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: internal/app/main.go",
		"@@ func main @@",
		" keep",
		"-secretRemovedLine",
		"+addedLine",
		"+addedLine2",
		"*** Add File: docs/new.md",
		"+fresh",
		"*** End Patch",
	}, "\n")
	got := ApprovalChangeSummary("patch", map[string]interface{}{"patch": patch})
	if got != "2 files +3/-1" {
		t.Fatalf("ApprovalChangeSummary(patch) = %q", got)
	}
	if strings.Contains(got, "secretRemovedLine") || strings.Contains(got, "addedLine") {
		t.Fatalf("summary must not carry content: %q", got)
	}
	if got := ApprovalChangeSummary("write_file", map[string]interface{}{"content": ""}); got != "empty file" {
		t.Fatalf("empty write summary = %q", got)
	}
	if got := ApprovalChangeSummary("terminal", map[string]interface{}{"command": "ls"}); got != "" {
		t.Fatalf("non-write tools have no change summary, got %q", got)
	}
}

// resetTriageTelemetryForTest clears the process-wide triage read model and
// restores the clock, so counts never leak between tests.
func resetTriageTelemetryForTest(t *testing.T) {
	t.Helper()
	triageRecords.mu.Lock()
	triageRecords.byKey = map[string][]triageEntry{}
	triageRecords.nowFn = nil
	triageRecords.mu.Unlock()
	t.Cleanup(func() {
		triageRecords.mu.Lock()
		triageRecords.byKey = map[string][]triageEntry{}
		triageRecords.nowFn = nil
		triageRecords.mu.Unlock()
	})
}
