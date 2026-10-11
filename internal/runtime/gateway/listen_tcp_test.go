package gateway

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"selfmind/internal/gateway/httpapi"
)

// The Host check must compare against the address the daemon actually listens
// on. With a configured port of 0 the string "127.0.0.1:0" names nothing, and
// checking against it would refuse every request.
func TestTheHostCheckFollowsTheListenersRealAddress(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	server := &httpapi.Server{}
	listener, err := listenTCP("127.0.0.1:0", server)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	if server.ListenAddr != listener.Addr().String() || port == 0 {
		t.Fatalf("ListenAddr = %q, listener = %q", server.ListenAddr, listener.Addr())
	}
	get := func(host string) int {
		request := httptest.NewRequest(http.MethodGet, "/v1/gateway/status", nil)
		request.Host = host
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		return recorder.Code
	}
	if code := get(net.JoinHostPort("127.0.0.1", strconv.Itoa(port))); code != http.StatusOK {
		t.Fatalf("the daemon's own address must be accepted on an OS-chosen port, got %d", code)
	}
	if code := get("rebind.attacker.example:" + strconv.Itoa(port)); code != http.StatusForbidden {
		t.Fatalf("a rebinding Host must still be refused, got %d", code)
	}

	// A wildcard listener reports its real wildcard address and checks nothing.
	wide := &httpapi.Server{}
	wideListener, err := listenTCP("0.0.0.0:0", wide)
	if err != nil {
		t.Fatal(err)
	}
	defer wideListener.Close()
	request := httptest.NewRequest(http.MethodGet, "/v1/gateway/status", nil)
	request.Host = "rebind.attacker.example:1"
	recorder := httptest.NewRecorder()
	wide.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("a daemon listening beyond loopback leaves the Host to its token, got %d", recorder.Code)
	}

	if _, err := listenTCP("not an address", &httpapi.Server{}); err == nil {
		t.Fatal("a bad address must fail")
	}
}
