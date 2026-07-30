package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/tools"
)

// TestToolApprovalHandlerPublishesDecisionContext pins what the daemon publishes
// with a human ask. The TUI builds its panel from the approval.requested event
// and IM surfaces read the persisted row, so anything missing HERE is invisible
// at decision time — which is exactly how "approve a command without seeing
// where it runs" happened.
func TestToolApprovalHandlerPublishesDecisionContext(t *testing.T) {
	srv, store, identity, task, _ := newApprovalTestServer(t)
	coordinator := &RunCoordinator{srv: srv}
	ctx := context.Background()

	handler := coordinator.toolApprovalHandler(identity, task, nil, "cli")
	decided := make(chan tools.ToolApprovalDecision, 1)
	go func() {
		decision, err := handler(ctx, tools.ToolApprovalRequest{
			TenantID:      identity.TenantID,
			PersonID:      identity.PersonID,
			TaskID:        task.ID,
			ToolName:      "terminal",
			Reason:        "arbitrary code execution requires approval in smart mode",
			Args:          map[string]interface{}{"command": "python3 scripts/report.py"},
			GrantClass:    `"python3" commands`,
			Environment:   "envsnap_1_789c4317",
			Cwd:           "/workspace/app",
			ChangeSummary: "1 file +12/-0",
			TriageState:   tools.TriageStateUnavailable,
		})
		if err != nil {
			t.Errorf("approval handler: %v", err)
		}
		decided <- decision
	}()

	pending := waitForPendingApproval(t, store, identity)

	// 1. The persisted row carries the context, so /approvals and IM can use it.
	var payload approvalPayload
	if err := json.Unmarshal(pending.Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Cwd != "/workspace/app" || payload.Environment != "envsnap_1_789c4317" {
		t.Fatalf("row payload lost the location: %+v", payload)
	}
	if payload.ChangeSummary != "1 file +12/-0" {
		t.Fatalf("row payload ChangeSummary = %q", payload.ChangeSummary)
	}
	if payload.TriageState != tools.TriageStateUnavailable {
		t.Fatalf("row payload TriageState = %q", payload.TriageState)
	}

	// 2. The event carries the same context: it is the TUI's only source.
	event := findApprovalRequestedEvent(t, store, task.ID)
	var eventPayload map[string]interface{}
	if err := json.Unmarshal(event.Payload, &eventPayload); err != nil {
		t.Fatalf("decode event payload: %v", err)
	}
	for key, want := range map[string]string{
		"cwd":            "/workspace/app",
		"environment":    "envsnap_1_789c4317",
		"change_summary": "1 file +12/-0",
		"grant_class":    `"python3" commands`,
		"triage_state":   tools.TriageStateUnavailable,
	} {
		if got, _ := eventPayload[key].(string); got != want {
			t.Fatalf("event payload %s = %q, want %q", key, got, want)
		}
	}

	if _, err := store.RespondApprovalRequest(ctx, identity.TenantID, identity.PersonID, pending.ID, "approved", "cli", control.ApprovalDecisionInput{}); err != nil {
		t.Fatalf("respond: %v", err)
	}
	select {
	case decision := <-decided:
		if !decision.Approved {
			t.Fatal("handler should report the approval")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("handler did not observe the decision")
	}
}

// TestSmartTriageDiagLineDistinguishesBrokenFromStrict is the /diag half of the
// same question: a person drowning in prompts must be able to tell a strict
// judge from a judge that never ruled at all.
func TestSmartTriageDiagLineDistinguishesBrokenFromStrict(t *testing.T) {
	srv, store, identity, _, _ := newApprovalTestServer(t)
	ctx := context.Background()
	if err := store.SetPersonSetting(ctx, identity.TenantID, identity.PersonID, personSettingApprovalMode, "smart"); err != nil {
		t.Fatal(err)
	}

	// Nothing recorded yet: the line still reports the mode, because a persisted
	// /mode can outrank the product default indefinitely without announcing it.
	line := srv.smartTriageDiagLines(ctx, identity)
	if !strings.Contains(line, "[mode: smart]") || !strings.Contains(line, "no dangerous operation reached triage") {
		t.Fatalf("empty-window line = %q", line)
	}

	tools.RecordTriageOutcome(identity.TenantID, identity.PersonID, tools.TriageOutcomeUnavailable, errTriageProbe{})
	line = srv.smartTriageDiagLines(ctx, identity)
	if !strings.Contains(line, "unavailable 1") {
		t.Fatalf("line should count the unavailable triage: %q", line)
	}
	if !strings.Contains(line, "automatic triage is not ruling") {
		t.Fatalf("line should name the actionable case: %q", line)
	}
	if !strings.Contains(line, "probe failure") {
		t.Fatalf("line should surface the judge error: %q", line)
	}

	// With auto-approvals in the window the funnel is working, so the alarming
	// advisory must disappear.
	tools.RecordTriageOutcome(identity.TenantID, identity.PersonID, tools.TriageOutcomeApproved, nil)
	line = srv.smartTriageDiagLines(ctx, identity)
	if strings.Contains(line, "automatic triage is not ruling") {
		t.Fatalf("a working judge must not be reported as broken: %q", line)
	}
	if !strings.Contains(line, "auto-approved 1") {
		t.Fatalf("line should count auto-approvals: %q", line)
	}
}

type errTriageProbe struct{}

func (errTriageProbe) Error() string { return "probe failure: provider unreachable" }

func waitForPendingApproval(t *testing.T, store *control.Store, identity *control.IdentityContext) control.ApprovalRequest {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		pending, err := store.ListApprovalRequests(context.Background(), identity.TenantID, identity.PersonID, "pending", 10)
		if err != nil {
			t.Fatalf("list approvals: %v", err)
		}
		for _, approval := range pending {
			if len(approval.Payload) > 0 && strings.Contains(string(approval.Payload), "envsnap_1_789c4317") {
				return approval
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("approval row was never created")
	return control.ApprovalRequest{}
}

func findApprovalRequestedEvent(t *testing.T, store *control.Store, taskID string) control.Event {
	t.Helper()
	events, err := store.ListTaskEvents(context.Background(), taskID, 50)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	for _, event := range events {
		if event.Type == "approval.requested" {
			return event
		}
	}
	t.Fatal("approval.requested event was never appended")
	return control.Event{}
}
