package control

import (
	"context"
	"testing"
	"time"
)

func TestApprovalTriageEventsPersistAndPrune(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	for _, event := range []struct {
		outcome string
		err     string
		at      time.Time
	}{
		{outcome: "contained", at: now.Add(-time.Hour)},
		{outcome: "human_ask", at: now.Add(-30 * time.Minute)},
		{outcome: "unavailable", err: "provider timeout", at: now.Add(-time.Minute)},
		{outcome: "approved", at: now.Add(-20 * 24 * time.Hour)},
	} {
		if err := store.RecordApprovalTriageEvent(ctx, "tenant", "person", event.outcome, event.err, event.at); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := store.ApprovalTriageStatsSince(ctx, "tenant", "person", now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if stats.Counts["contained"] != 1 || stats.Counts["human_ask"] != 1 || stats.Counts["unavailable"] != 1 || stats.Counts["approved"] != 0 {
		t.Fatalf("counts=%+v", stats.Counts)
	}
	if stats.LastError != "provider timeout" || stats.LastErrorAt.Unix() != now.Add(-time.Minute).Unix() {
		t.Fatalf("last error=%q at=%s", stats.LastError, stats.LastErrorAt)
	}
	pruned, err := store.PruneApprovalTriageEvents(ctx, now.Add(-14*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 1 {
		t.Fatalf("pruned=%d want 1", pruned)
	}
}

func TestApprovalTriageAuditPersistsStructuredEvidenceWithoutCommandText(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().Truncate(time.Second)
	if err := store.RecordApprovalTriageAudit(context.Background(), ApprovalTriageEvent{
		TenantID: "tenant", PersonID: "person", TaskID: "task-1", RunID: "run-1", ToolName: "terminal",
		Outcome: "escalated", RiskLevel: "medium", UserAuthorization: "unknown", GrantKey: "exec:observe",
		ProviderRoute: "fast_classifier", LatencyMS: 742, ErrorClass: "", PolicyVersion: "smart-v2",
		Rationale: "The operation needs a person.", At: now,
	}); err != nil {
		t.Fatal(err)
	}
	var taskID, runID, toolName, risk, auth, route, policy, rationale string
	var latency int64
	err = store.db.QueryRowContext(context.Background(), `SELECT task_id, run_id, tool_name, risk_level,
		user_authorization, provider_route, latency_ms, policy_version, rationale
		FROM approval_triage_events WHERE tenant_id = 'tenant' AND person_id = 'person'`).Scan(
		&taskID, &runID, &toolName, &risk, &auth, &route, &latency, &policy, &rationale)
	if err != nil {
		t.Fatal(err)
	}
	if taskID != "task-1" || runID != "run-1" || toolName != "terminal" || risk != "medium" || auth != "unknown" || route != "fast_classifier" || latency != 742 || policy != "smart-v2" || rationale == "" {
		t.Fatalf("audit fields = %q %q %q %q %q %q %d %q %q", taskID, runID, toolName, risk, auth, route, latency, policy, rationale)
	}
}
