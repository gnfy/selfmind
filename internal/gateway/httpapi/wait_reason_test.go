package httpapi

import (
	"testing"

	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/router"
)

func TestStructuredWaitKeepsRuntimeCause(t *testing.T) {
	for _, reason := range []string{"external_resource_wait", "provider_wait", "approval_parked"} {
		status := "waiting_external"
		if reason == "approval_parked" {
			status = "waiting_user"
		}
		got := reconcileTurnCompletion(api.RunOutcome{Status: status, CompletionReason: reason}, router.TurnCompletion{Status: "completed", CompletionReason: "completed"}, true)
		if got.Status != status || got.CompletionReason != reason || got.Resumable {
			t.Fatalf("cause %s lost at finalization: %+v", reason, got)
		}
	}
}
