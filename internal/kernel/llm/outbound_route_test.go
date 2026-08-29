package llm

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func proxyTestRequest(t *testing.T, rawURL string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func mustProxyURL(t *testing.T, rawURL string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestManagedMacOSSystemDirectOverridesStaleEnvironmentProxy(t *testing.T) {
	envProxy := mustProxyURL(t, "http://127.0.0.1:7897")
	router := newProviderNetworkRouter(true, func(*http.Request) (*url.URL, error) {
		return envProxy, nil
	}, func(context.Context) systemProxySnapshot {
		return systemProxySnapshot{mode: systemProxyDirect, detail: "macOS direct"}
	})

	got, err := router.Proxy(proxyTestRequest(t, "https://api.example.test/v1"))
	if err != nil || got != nil {
		t.Fatalf("managed route = %v, %v; want direct", got, err)
	}
	if status := router.snapshot(); status.Mode != "direct" || status.Source != "system" {
		t.Fatalf("status = %+v", status)
	}
}

func TestManagedMacOSFollowsSystemProxyWhenClashStarts(t *testing.T) {
	proxyURL := mustProxyURL(t, "http://127.0.0.1:7897")
	decision := systemProxySnapshot{mode: systemProxyDirect}
	now := time.Unix(100, 0)
	router := newProviderNetworkRouter(true, func(*http.Request) (*url.URL, error) { return nil, nil }, func(context.Context) systemProxySnapshot {
		return decision
	})
	router.now = func() time.Time { return now }

	if got, err := router.Proxy(proxyTestRequest(t, "https://api.example.test/v1")); err != nil || got != nil {
		t.Fatalf("initial route = %v, %v; want direct", got, err)
	}
	firstGeneration := router.snapshot().Generation

	decision = systemProxySnapshot{mode: systemProxyConfigured, httpsProxy: proxyURL}
	router.invalidate()
	got, err := router.Proxy(proxyTestRequest(t, "https://api.example.test/v1"))
	if err != nil || got == nil || got.String() != proxyURL.String() {
		t.Fatalf("changed route = %v, %v; want %s", got, err, proxyURL)
	}
	if router.snapshot().Generation <= firstGeneration {
		t.Fatalf("route generation did not advance: before=%d after=%d", firstGeneration, router.snapshot().Generation)
	}
}

func TestRefreshAndObserveReportsRouteChangeWithoutNetworkCall(t *testing.T) {
	proxyURL := mustProxyURL(t, "http://127.0.0.1:7897")
	decision := systemProxySnapshot{mode: systemProxyDirect}
	router := newProviderNetworkRouter(true, func(*http.Request) (*url.URL, error) { return nil, nil }, func(context.Context) systemProxySnapshot {
		return decision
	})
	if _, err := router.Proxy(proxyTestRequest(t, "https://api.example.test/v1")); err != nil {
		t.Fatal(err)
	}
	decision = systemProxySnapshot{mode: systemProxyConfigured, httpsProxy: proxyURL}
	status, changed := router.refreshAndObserve(context.Background())
	if !changed || status.Mode != "proxy" || status.Endpoint != "127.0.0.1:7897" {
		t.Fatalf("status=%+v changed=%v", status, changed)
	}
}

func TestShellLaunchedProcessKeepsExplicitEnvironmentProxy(t *testing.T) {
	envProxy := mustProxyURL(t, "http://proxy.example.test:8080")
	router := newProviderNetworkRouter(false, func(*http.Request) (*url.URL, error) {
		return envProxy, nil
	}, func(context.Context) systemProxySnapshot {
		return systemProxySnapshot{mode: systemProxyDirect}
	})

	got, err := router.Proxy(proxyTestRequest(t, "https://api.example.test/v1"))
	if err != nil || got == nil || got.String() != envProxy.String() {
		t.Fatalf("route = %v, %v; want environment proxy", got, err)
	}
}

func TestSystemProxyBypassKeepsLocalModelDirect(t *testing.T) {
	router := newProviderNetworkRouter(true, func(*http.Request) (*url.URL, error) { return nil, nil }, func(context.Context) systemProxySnapshot {
		return systemProxySnapshot{
			mode:      systemProxyConfigured,
			httpProxy: mustProxyURL(t, "http://127.0.0.1:7897"),
			bypass:    []string{"localhost", "127.0.0.1"},
		}
	})
	got, err := router.Proxy(proxyTestRequest(t, "http://127.0.0.1:11434/v1"))
	if err != nil || got != nil {
		t.Fatalf("local model route = %v, %v; want direct", got, err)
	}
}

func TestActionableProviderNetworkErrorNamesRepair(t *testing.T) {
	previous := providerNetwork
	t.Cleanup(func() { providerNetwork = previous })
	providerNetwork = newProviderNetworkRouter(false, func(*http.Request) (*url.URL, error) {
		return mustProxyURL(t, "http://127.0.0.1:7897"), nil
	}, nil)
	_, _ = providerNetwork.Proxy(proxyTestRequest(t, "https://api.example.test/v1"))

	err := ActionableProviderNetworkError(errors.New("proxyconnect tcp: connection refused"))
	if got := err.Error(); !strings.Contains(got, "127.0.0.1:7897") || !strings.Contains(got, "selfmind env refresh --restart") {
		t.Fatalf("actionable error = %q", got)
	}
}

func TestProviderTimeoutRefreshDoesNotMisclassifyLiveProxyListener(t *testing.T) {
	previous := providerNetwork
	t.Cleanup(func() { providerNetwork = previous })
	providerNetwork = newProviderNetworkRouter(false, func(*http.Request) (*url.URL, error) {
		return mustProxyURL(t, "http://127.0.0.1:7897"), nil
	}, nil)
	_, _ = providerNetwork.Proxy(proxyTestRequest(t, "https://api.example.test/v1"))
	providerNetwork.setReachability("reachable")

	if !RefreshProviderNetworkRouteAfterError(errors.New("Post https://api.example.test: context deadline exceeded")) {
		t.Fatal("provider timeout did not refresh route discovery")
	}
	if got := providerNetwork.snapshot().Reachability; got != "reachable" {
		t.Fatalf("reachability = %q, want unchanged", got)
	}
	if !RefreshProviderNetworkRouteAfterError(errors.New("proxyconnect tcp: dial tcp 127.0.0.1:7897: connect: connection refused")) {
		t.Fatal("proxy refusal did not refresh route discovery")
	}
	if got := providerNetwork.snapshot().Reachability; got != "unreachable" {
		t.Fatalf("reachability = %q, want unreachable", got)
	}
}
