package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/command"
)

// invocationForm returns a switch-acceptable invocation of a registry gateway
// command. /resume and /workspace require an argument to reach their switch
// case; the rest are handled bare.
func invocationForm(name string) string {
	switch name {
	case "/resume":
		return "/resume tsk_nonexistent"
	case "/workspace":
		return "/workspace ws_nonexistent"
	default:
		return name
	}
}

// TestEveryRegistryGatewayCommandIsHandledBySwitch is the registry↔switch drift
// guard: every Gateway-scope command in the shared registry must be handled by
// Server.tryHandleControlCommand (never fall through to the "Unknown command"
// reject gate). Combined with command.TestKnownMatchesGatewayContract (which
// pins the reverse direction — every switch case has a registry entry), the two
// keep detection/help/async-hint from drifting from execution again.
func TestEveryRegistryGatewayCommandIsHandledBySwitch(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	daemon := &Server{Control: store, DefaultTenantID: "default"}
	ctx := context.Background()

	for _, name := range command.Known() {
		content := invocationForm(name)
		resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{Content: content})
		if status != http.StatusOK {
			t.Errorf("%q: status = %d, want 200", content, status)
		}
		if strings.Contains(resp.Content, "Unknown command") {
			t.Errorf("%q fell through to the unknown-command reject gate: %q", content, resp.Content)
		}
	}
}

// TestUnknownSlashStillRejected pins that the gateway reject gate is unchanged:
// an unrecognized slash is rejected (not dispatched to the agent), and a
// near-miss carries a suggestion via the shared registry.
func TestUnknownSlashStillRejected(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	daemon := &Server{Control: store, DefaultTenantID: "default"}
	ctx := context.Background()

	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{Content: "/qwer"})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(resp.Content, "Unknown command") {
		t.Fatalf("/qwer should be rejected, got %q", resp.Content)
	}

	resp, _ = daemon.ProcessMessage(ctx, api.MessageRequest{Content: "/approves"})
	if !strings.Contains(resp.Content, "did you mean /approve") {
		t.Fatalf("/approves should suggest /approve, got %q", resp.Content)
	}
}
