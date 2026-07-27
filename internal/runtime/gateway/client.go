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
	if rec, ok := manager.RunningRecord(); ok {
		if !opts.Replace {
			return StartResult{}, AlreadyRunningError{Record: rec}
		}
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
	logFile, err := os.OpenFile(manager.Paths.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return StartResult{}, err
	}
	defer logFile.Close()

	exe, err := resolveDaemonExecutable(opts.Executable)
	if err != nil {
		return StartResult{}, err
	}
	args := detachedRunArgs()
	cmd := exec.Command(exe, args...)
	cmd.Env = os.Environ()
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

func detachedRunArgs() []string {
	return []string{"gateway", "run"}
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
