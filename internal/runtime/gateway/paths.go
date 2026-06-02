package gateway

import (
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
