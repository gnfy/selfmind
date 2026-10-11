package httpapi

import (
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"strings"

	"selfmind/internal/gateway/api"
)

// Daemon administration.
//
// Stopping the daemon and reading what it is doing are the person's own
// controls. With no gateway token configured the TCP listener is open to every
// local process, including a confined tool that reached it, so these routes
// answer only a caller that proves more than reaching a port: the verified
// local CLI on the control socket, or the holder of the gateway token (a remote
// operator). The one exception is the restart helper the daemon starts for a
// model change, which is a descendant of the daemon and therefore never the
// verified CLI: it presents a capability only the two of them know, good for
// stopping the daemon for that model change and for nothing else.

// RestartCapabilityHeader carries the restart helper's capability.
const RestartCapabilityHeader = "X-SelfMind-Restart-Capability"

// adminCaller reports whether the caller of an authorized request may administer
// the daemon: the person's own verified CLI, or a holder of the configured
// gateway token. Callers have already passed Server.authorized, so a configured
// token was presented.
func (d *Server) adminCaller(r *http.Request) bool {
	return requestLocalPeer(r).Verified || gatewayToken() != ""
}

// restartCapabilityHeld reports whether the request is the restart helper
// stopping the daemon for the model change it was started for.
func (d *Server) restartCapabilityHeld(r *http.Request, reason string) bool {
	if d.RestartCapability == "" {
		return false
	}
	if _, ok := modelChangeShutdownID(reason); !ok {
		return false
	}
	got := r.Header.Get(RestartCapabilityHeader)
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(d.RestartCapability)) == 1
}

// adminRefusal explains a refused administrative request and where to go
// instead: the person's own terminal, which reaches the control socket.
func (d *Server) adminRefusal(r *http.Request, what string) error {
	// Where the socket is stays out of the answer: this caller has not proved it may
	// know, and the CLI derives the path and shows it to the person.
	via := "the daemon's local control socket"
	peer := requestLocalPeer(r)
	// Refusing an attempt to stop or inspect the daemon is worth one line; allowing
	// the restart helper to, or answering a liveness probe, is not.
	d.refusals.warn(refusalKey("admin", peer, r.RemoteAddr), "gateway: a peer was refused daemon administration",
		"request", what, "socket", peer.Socket, "pid", peer.PID, "remote", r.RemoteAddr, "reason", peer.Refusal)
	if peer.Socket && !peer.Verified {
		return &localControlRefusal{message: what + " requires the person's own selfmind CLI on " + via +
			", or the gateway token; this peer was refused: " + peer.Refusal}
	}
	return &localControlRefusal{message: what + " over TCP requires the gateway token, and none is configured: " +
		"run the selfmind command from your own terminal, which reaches " + via + ", or set gateway.token"}
}

// livenessStatus is what a caller that may not administer the daemon learns: it
// is up, whether it is draining, how much work is running, and which build. Run
// summaries, identities, paths and diagnostics stay with the administrator.
func livenessStatus(status api.GatewayStatusResponse) api.GatewayStatusResponse {
	return api.GatewayStatusResponse{
		Runtime: api.GatewayRuntimeInfo{
			State:            status.Runtime.State,
			Version:          status.Runtime.Version,
			Commit:           status.Runtime.Commit,
			BuiltAt:          status.Runtime.BuiltAt,
			BuildFingerprint: status.Runtime.BuildFingerprint,
		},
		State:          status.State,
		Draining:       status.Draining,
		ActiveRuns:     []api.ActiveRunStatus{},
		ActiveRunCount: status.ActiveRunCount,
		LocalControl:   status.LocalControl,
		LivenessOnly:   true,
	}
}

// requireLoopbackHost refuses a TCP request whose Host does not name this
// loopback daemon. A page that points its own domain at 127.0.0.1 (DNS
// rebinding) is same-origin to the browser and sends no Origin, but its Host is
// still its own name. The check is off when the daemon listens beyond loopback,
// where the names a client may use are not known. A socket request has no
// browser behind it, /health says nothing, and a client that sends no Host
// (HTTP/1.0) cannot be a browser.
func (d *Server) requireLoopbackHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		port, loopback := loopbackListener(d.ListenAddr)
		if !loopback || r.URL.Path == "/health" || r.Host == "" || requestLocalPeer(r).Socket || hostNamesLoopback(r.Host, port) {
			next.ServeHTTP(w, r)
			return
		}
		writeError(w, http.StatusForbidden, errors.New("the Host header "+r.Host+" is not this daemon's loopback address "+
			net.JoinHostPort("127.0.0.1", port)+"; connect to it by that address or localhost"))
	})
}

// loopbackListener reports the port of a listener bound to loopback only.
func loopbackListener(addr string) (string, bool) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil || port == "" {
		return "", false
	}
	if strings.EqualFold(host, "localhost") {
		return port, true
	}
	ip := net.ParseIP(host)
	return port, ip != nil && ip.IsLoopback()
}

// hostNamesLoopback reports whether a Host header is localhost, 127.0.0.1 or
// [::1], on the listening port or none.
func hostNamesLoopback(host, listenPort string) bool {
	port := ""
	switch {
	case strings.HasPrefix(host, "["):
		end := strings.Index(host, "]")
		if end < 0 {
			return false
		}
		rest := host[end+1:]
		host = host[1:end]
		if rest != "" {
			if !strings.HasPrefix(rest, ":") {
				return false
			}
			port = rest[1:]
		}
	case strings.Count(host, ":") == 1:
		cut := strings.Index(host, ":")
		host, port = host[:cut], host[cut+1:]
	case strings.Contains(host, ":"):
		return false
	}
	if port != "" && port != listenPort {
		return false
	}
	host = strings.ToLower(host)
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
