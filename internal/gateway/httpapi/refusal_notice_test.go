package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"selfmind/internal/modelchange"
)

// A refusal has to be something the person can act on, and the daemon's log must
// say something only when something was refused.

func TestACLIClaimOverTCPIsRefusedWithWhereToGo(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	daemon := adminServer(nil)
	recorder := serveAs(t, daemon, tcpPeer, http.MethodPost, "/v1/message", `{"platform":"cli","content":"/id"}`)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	// A CLI that reached the daemon over TCP took it for a remote gateway, whatever
	// the person thinks: say where the socket is and which settings decide that.
	for _, want := range []string{"local control socket", "gateway.url", "SELF_GATEWAY_URL", "own terminal"} {
		if !strings.Contains(body.Error, want) {
			t.Fatalf("the refusal must name %q: %s", want, body.Error)
		}
	}
	// Not where it is: an unauthenticated caller does not learn the path.
	if strings.Contains(body.Error, adminSocketPath) || strings.Contains(body.Error, "/home/u") {
		t.Fatalf("the refusal must not disclose the socket path: %s", body.Error)
	}
	// A refused socket peer has a different problem and is told so.
	refused := serveAs(t, daemon, refusedPeer, http.MethodPost, "/v1/message", `{"platform":"cli","content":"/id"}`)
	if refused.Code != http.StatusForbidden || !strings.Contains(refused.Body.String(), refusedPeer.Refusal) ||
		!strings.Contains(refused.Body.String(), "own terminal") || strings.Contains(refused.Body.String(), adminSocketPath) {
		t.Fatalf("a refused peer must be told why and where to go: %s", refused.Body.String())
	}
}

func TestARefusedPeerIsWarnedAboutOnlyWhenItAsksForWhatItMayNotHave(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	service, _ := testModelChangeService(t)
	status, err := service.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	candidate := status.Running
	candidate.Primary.Model = "gpt-after-restart"
	prepared, err := service.Prepare(context.Background(), modelchange.PrepareRequest{Candidate: candidate, Source: "test", RequireConfirmation: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Confirm(context.Background(), prepared.Change.ID); err != nil {
		t.Fatal(err)
	}
	reason, _ := json.Marshal(map[string]string{"reason": "model_change:" + prepared.Change.ID})

	warnings := captureWarnings(t)
	stopped := make(chan struct{}, 1)
	daemon := adminServer(stopped)
	// The accepted HTTP response precedes durable draining. Join the shutdown
	// callback before TempDir cleanup can race its state-file writes.
	t.Cleanup(func() {
		select {
		case <-stopped:
		case <-time.After(2 * time.Second):
			t.Error("accepted model-change shutdown did not finish")
		}
	})
	daemon.ModelChanges = service
	daemon.RestartCapability = "cap-1234"
	clock := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	daemon.refusals.now = func() time.Time { return clock }

	// Reading liveness judges the peer but asks for nothing it may not have.
	if code := serveAs(t, daemon, refusedPeer, http.MethodGet, "/v1/gateway/status", "").Code; code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if n := warnings.lines("WARN"); n != 0 {
		t.Fatalf("a status read must not warn, got:\n%s", warnings.String())
	}

	// The restart helper stops the daemon for its own model change: a descendant of
	// the daemon, never the person's CLI, and entitled to do exactly this.
	helper := serveAs(t, daemon, refusedPeer, http.MethodPost, "/v1/gateway/shutdown", string(reason), "X-SelfMind-Restart-Capability", "cap-1234")
	if helper.Code != http.StatusAccepted {
		t.Fatalf("the helper must be accepted: %d %s", helper.Code, helper.Body.String())
	}
	if n := warnings.lines("WARN"); n != 0 {
		t.Fatalf("the capability-carrying helper must not be warned about, got:\n%s", warnings.String())
	}

	// A refused peer asking for CLI identity, and a refused peer asking to stop the
	// daemon without the capability, are each one warning that names the peer.
	serveAs(t, daemon, refusedPeer, http.MethodPost, "/v1/message", `{"platform":"cli","content":"/id"}`)
	if n := warnings.lines("WARN", "refused", "CLI identity"); n != 1 {
		t.Fatalf("a refused CLI claim must warn once, got %d:\n%s", n, warnings.String())
	}
	stop := serveAs(t, daemon, refusedPeer, http.MethodPost, "/v1/gateway/shutdown", `{}`)
	if stop.Code != http.StatusForbidden {
		t.Fatalf("a refused peer must not stop the daemon: %d", stop.Code)
	}
	if n := warnings.lines("WARN", "refused", "administration"); n != 1 {
		t.Fatalf("a refused stop must warn once, got %d:\n%s", n, warnings.String())
	}
	// The same for a wrong capability: it is a refusal like any other (once the
	// limit's interval has passed, the same peer is warned about again).
	clock = clock.Add(2 * time.Minute)
	serveAs(t, daemon, refusedPeer, http.MethodPost, "/v1/gateway/shutdown", string(reason), "X-SelfMind-Restart-Capability", "cap-0000")
	if n := warnings.lines("WARN", "refused", "administration"); n != 2 {
		t.Fatalf("a wrong capability must warn, got %d:\n%s", n, warnings.String())
	}
}
