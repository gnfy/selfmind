package httpapi

import (
	"context"
	"encoding/json"
	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"testing"
)

func TestFormalVerificationReportsObservedDispatch(t *testing.T) {
	for _, tc := range []struct{ name, facts, want string }{
		{"preparation refused", `"invoked":true,"process":{"started":false},"effect_state":"not_dispatched"`, "blocked"},
		{"admission refused", `"invoked":false`, "blocked"},
		{"test failed", `"invoked":true,"process":{"started":true,"exit_code":1}`, "failed"},
		{"exit unknown", `"invoked":true,"process":{"started":true}`, "blocked"},
		{"dispatch unknown", `"invoked":true`, "blocked"},
		{"historical failure", `"metadata":{}`, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := controltest.NewStore(t)
			defer store.Close()
			ctx := context.Background()
			identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "facts", "Facts")
			if err != nil {
				t.Fatal(err)
			}
			task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Verify", Channel: "cli"})
			if err != nil {
				t.Fatal(err)
			}
			run, err := store.StartRun(ctx, task, "cli", "verify")
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.AppendEvent(ctx, control.Event{RunID: run.ID, Type: "evidence.recorded", Payload: json.RawMessage(`{"evidence":{"tool_call_id":"check","tool_name":"verify","kind":"verification","status":"failed","command":{"command":"go test ./...","exit_code":-1},` + tc.facts + `}}`)})
			if err != nil {
				t.Fatal(err)
			}
			got, _ := (&Server{Control: store}).coordinator().evidenceOutcome(ctx, identity.TenantID, run.ID)
			if got.State != tc.want {
				t.Fatalf("state=%s want=%s checks=%+v", got.State, tc.want, got.Checks)
			}
		})
	}
}
