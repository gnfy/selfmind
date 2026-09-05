package httpapi

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
)

func TestModelsDiagReplySummarizesMaintenanceProviderCost(t *testing.T) {
	store := controltest.NewStore(t)
	if err := store.RecordMaintenanceProviderCall(context.Background(), control.MaintenanceProviderCall{
		TenantID: "tenant", Role: "maintenance_backup", Provider: "minimax", Model: "MiniMax-M2.7",
		Status: control.MaintenanceProviderCallSucceeded, InputTokens: 12500, OutputTokens: 800,
	}); err != nil {
		t.Fatal(err)
	}
	reply, err := (&Server{Control: store}).modelsDiagReply(context.Background(), &control.IdentityContext{TenantID: "tenant"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"minimax/MiniMax-M2.7", "maintenance_backup", "calls 1", "input 12.5K", "output 800"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply missing %q:\n%s", want, reply)
		}
	}
}
