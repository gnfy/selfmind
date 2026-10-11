package client

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/httpapi"
)

// The TUI is a daemon client: every request it makes travels on the local
// control socket and claims the CLI platform. This drives the real Client, with
// the transport the CLI hands it, against the real handler, so that a request
// the daemon would refuse (a missing JSON content type, an identity it cannot
// resolve, a stream it will not serve) fails here instead of in a terminal.
func TestTUIClientWorksAgainstTheRealHandlerOverTheLocalControlSocket(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	t.Setenv("SELFMIND_CLI_USER_ID", "tui-person")

	store := controltest.NewStore(t)
	identity, err := store.ResolveOrCreateAccount(context.Background(), "default", "cli", "tui-person", "TUI person")
	if err != nil {
		t.Fatal(err)
	}
	daemon := &httpapi.Server{Control: store, DefaultTenantID: "default"}

	directory, err := os.MkdirTemp("/tmp", "smtui")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(directory) })
	socket := filepath.Join(directory, "control.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{
		Handler: daemon.Handler(),
		// The verdict the daemon's listener gives the person's own CLI.
		ConnContext: func(ctx context.Context, _ net.Conn) context.Context {
			return httpapi.WithLocalPeer(ctx, httpapi.LocalPeer{Socket: true, Verified: true, PID: 4242})
		},
	}
	go server.Serve(listener)
	t.Cleanup(func() { _ = server.Close() })

	c := New("http://127.0.0.1:1", "")
	c.HTTP = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "unix", socket)
	}}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	began := time.Now()
	step := func(name string) { t.Logf("%6dms %s", time.Since(began).Milliseconds(), name) }
	refused := func(what string, err error) {
		t.Helper()
		if err != nil && (strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "415") ||
			strings.Contains(err.Error(), "requires the person's own selfmind CLI")) {
			t.Fatalf("%s was refused by the daemon: %v", what, err)
		}
	}

	// Chat and slash controls.
	response, status := c.ProcessMessageDetached(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "tui-person", Channel: "tui-1", Content: "/mode",
	})
	if status != http.StatusOK || !strings.Contains(response.Content, "Approval mode") {
		t.Fatalf("a slash control: status=%d resp=%+v", status, response)
	}
	if response.Identity == nil || response.Identity.PersonID != identity.PersonID {
		t.Fatalf("the TUI must act as the person who owns cli:tui-person, got %+v", response.Identity)
	}

	step("slash control done")
	// Attach digest and presence.
	digest, err := c.Digest(ctx)
	if err != nil || digest == nil {
		t.Fatalf("digest: %v", err)
	}
	if err := c.PingPresence(ctx); err != nil {
		t.Fatalf("presence ping: %v", err)
	}

	step("digest and presence done")
	// Approval, steering and dispatch reach their handlers; their own answers
	// (nothing pending, nothing to steer, no agent here) are not refusals.
	refused("an approval response", c.RespondApproval("1", "approve", "", ""))
	if err := c.RespondApproval("1", "approve", "", ""); err == nil || !strings.Contains(strings.ToLower(err.Error()), "approval") {
		t.Fatalf("an approval response with nothing pending must say so, got %v", err)
	}
	refused("a steer", c.SteerRun("run_missing", "tui-1", "go on", ""))
	_, err = c.Dispatch("skill_manage", map[string]interface{}{"action": "list"})
	refused("a dispatch", err)

	step("approval, steer, dispatch done")
	// The live event stream: a person-wide notice published by the daemon
	// reaches the attached TUI over the socket.
	var mu sync.Mutex
	var seen []string
	watchCtx, stopWatching := context.WithCancel(ctx)
	defer stopWatching()
	go c.WatchEvents(watchCtx, "default", "tui-1", nil, func(event api.RunEvent) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, event.Type)
	})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		daemon.PublishBackgroundNotice("daemon says hello", false)
		time.Sleep(150 * time.Millisecond)
		mu.Lock()
		got := append([]string(nil), seen...)
		mu.Unlock()
		for _, kind := range got {
			if kind == "background.notice" {
				step("notice delivered")
				return
			}
		}
	}
	t.Fatal("the TUI's event stream never delivered the daemon's notice over the local control socket")
}
