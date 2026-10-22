package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// One warning per refused request lets any local process that can reach the
// listener fill gateway.log. The daemon warns once per peer and kind of refusal
// per interval, counts what it suppressed, and bounds the memory it spends on it.
// (A CLI claim over TCP is refused without a warning: an HTTP integration that
// names the wrong platform is not an event.)

func refuse(daemon *Server, peer LocalPeer, remote, method, path, body string) int {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = remote
	request = request.WithContext(WithLocalPeer(context.Background(), peer))
	recorder := httptest.NewRecorder()
	daemon.Handler().ServeHTTP(recorder, request)
	return recorder.Code
}

func TestRefusalWarningsAreRateLimitedPerPeerAndKind(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	warnings := captureWarnings(t)
	daemon := adminServer(nil)
	clock := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	daemon.refusals.now = func() time.Time { return clock }
	stop := func(peer LocalPeer, remote string) int {
		return refuse(daemon, peer, remote, http.MethodPost, "/v1/gateway/shutdown", `{}`)
	}
	claim := func(peer LocalPeer, remote string) int {
		return refuse(daemon, peer, remote, http.MethodPost, "/v1/message", `{"platform":"cli","content":"/id"}`)
	}

	// A looper that changes its source port on every request is one peer.
	for i := 0; i < 200; i++ {
		if code := stop(tcpPeer, fmt.Sprintf("127.0.0.1:%d", 40000+i)); code != http.StatusForbidden {
			t.Fatalf("a refused stop must still be refused, got %d", code)
		}
	}
	if n := warnings.lines("WARN", "administration"); n != 1 {
		t.Fatalf("200 refusals from one peer must warn once, got %d:\n%s", n, warnings.String())
	}

	// Another peer, and another kind of refusal, have their own.
	stop(tcpPeer, "127.0.0.2:5000")
	if n := warnings.lines("WARN", "administration"); n != 2 {
		t.Fatalf("a second peer must warn, got %d", n)
	}
	// A socket peer is told apart by its process, not by an address.
	one, two := LocalPeer{Socket: true, PID: 111, Refusal: "peer is the daemon"}, LocalPeer{Socket: true, PID: 222, Refusal: "peer is the daemon"}
	for i := 0; i < 20; i++ {
		claim(one, "@")
		claim(two, "@")
	}
	for _, pid := range []string{"pid=111", "pid=222"} {
		if n := warnings.lines("WARN", "CLI identity", pid); n != 1 {
			t.Fatalf("one warning per socket peer process, got %d for %s:\n%s", n, pid, warnings.String())
		}
	}
	// The same process asking for a different thing is a different refusal.
	stop(one, "@")
	if n := warnings.lines("WARN", "administration", "pid=111"); n != 1 {
		t.Fatalf("another kind of refusal from the same process warns once, got %d", n)
	}

	// After the interval the next refusal warns again and says how many it held back.
	clock = clock.Add(2 * time.Minute)
	stop(tcpPeer, "127.0.0.1:42000")
	if n := warnings.lines("WARN", "administration", "suppressed=199"); n != 1 {
		t.Fatalf("the warning after the interval must carry the suppressed count (199):\n%s", warnings.String())
	}
}

// The per-peer limit bounds what one peer writes, not what many peers write: a
// process that varies its address (or many processes) is a new peer each time. The
// daemon also holds a ceiling across all peers, and says once how much it held back.
func TestRefusalWarningsHaveAGlobalCeilingAcrossPeers(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	warnings := captureWarnings(t)
	daemon := adminServer(nil)
	clock := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	daemon.refusals.now = func() time.Time { return clock }
	stopFrom := func(i int) int {
		remote := fmt.Sprintf("10.%d.%d.%d:5000", i/65536%256, i/256%256, i%256)
		return refuse(daemon, tcpPeer, remote, http.MethodPost, "/v1/gateway/shutdown", `{}`)
	}

	// Every request from another address: each one is a peer never warned about.
	const flood = 500
	for i := 0; i < flood; i++ {
		if code := stopFrom(i); code != http.StatusForbidden {
			t.Fatalf("a refused stop must still be refused, got %d", code)
		}
	}
	if n := warnings.lines("WARN", "administration"); n != maxRefusalWarningsPerInterval {
		t.Fatalf("%d refusals from %d peers wrote %d warnings, the ceiling is %d",
			flood, flood, n, maxRefusalWarningsPerInterval)
	}
	if got := daemon.refusals.size(); got > maxRefusalWarningsPerInterval {
		t.Fatalf("peers whose warning was held back must not take table space: %d held", got)
	}
	if n := warnings.lines("WARN", "suppressed"); n != 0 {
		t.Fatalf("the summary is one line when the window ends, not during it:\n%s", warnings.String())
	}

	// When the window has passed the next refusal brings one summary line with the
	// number held back, and warnings flow again.
	clock = clock.Add(2 * refusalWarnInterval)
	stopFrom(flood)
	held := flood - maxRefusalWarningsPerInterval
	if n := warnings.lines("WARN", fmt.Sprintf("suppressed=%d", held)); n != 1 {
		t.Fatalf("expected one summary line with suppressed=%d, got %d:\n%s", held, n, warnings.String())
	}
	if n := warnings.lines("WARN", "suppressed"); n != 1 {
		t.Fatalf("exactly one summary line, got %d:\n%s", n, warnings.String())
	}
	if n := warnings.lines("WARN", "administration"); n != maxRefusalWarningsPerInterval+1 {
		t.Fatalf("the refusal after the window must warn: %d administration lines", n)
	}
}

func TestRefusalWarningMemoryIsBounded(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	captureWarnings(t)
	daemon := adminServer(nil)
	clock := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	daemon.refusals.now = func() time.Time { return clock }
	for i := 0; i < 5000; i++ {
		// Every request from a different address: the worst case for memory. The
		// clock moves so the ceiling across peers lets each one through and the
		// table, not the ceiling, is what is being bounded.
		clock = clock.Add(30 * time.Second)
		refuse(daemon, tcpPeer, fmt.Sprintf("10.%d.%d.%d:5000", i/65536%256, i/256%256, i%256), http.MethodPost, "/v1/gateway/shutdown", `{}`)
	}
	if got := daemon.refusals.size(); got > maxRefusalPeers {
		t.Fatalf("the refusal log holds %d peers, bound is %d", got, maxRefusalPeers)
	}
	// Still warning after the table filled: a new peer is not silenced forever.
	warnings := captureWarnings(t)
	clock = clock.Add(5 * time.Minute)
	refuse(daemon, tcpPeer, "192.0.2.1:5000", http.MethodPost, "/v1/gateway/shutdown", `{}`)
	if n := warnings.lines("WARN", "administration"); n != 1 {
		t.Fatalf("a refusal after the table filled must still warn, got %d", n)
	}
}
