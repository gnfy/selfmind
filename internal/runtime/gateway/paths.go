package gateway

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	DefaultAddr      = "127.0.0.1:8765"
	defaultDrainTime = 30 * time.Second
)

type Paths struct {
	DataDir    string
	RuntimeDir string
	PIDPath    string
	StatePath  string
	LockPath   string
	LogPath    string
}

func ResolvePaths(dataDir string) Paths {
	runtimeDir := filepath.Join(dataDir, "gateway")
	return Paths{
		DataDir:    dataDir,
		RuntimeDir: runtimeDir,
		PIDPath:    filepath.Join(runtimeDir, "gateway.pid"),
		StatePath:  filepath.Join(runtimeDir, "gateway_state.json"),
		LockPath:   filepath.Join(runtimeDir, "gateway.lock"),
		LogPath:    filepath.Join(runtimeDir, "gateway.log"),
	}
}

func ResolveAddr(explicit string) string {
	if addr := strings.TrimSpace(explicit); addr != "" {
		return addr
	}
	if addr := strings.TrimSpace(os.Getenv("SELF_GATEWAY_ADDR")); addr != "" {
		return addr
	}
	if addr := strings.TrimSpace(os.Getenv("SELF_DAEMON_ADDR")); addr != "" {
		return addr
	}
	return DefaultAddr
}

// guardPublicBind is the W5 fail-closed check. The default bind is loopback
// (127.0.0.1), so nothing is exposed and no token is needed. But if the operator
// binds a non-loopback address (e.g. 0.0.0.0 to reach webhook IM from the
// internet) without setting an auth token, the API — including /v1/message and
// /v1/gateway/shutdown — would be open to anyone. Refuse to start in that case
// with an actionable message instead of silently exposing the agent.
func guardPublicBind(addr, token string) error {
	if strings.TrimSpace(token) != "" {
		return nil // authenticated: any bind is the operator's choice
	}
	host := strings.TrimSpace(addr)
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = strings.TrimSpace(h)
	}
	if host == "" {
		// Empty host means "all interfaces" (e.g. ":8765") — treat as public.
		return fmt.Errorf("refusing to bind %q on all interfaces without an auth token; set SELF_GATEWAY_TOKEN (or gateway.token), or bind a loopback address like %s", addr, DefaultAddr)
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() {
			return nil
		}
	} else if host == "localhost" {
		return nil
	}
	return fmt.Errorf("refusing to bind non-loopback address %q without an auth token; set SELF_GATEWAY_TOKEN (or gateway.token) before exposing the gateway, or bind a loopback address like %s", addr, DefaultAddr)
}

func ResolveURL(explicit string) string {
	if url := strings.TrimRight(strings.TrimSpace(explicit), "/"); url != "" {
		return url
	}
	if url := strings.TrimRight(strings.TrimSpace(os.Getenv("SELF_GATEWAY_URL")), "/"); url != "" {
		return url
	}
	if url := strings.TrimRight(strings.TrimSpace(os.Getenv("SELF_DAEMON_URL")), "/"); url != "" {
		return url
	}
	return "http://" + ResolveAddr("")
}

func ResolveToken() string {
	if token := strings.TrimSpace(os.Getenv("SELF_GATEWAY_TOKEN")); token != "" {
		return token
	}
	return strings.TrimSpace(os.Getenv("SELF_DAEMON_TOKEN"))
}

func ResolveDrainTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv("SELF_GATEWAY_DRAIN_TIMEOUT"))
	if raw == "" {
		return defaultDrainTime
	}
	if seconds, err := time.ParseDuration(raw); err == nil && seconds > 0 {
		return seconds
	}
	if n, err := time.ParseDuration(raw + "s"); err == nil && n > 0 {
		return n
	}
	return defaultDrainTime
}
