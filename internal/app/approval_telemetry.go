package app

import (
	"context"
	"encoding/json"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/tools"
)

// InstallApprovalTelemetry connects the production approval funnel to its owned
// store. The daemon and isolated smart evals share this adapter and cleanup.
func InstallApprovalTelemetry(controlStore *control.Store) func() {
	return tools.SetTriageTelemetrySink(func(event tools.TriageAuditEvent) {
		// Diagnostics are best-effort and must never turn a busy SQLite writer
		// into approval latency on the foreground tool path.
		writeCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()
		if event.Response.Version > 0 && event.RunID != "" {
			payload, marshalErr := json.Marshal(map[string]interface{}{"tool": event.ToolName, "outcome": event.Outcome, "response": event.Response})
			if marshalErr == nil {
				_, _ = controlStore.AppendEvent(writeCtx, control.Event{TaskID: event.TaskID, RunID: event.RunID, Type: "approval.response", Visibility: "task", Payload: payload})
			}
		}
		_ = controlStore.RecordApprovalTriageAudit(writeCtx, control.ApprovalTriageEvent{
			TenantID: event.TenantID, PersonID: event.PersonID, TaskID: event.TaskID, RunID: event.RunID,
			ToolName: event.ToolName, Outcome: string(event.Outcome), RiskLevel: event.RiskLevel,
			UserAuthorization: event.Authorization, GrantKey: event.GrantKey, ProviderRoute: event.ProviderRoute,
			LatencyMS: event.Latency.Milliseconds(), ErrorClass: event.ErrorClass, PolicyVersion: event.PolicyVersion,
			Rationale: event.Rationale, LastError: event.RedactedError, At: event.At,
		})
	})
}
