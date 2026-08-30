package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const providerSystemProxyCacheTTL = 30 * time.Second

type systemProxyMode uint8

const (
	systemProxyUnavailable systemProxyMode = iota
	systemProxyDirect
	systemProxyConfigured
	systemProxyUnsupported
)

type systemProxySnapshot struct {
	mode       systemProxyMode
	httpProxy  *url.URL
	httpsProxy *url.URL
	socksProxy *url.URL
	bypass     []string
	detail     string
}

type systemProxyLookup func(context.Context) systemProxySnapshot

// ProviderNetworkStatus is a credential-free snapshot of the route most
// recently selected for provider traffic. It is suitable for diagnostics and
// user-facing recovery guidance.
type ProviderNetworkStatus struct {
	Mode         string
	Source       string
	Endpoint     string
	Detail       string
	Reachability string
	Fingerprint  string
	Generation   uint64
	UpdatedAt    time.Time
}

// providerNetworkRouter is the single deep module for provider egress. The
// HTTP adapters know only its Proxy interface; platform lookup, precedence,
// cache invalidation, redaction, and route diagnostics remain local here.
type providerNetworkRouter struct {
	mu sync.Mutex

	managedMacOS bool
	envProxy     func(*http.Request) (*url.URL, error)
	systemLookup systemProxyLookup
	cacheTTL     time.Duration
	now          func() time.Time

	systemSnapshot systemProxySnapshot
	systemExpires  time.Time
	status         ProviderNetworkStatus
	lastTarget     *url.URL
}

func newProviderNetworkRouter(
	managedMacOS bool,
	envProxy func(*http.Request) (*url.URL, error),
	lookup systemProxyLookup,
) *providerNetworkRouter {
	if envProxy == nil {
		envProxy = http.ProxyFromEnvironment
	}
	return &providerNetworkRouter{
		managedMacOS: managedMacOS,
		envProxy:     envProxy,
		systemLookup: lookup,
		cacheTTL:     providerSystemProxyCacheTTL,
		now:          time.Now,
	}
}

func newProcessProviderNetworkRouter() *providerNetworkRouter {
	managedMacOS := strings.EqualFold(strings.TrimSpace(os.Getenv("SELFMIND_SERVICE_MANAGER")), "launchd")
	return newProviderNetworkRouter(managedMacOS, http.ProxyFromEnvironment, platformSystemProxyLookup())
}

var providerNetwork = newProcessProviderNetworkRouter()

func (r *providerNetworkRouter) Proxy(req *http.Request) (*url.URL, error) {
	if req == nil || req.URL == nil {
		return nil, nil
	}
	r.rememberTarget(req.URL)

	// A shell-launched process treats an explicit environment proxy as an
	// operator decision. A managed macOS daemon instead follows the current OS
	// route so a stale launchd snapshot cannot pin a laptop to yesterday's
	// network. When no environment proxy exists, on-demand macOS daemons may
	// still use the system route.
	if !r.managedMacOS {
		proxyURL, err := r.envProxy(req)
		if err != nil {
			return nil, err
		}
		if proxyURL != nil {
			r.record("proxy", "environment", proxyURL, "")
			return proxyURL, nil
		}
	}

	if r.systemLookup != nil {
		snapshot := r.currentSystemSnapshot(req.Context())
		switch snapshot.mode {
		case systemProxyDirect:
			r.record("direct", "system", nil, snapshot.detail)
			return nil, nil
		case systemProxyConfigured:
			if systemProxyBypasses(req.URL, snapshot.bypass) {
				r.record("direct", "system", nil, "system proxy bypass")
				return nil, nil
			}
			proxyURL := systemProxyForURL(req.URL, snapshot)
			if proxyURL == nil {
				r.record("direct", "system", nil, snapshot.detail)
				return nil, nil
			}
			r.record("proxy", "system", proxyURL, snapshot.detail)
			return proxyURL, nil
		case systemProxyUnsupported:
			r.record("blocked", "system", nil, snapshot.detail)
			return nil, fmt.Errorf("%s; use Clash System Proxy/TUN or disable automatic proxy configuration", snapshot.detail)
		}
	}

	proxyURL, err := r.envProxy(req)
	if err != nil {
		return nil, err
	}
	if proxyURL != nil {
		r.record("proxy", "environment", proxyURL, "system proxy lookup unavailable")
		return proxyURL, nil
	}
	r.record("direct", "host", nil, "")
	return nil, nil
}

