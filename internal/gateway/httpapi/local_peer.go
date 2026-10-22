package httpapi

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"strings"
	"sync"

	"selfmind/internal/control"
)

// Local control authority.
//
// The CLI platform is the person's own terminal. A request may act with CLI
// identity only when it arrived on the daemon's local control socket from a
// peer the daemon verified from kernel credentials: the daemon owner's own
// process outside the daemon's process tree. Nothing a request carries counts
// toward that verdict: not a platform field, a gateway bearer token, or the
// retired local-control token header. TCP never grants CLI identity, so a
// tool that reaches the TCP listener, or a remote client holding the gateway
// token, can act only as the non-CLI identity it names.

// LocalPeer is the daemon's verdict on the process at the other end of a
// local control socket connection. The listener attaches it to the
// connection; nothing in a request can set or change it.
type LocalPeer struct {
	// Socket is true when the request arrived on the local control socket.
	Socket bool
	// Verified is true only for the daemon owner's own process outside the
	// daemon's process tree: the person's own CLI.
	Verified bool
	UID      int
	PID      int
	// Refusal explains why a socket peer was not verified.
	Refusal string
}

type localPeerKey struct{}

// WithLocalPeer attaches an already-known verdict to a connection context.
func WithLocalPeer(ctx context.Context, peer LocalPeer) context.Context {
	return context.WithValue(ctx, localPeerKey{}, func() LocalPeer { return peer })
}

// WithLazyLocalPeer attaches a verdict that is taken the first time a request
// needs it and then reused for the rest of the connection. The listener uses it
// because the verdict reads the peer's kernel credentials, which exist only
// while the peer is connected: taking it at accept time judges a probe that
// connected and left, and wastes a lookup on traffic that never claims CLI
// identity. A request that does claim it is judged while its client is still
// waiting for the answer.
func WithLazyLocalPeer(ctx context.Context, resolve func() LocalPeer) context.Context {
	var once sync.Once
	var peer LocalPeer
	return context.WithValue(ctx, localPeerKey{}, func() LocalPeer {
		once.Do(func() { peer = resolve() })
		return peer
	})
}

func requestLocalPeer(r *http.Request) LocalPeer {
	if r == nil {
		return LocalPeer{}
	}
	resolve, _ := r.Context().Value(localPeerKey{}).(func() LocalPeer)
	if resolve == nil {
		return LocalPeer{}
	}
	return resolve()
}

// cliClaim reports whether a request names the CLI platform. The control
// store resolves an empty platform to cli, so an omitted platform is a claim.
func cliClaim(platform string) bool {
	platform = strings.TrimSpace(platform)
	return platform == "" || strings.EqualFold(platform, "cli")
}

// localCLIRequest reports whether the request is the person's own verified
// CLI and claims the CLI platform.
func (d *Server) localCLIRequest(r *http.Request, platform string) bool {
	return cliClaim(platform) && requestLocalPeer(r).Verified
}

// localControlRefusal is a request refused because it claims CLI identity or
// local-control authority without being the verified local CLI.
type localControlRefusal struct{ message string }

func (e *localControlRefusal) Error() string { return e.message }

// newLocalControlRefusal explains a refused CLI claim and what to do about it. A
// request over TCP was made by a CLI that took this daemon for a remote
// gateway, or by something that is not a CLI at all: either way the person
// needs the settings that decide which it is. The socket's path is not in the
// answer, which goes to a caller that has not proved it may know it; the CLI
// derives the path and shows it to the person.
func (d *Server) newLocalControlRefusal(r *http.Request, what string) error {
	peer := requestLocalPeer(r)
	socket := "the daemon's local control socket"
	if peer.Socket && !peer.Verified {
		d.refusals.warn(refusalKey("cli", peer, r.RemoteAddr), "gateway: a peer claiming CLI identity was refused",
			"pid", peer.PID, "uid", peer.UID, "reason", peer.Refusal)
		return &localControlRefusal{message: what + " requires the person's own selfmind CLI on " + socket +
			"; this peer was refused: " + peer.Refusal + ". Run selfmind from your own terminal, not from a tool the agent started"}
	}
	return &localControlRefusal{message: what + " requires the person's own selfmind CLI on " + socket +
		"; requests over TCP never carry CLI identity. If this is your own selfmind, it took this daemon for a remote gateway: " +
		"gateway.url and SELF_GATEWAY_URL must name this machine (127.0.0.1, localhost, or the wildcard address the daemon binds), " +
		"and the CLI must be run from your own terminal"}
}

// requireLocalCLI writes the refusal and returns false unless the request is
// the verified local CLI.
func (d *Server) requireLocalCLI(w http.ResponseWriter, r *http.Request, what string) bool {
	if requestLocalPeer(r).Verified {
		return true
	}
	writeError(w, http.StatusForbidden, d.newLocalControlRefusal(r, what))
	return false
}

// resolveRequestIdentity is the only way an HTTP handler resolves its caller.
// It refuses a CLI claim that is not the verified local CLI before any
// account is resolved or created.
func (d *Server) resolveRequestIdentity(r *http.Request, tenantID, platform, platformUserID, displayName string) (*control.IdentityContext, error) {
	if cliClaim(platform) && !requestLocalPeer(r).Verified {
		return nil, d.newLocalControlRefusal(r, "CLI identity")
	}
	return d.Control.ResolveOrCreateAccount(r.Context(), tenantID, platform, platformUserID, displayName)
}

// writeIdentityError maps a refused claim to 403 and anything else to 500.
func writeIdentityError(w http.ResponseWriter, err error) {
	var refusal *localControlRefusal
	if errors.As(err, &refusal) {
		writeError(w, http.StatusForbidden, err)
		return
	}
	writeError(w, http.StatusInternalServerError, err)
}

// rejectBrowserRequests refuses what a web page can send to a loopback
// listener. The daemon has no browser client, so no Origin is allowed and a
// request a browser marks with Sec-Fetch-Site is refused on every route except
// the health probe. A state change must declare a JSON body: a cross-origin
// page can post a form or text/plain without a CORS preflight, but not JSON.
// IM webhooks keep the body type their platform sends; their Origin check
// still applies.
func rejectBrowserRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		// A browser adds Sec-Fetch-Site to every request, including a same-origin
		// GET after DNS rebinding that carries no Origin. Sec-Fetch-Mode alone is
		// not a browser marker: Node's fetch sends it on every request.
		if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "" {
			http.Error(w, "browser-originated requests are not accepted", http.StatusForbidden)
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if !strings.HasPrefix(r.URL.Path, "/v1/im/") && !jsonContentType(r.Header.Get("Content-Type")) {
				http.Error(w, "state-changing requests must send Content-Type: application/json", http.StatusUnsupportedMediaType)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func jsonContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && mediaType == "application/json"
}
