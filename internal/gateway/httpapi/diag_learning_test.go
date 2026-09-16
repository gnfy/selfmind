package httpapi

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
)

func TestLearningDiagnosticsExplainEmptyEvidenceAndShadowWithoutWriting(t *testing.T) {
	ctx := context.Background()
	store := controltest.NewStore(t)
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "learning-diag", "Learning")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Control: store, MemoryConsolidator: &testMemoryConsolidator{}}
	handled, reply, _, err := server.tryHandleControlCommand(ctx, identity, api.MessageRequest{Channel: "cli", Content: "/diag learning"})
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	for _, want := range []string{"Selection window: 0 / 48", "No workflow observations", "managed activations: 0", "curator configured: false", "Governance mode: shadow", "without automatic merge/archive", "preference intake and explicit memory writes remain separate", "Governance scheduler: not initialized"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("missing %q: %s", want, reply)
		}
	}
	if _, ok, err := store.MemoryGovernanceScheduleForPerson(ctx, identity.TenantID, identity.PersonID); err != nil || ok {
		t.Fatalf("diagnosis initialized scheduler: %v %v", ok, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reply, err = server.learningDiagReply(ctx, identity)
	if err != nil || !strings.Contains(reply, "unavailable") || strings.Contains(reply, "Selection window: 0") {
		t.Fatalf("query failure presented as no evidence: %q %v", reply, err)
	}
}
