package httpapi

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
)

func TestInboundDiagnosticIsPersonScopedAndDoesNotReplay(t *testing.T) {
	ctx := context.Background()
	store := controltest.NewStore(t)
	t.Cleanup(func() { _ = store.Close() })
	alice, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "bob", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginInbound(ctx, "telegram", "alice-msg", []byte(`{"content":"Alice secret"}`),
		control.InboundOwner{TenantID: alice.TenantID, PersonID: alice.PersonID, Preview: "Alice secret"}); err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimInbound(ctx, "telegram", "alice-msg"); err != nil || !claimed {
		t.Fatalf("claim = %t, %v", claimed, err)
	}
	daemon := &Server{Control: store}
	visible, err := daemon.inboundDiagReply(ctx, alice)
	if err != nil || !strings.Contains(visible, "dispatching") || !strings.Contains(visible, "telegram/") || strings.Contains(visible, "Alice secret") {
		t.Fatalf("owner diagnostic = %q, %v", visible, err)
	}
	hidden, err := daemon.inboundDiagReply(ctx, bob)
	if err != nil || strings.Contains(hidden, "Alice secret") || !strings.Contains(hidden, "No unresolved") {
		t.Fatalf("other person's diagnostic = %q, %v", hidden, err)
	}
}
