package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type StartResult struct {
	PID     int
	LogPath string
}

type StartOptions struct {
	Replace    bool
	ConfigPath string
	// InheritedRestartEnvironment carries the running daemon's executable
	// search path and proxy variables across a restart that stops the old
	// process before StartDetached runs. Only PATH and recognized proxy keys
	// are accepted. Current PATH entries win, while old-only entries remain
	// available to tools installed outside system directories.
	InheritedRestartEnvironment []string
	// Executable optionally overrides which binary launches the detached
	// daemon. Empty resolves the current process's executable, falling back
	// to `selfmind` on PATH when that path no longer exists — a package
	// upgrade (npm swaps the global package directory) can delete it out from
	// under a running process.
	Executable string
}

type StopOptions struct {
	URL     string
	DataDir string
	Force   bool
	Timeout time.Duration
}

func StartDetached(opts StartOptions) (StartResult, error) {
	cfg, err := loadConfigOrDefault(opts.ConfigPath)
	if err != nil {
		return StartResult{}, err
	}
	dataDir := resolveDataDir(cfg)
	manager := NewManager(dataDir, ResolveAddr(cfg.Gateway.Addr))
	previousRestartEnv := append([]string(nil), opts.InheritedRestartEnvironment...)
	if rec, ok := manager.RunningRecord(); ok {
		if !opts.Replace {
			return StartResult{}, AlreadyRunningError{Record: rec}
		}
		// A restart may be invoked by an IDE, updater, or another non-login
		// process that does not carry the login shell's executable search path
		// or proxy variables. Capture only those restart-safe values before the
		// old daemon exits; arbitrary credentials are never copied.
		previousRestartEnv = append(previousRestartEnv, processRestartEnvironment(rec.PID)...)
		drainTimeout := resolveDrainTimeout(cfg.Gateway.DrainTimeout)
		ctx, cancel := context.WithTimeout(context.Background(), drainTimeout+15*time.Second)
		err := stopExistingForReplace(ctx, manager, drainTimeout)
		cancel()
		if err != nil {
			return StartResult{}, fmt.Errorf("replace running gateway: %w", err)
		}
	}
	if err := os.MkdirAll(manager.Paths.RuntimeDir, 0755); err != nil {
		return StartResult{}, err
	}
	logFile, err := os.OpenFile(manager.Paths.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return StartResult{}, err
	}
	// Older installs created this diagnostic log as 0644. Tighten it whenever
	// the daemon starts because it can contain command lines and provider errors.
	if err := os.Chmod(manager.Paths.LogPath, 0600); err != nil {
		_ = logFile.Close()
		return StartResult{}, err
	}
	defer logFile.Close()

	exe, err := resolveDaemonExecutable(opts.Executable)
	if err != nil {
		return StartResult{}, err
	}
	args := detachedRunArgs()
	cmd := exec.Command(exe, args...)
	cmd.Env = mergeRestartEnvironment(os.Environ(), previousRestartEnv)
	if opts.ConfigPath != "" {
		cmd.Env = append(cmd.Env, "SELF_CONFIG="+opts.ConfigPath)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	configureDetachedCommand(&cmd.SysProcAttr)
	if err := cmd.Start(); err != nil {
		return StartResult{}, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return StartResult{PID: pid, LogPath: manager.Paths.LogPath}, nil
}

// RunningRestartEnvironment returns only PATH and proxy-related environment
// values from the currently running local gateway. It is intentionally narrow:
// restart callers must not copy arbitrary daemon credentials or process state.
func RunningRestartEnvironment(dataDir string) []string {
	rec, ok := NewManager(dataDir, "").RunningRecord()
	if !ok {
		return nil
	}
	return processRestartEnvironment(rec.PID)
}

func detachedRunArgs() []string {
	return []string{"gateway", "run"}
}

func mergeRestartEnvironment(current, previous []string) []string {
	merged := append([]string(nil), current...)
	currentGroups := make(map[string]struct{}, len(current))
	exactKeys := make(map[string]struct{}, len(current))
	pathIndex := -1
	currentPath := ""
	for i, entry := range current {
		if key, value, ok := strings.Cut(entry, "="); ok {
			key = strings.TrimSpace(key)
			currentGroups[strings.ToLower(key)] = struct{}{}
			exactKeys[key] = struct{}{}
			if strings.EqualFold(key, "PATH") {
				pathIndex = i
				currentPath = value
			}
		}
	}
	previousPath := ""
	for _, entry := range previous {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || !isRestartEnvironmentKey(key) {
			continue
		}
		key = strings.TrimSpace(key)
		if strings.EqualFold(key, "PATH") {
			previousPath = value
			continue
		}
		if _, ok := currentGroups[strings.ToLower(key)]; ok {
			continue
		}
		if _, ok := exactKeys[key]; ok {
			continue
		}
		merged = append(merged, entry)
		exactKeys[key] = struct{}{}
	}
	if path := mergePathValues(currentPath, previousPath); path != "" {
		entry := "PATH=" + path
		if pathIndex >= 0 {
			merged[pathIndex] = entry
		} else {
			merged = append(merged, entry)
		}
	}
	return merged
}

func restartEnvironmentFromBlock(data []byte) []string {
	entries := bytes.Split(data, []byte{0})
	out := make([]string, 0, len(entries))
	for _, raw := range entries {
		entry := string(raw)
		key, _, ok := strings.Cut(entry, "=")
		if ok && isRestartEnvironmentKey(key) {
			out = append(out, entry)
		}
	}
	return out
}

func isRestartEnvironmentKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "path", "http_proxy", "https_proxy", "all_proxy", "no_proxy":
		return true
	default:
		return false
	}
}

