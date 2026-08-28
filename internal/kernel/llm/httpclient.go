package llm

import (
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// tcpKeepAlive is the probe interval for the shared provider HTTP client's
// sockets. When a provider (notably the codex Responses backend) silently drops
// a connection mid-response, a keepalive probe surfaces the dead socket fast
// instead of leaving a read blocked until an OS-level timeout. This is the
// network-level complement to the SSE idle watchdog in responses_adapter.go.
const tcpKeepAlive = 30 * time.Second

var sharedProviderClient = &http.Client{Transport: newProviderTransport()}

// newProviderTransport clones the process default transport and enables TCP
// keepalive on the dialer. Cloning deliberately preserves
// http.ProxyFromEnvironment: a proxy configured for the current process is
// honored, while an absent proxy leaves routing to the host network. Callers
// that need protocol tweaks (e.g. the Kimi HTTP/1.1-only path) clone from here
// so keepalive stays consistent. It does not set an overall client Timeout:
// streaming responses outlive any fixed deadline, and liveness is bounded by
// the request context plus the SSE idle watchdog.
func newProviderTransport() *http.Transport {
	var t *http.Transport
	if base, ok := http.DefaultTransport.(*http.Transport); ok && base != nil {
		t = base.Clone()
	} else {
		t = &http.Transport{}
	}
	t.DialContext = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: tcpKeepAlive,
	}).DialContext
	return t
}

// ProviderHTTPClient returns the shared HTTP client for provider calls. It has
// TCP keepalive enabled so dead sockets surface quickly for the retry loop.
func ProviderHTTPClient() *http.Client { return sharedProviderClient }

// configuredStreamIdle is a process-wide default (nanoseconds) for the SSE idle
// watchdog, set once from config at startup via SetStreamIdleTimeout. It is
// deployment configuration, not per-request/per-tenant state; the env override
// and the built-in default still apply when it is unset.
var configuredStreamIdle atomic.Int64

// SetStreamIdleTimeout configures the default SSE idle watchdog timeout used by
// streaming adapters. A non-positive value clears the override (built-in
// default applies). The SELFMIND_STREAM_IDLE_TIMEOUT env var still wins.
func SetStreamIdleTimeout(d time.Duration) {
	if d > 0 {
		configuredStreamIdle.Store(int64(d))
	} else {
		configuredStreamIdle.Store(0)
	}
}
