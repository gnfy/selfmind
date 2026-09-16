package tools

import (
	"context"
	"testing"

	"selfmind/internal/kernel"
)

func TestVerifyPreservesFailedCommandBeforeSuccessfulOutput(t *testing.T) {
	for _, command := range []string{"false; echo misleading-success", "false | cat; echo misleading-success", "selfmind_missing_check_command; echo misleading-success"} {
		t.Run(command, func(t *testing.T) {
			events := make(chan string, 32)
			ctx := kernel.WithEventChannel(context.Background(), events)
			args := map[string]interface{}{"_context": ctx, "_tool_name": "verify", "command": command, "cwd": t.TempDir()}
			_, err := EvidenceMiddleware()(NewVerifyTool().ExecuteResult)(args)
			if err == nil {
				t.Fatal("a failed check was masked by a later successful command")
			}
			for len(events) > 0 {
				raw := <-events
				event, ok := kernel.DecodeAgentEvent(raw)
				if ok && event.Type == "evidence.recorded" {
					evidence := event.Payload["evidence"].(map[string]interface{})
					if evidence["status"] != "failed" {
						t.Fatalf("evidence=%v", evidence)
					}
					return
				}
			}
			t.Fatal("verification evidence missing")
		})
	}
}

func TestVerifyAllowsExplicitlyHandledFailure(t *testing.T) {
	_, err := NewVerifyTool().Execute(map[string]interface{}{"command": "if false; then exit 1; fi; printf handled", "cwd": t.TempDir()})
	if err != nil {
		t.Fatalf("explicitly handled failure rejected: %v", err)
	}
}