func mergePathValues(current, previous string) string {
	parts := make([]string, 0)
	seen := make(map[string]struct{})
	appendParts := func(value string) {
		for _, part := range filepath.SplitList(value) {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if _, ok := seen[part]; ok {
				continue
			}
			seen[part] = struct{}{}
			parts = append(parts, part)
		}
	}
	appendParts(current)
	appendParts(previous)
	return strings.Join(parts, string(os.PathListSeparator))
}

// resolveDaemonExecutable picks the binary that runs the detached daemon:
// explicit override → the current executable → `selfmind` on PATH.
func resolveDaemonExecutable(override string) (string, error) {
	current, err := currentExecutable()
	return pickDaemonExecutable(override, current, err)
}

// pickDaemonExecutable is the testable core of resolveDaemonExecutable. The
// existence check on the current executable matters after an in-place package
// upgrade: npm renames the global package directory to a staging dir and
// deletes it, so /proc/self/exe points at a path that no longer exists and a
// fork/exec of it fails (observed live via `selfmind update`).
func pickDaemonExecutable(override, current string, currentErr error) (string, error) {
	if s := strings.TrimSpace(override); s != "" {
		return s, nil
	}
	if currentErr == nil && strings.TrimSpace(current) != "" {
		if _, err := os.Stat(current); err == nil {
			return current, nil
		}
	}
	fromPath, lookErr := exec.LookPath("selfmind")
	if lookErr != nil {
		if currentErr != nil {
			return "", fmt.Errorf("resolve daemon executable: %v; and `selfmind` is not on PATH: %w", currentErr, lookErr)
		}
		return "", fmt.Errorf("current executable %s no longer exists (replaced by an upgrade?) and `selfmind` is not on PATH: %w", current, lookErr)
	}
	return fromPath, nil
}

func RequestStatus(ctx context.Context, url string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ResolveURL(url)+"/v1/gateway/status", nil)
	if err != nil {
		return nil, 0, err
	}
	attachAuth(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return data, resp.StatusCode, nil
}

func RequestShutdown(ctx context.Context, opts StopOptions) error {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = ResolveDrainTimeout() + 5*time.Second
	}
	initialPID := 0
	if opts.DataDir != "" {
		if rec, ok := NewManager(opts.DataDir, "").RunningRecord(); ok {
			initialPID = rec.PID
		}
	}
	payload, _ := json.Marshal(map[string]interface{}{"force": opts.Force})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ResolveURL(opts.URL)+"/v1/gateway/shutdown", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	attachAuth(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if opts.DataDir != "" {
			manager := NewManager(opts.DataDir, "")
			if _, ok := manager.RunningRecord(); !ok {
				return nil
			}
		}
		if opts.Force {
			return forceStopFromDataDir(opts.DataDir)
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(resp.Body)
		if opts.Force {
			return forceStopFromDataDir(opts.DataDir)
		}
		return fmt.Errorf("%s: %s", resp.Status, string(data))
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if opts.DataDir != "" {
			manager := NewManager(opts.DataDir, "")
			if rec, ok := manager.RunningRecord(); !ok || rec.PID <= 0 || (initialPID > 0 && rec.PID != initialPID) {
				return nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	if opts.Force {
		return forceStopFromDataDir(opts.DataDir)
	}
	return nil
}

func forceStopFromDataDir(dataDir string) error {
	if dataDir == "" {
		cfg, err := loadConfigOrDefault("")
		if err != nil {
			return err
		}
		dataDir = resolveDataDir(cfg)
	}
	manager := NewManager(dataDir, "")
	rec, ok := manager.RunningRecord()
	if !ok {
		return nil
	}
	return terminateProcess(rec.PID, true)
}

func attachAuth(req *http.Request) {
	if token := ResolveToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}