func (r *providerNetworkRouter) currentSystemSnapshot(ctx context.Context) systemProxySnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if now.Before(r.systemExpires) {
		return r.systemSnapshot
	}
	snapshot := r.systemLookup(ctx)
	r.systemSnapshot = snapshot
	r.systemExpires = now.Add(r.cacheTTL)
	return snapshot
}

func (r *providerNetworkRouter) invalidate() {
	r.mu.Lock()
	r.systemExpires = time.Time{}
	r.mu.Unlock()
}

func (r *providerNetworkRouter) record(mode, source string, proxyURL *url.URL, detail string) {
	endpoint := ""
	if proxyURL != nil {
		endpoint = proxyURL.Host
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status.Mode != mode || r.status.Source != source || r.status.Endpoint != endpoint {
		r.status.Reachability = ""
	}
	signature := strings.Join([]string{mode, source, endpoint, detail, r.status.Reachability}, "\x00")
	previous := strings.Join([]string{r.status.Mode, r.status.Source, r.status.Endpoint, r.status.Detail, r.status.Reachability}, "\x00")
	if r.status.Generation == 0 || signature != previous {
		r.status.Generation++
	}
	r.status.Mode = mode
	r.status.Source = source
	r.status.Endpoint = endpoint
	r.status.Detail = detail
	r.status.Fingerprint = routeFingerprint(signature)
	r.status.UpdatedAt = now
}

func (r *providerNetworkRouter) rememberTarget(target *url.URL) {
	if target == nil {
		return
	}
	copy := &url.URL{Scheme: target.Scheme, Host: target.Host}
	r.mu.Lock()
	r.lastTarget = copy
	r.mu.Unlock()
}

func (r *providerNetworkRouter) refreshAndObserve(ctx context.Context) (ProviderNetworkStatus, bool) {
	r.mu.Lock()
	before := r.status.Fingerprint
	target := r.lastTarget
	r.systemExpires = time.Time{}
	r.mu.Unlock()
	if target == nil {
		return r.snapshot(), false
	}
	req := (&http.Request{URL: target}).WithContext(ctx)
	_, _ = r.Proxy(req)
	r.observeLoopbackProxy(ctx)
	after := r.snapshot()
	return after, before != "" && after.Fingerprint != "" && before != after.Fingerprint
}

func (r *providerNetworkRouter) observeLoopbackProxy(ctx context.Context) {
	status := r.snapshot()
	if status.Mode != "proxy" || status.Endpoint == "" || !loopbackEndpoint(status.Endpoint) {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(probeCtx, "tcp", status.Endpoint)
	if err == nil {
		_ = conn.Close()
		r.setReachability("reachable")
		return
	}
	r.setReachability("unreachable")
}

func loopbackEndpoint(endpoint string) bool {
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		return false
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (r *providerNetworkRouter) setReachability(value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status.Reachability == value {
		return
	}
	r.status.Reachability = value
	signature := strings.Join([]string{
		r.status.Mode, r.status.Source, r.status.Endpoint, r.status.Detail, r.status.Reachability,
	}, "\x00")
	r.status.Generation++
	r.status.Fingerprint = routeFingerprint(signature)
	r.status.UpdatedAt = r.now()
}

func routeFingerprint(signature string) string {
	sum := sha256.Sum256([]byte(signature))
	return hex.EncodeToString(sum[:8])
}

func (r *providerNetworkRouter) snapshot() ProviderNetworkStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

func systemProxyForURL(target *url.URL, snapshot systemProxySnapshot) *url.URL {
	if target == nil {
		return nil
	}
	switch strings.ToLower(target.Scheme) {
	case "https", "wss":
		if snapshot.httpsProxy != nil {
			return snapshot.httpsProxy
		}
	case "http", "ws":
		if snapshot.httpProxy != nil {
			return snapshot.httpProxy
		}
	}
	return snapshot.socksProxy
}

func systemProxyBypasses(target *url.URL, rules []string) bool {
	if target == nil {
		return true
	}
	host := strings.ToLower(strings.TrimSpace(target.Hostname()))
	if host == "" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	for _, raw := range rules {
		rule := strings.ToLower(strings.TrimSpace(raw))
		switch {
		case rule == "":
			continue
		case rule == "<local>":
			if !strings.Contains(host, ".") {
				return true
			}
		case strings.Contains(rule, "/"):
			if ip := net.ParseIP(host); ip != nil {
				if _, network, err := net.ParseCIDR(rule); err == nil && network.Contains(ip) {
					return true
				}
			}
		case strings.HasPrefix(rule, "*."):
			if strings.HasSuffix(host, rule[1:]) {
				return true
			}
		case strings.HasPrefix(rule, "."):
			if strings.HasSuffix(host, rule) || host == strings.TrimPrefix(rule, ".") {
				return true
			}
		case host == rule:
			return true
		}
	}
	return false
}

// RefreshProviderNetworkRoute invalidates platform route discovery and drops
// idle provider connections. It does not authorize direct fallback; the next
// request must resolve a fresh route from the same policy sources.
func RefreshProviderNetworkRoute() {
	providerNetwork.invalidate()
	if transport, ok := sharedProviderClient.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
}

// RefreshProviderNetworkRouteAfterError refreshes only connection-level
// failures that a changed network/proxy route can repair. Provider 429/5xx and
// semantic stream errors keep their ordinary retry path without spawning a
// platform proxy lookup.
func RefreshProviderNetworkRouteAfterError(err error) bool {
	if IsProviderNetworkError(err) {
		status := providerNetwork.snapshot()
		if status.Mode == "proxy" && loopbackEndpoint(status.Endpoint) && isLocalProxyReachabilityFailure(err) {
			providerNetwork.setReachability("unreachable")
		}
		RefreshProviderNetworkRoute()
		return true
	}
	return false
}

func isLocalProxyReachabilityFailure(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "connection refused") ||
		(strings.Contains(lower, "proxyconnect") && strings.Contains(lower, "connect:"))
}

func IsProviderNetworkError(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	for _, marker := range []string{
		"proxyconnect", "connection refused", "connection reset", "network is unreachable",
		"no route to host", "dial tcp", "dial udp", "lookup ",
		"context deadline exceeded", "client.timeout exceeded",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// RefreshAndObserveProviderNetworkRoute re-resolves the last provider target
// without contacting the model endpoint. It may perform a bounded loopback TCP
// probe for a configured local proxy. Background governance uses the returned
// change signal to release only jobs parked on an obsolete network route.
func RefreshAndObserveProviderNetworkRoute(ctx context.Context) (ProviderNetworkStatus, bool) {
	status, changed := providerNetwork.refreshAndObserve(ctx)
	if changed {
		if transport, ok := sharedProviderClient.Transport.(*http.Transport); ok {
			transport.CloseIdleConnections()
		}
	}
	return status, changed
}

func CurrentProviderNetworkStatus() ProviderNetworkStatus {
	return providerNetwork.snapshot()
}

// ActionableProviderNetworkError adds a concrete recovery path for the
// selected direct or proxy route. The original error remains wrapped for
// retry and policy classification.
func ActionableProviderNetworkError(err error) error {
	if err == nil {
		return nil
	}
	status := CurrentProviderNetworkStatus()
	if !IsProviderNetworkError(err) {
		return err
	}
	if status.Mode == "proxy" && status.Endpoint != "" && status.Source == "system" {
		return fmt.Errorf("%w; macOS currently routes model traffic through proxy %s, but it is not accepting connections; start Clash or disable its System Proxy setting—SelfMind will retry automatically after the route changes", err, status.Endpoint)
	}
	if status.Mode == "proxy" && status.Endpoint != "" {
		return fmt.Errorf("%w; the Gateway environment routes model traffic through proxy %s, but that route is unavailable; start the proxy or run `selfmind env refresh --restart` from the network you want to use", err, status.Endpoint)
	}
	if status.Source == "system" {
		return fmt.Errorf("%w; macOS is currently using a direct model connection; restore direct network access, or start Clash and enable System Proxy/TUN—SelfMind will retry automatically after the route changes", err)
	}
	return fmt.Errorf("%w; SelfMind is currently using a direct model connection; restore direct network access, or configure HTTPS_PROXY/use a system TUN and run `selfmind env refresh --restart`", err)
}
