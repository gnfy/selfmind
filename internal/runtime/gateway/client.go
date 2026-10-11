package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/httpapi"
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
	// Environment, when set, becomes the daemon's environment verbatim instead
	// of this process's.
	//
	// Without it a restart could only ever inherit the CLI's own environment,
	// which made "sample a login shell, then restart to adopt it" impossible:
	// the sample was discarded and the daemon came back with exactly the stale
	// environment the operator was trying to replace.
	Environment []string
}

type StopOptions struct {
	URL     string
	DataDir string
	Force   bool
	Timeout time.Duration
	Reason  string
	// WaitForSafeBoundary leaves the wait bounded only by ctx. Model changes
	// use it so an approval wait is never silently converted into a forced
	// interruption after an arbitrary infrastructure timeout.
	WaitForSafeBoundary bool
	// RequireSafeBoundary tells the daemon that active work must finish before
	// shutdown. This is separate from WaitForSafeBoundary: managed service
	// reconciliation waits for the daemon's bounded drain decision, while an
	// operator restart owns an unbounded drain and must never interrupt a Run.
	RequireSafeBoundary bool
	// Abort is checked while waiting for the old owner to stop. A model-change
	// helper uses it to leave cleanly when its still-cancellable transaction is
	// cancelled or superseded instead of lingering and starting the wrong route.
	Abort func() bool
}

var ErrShutdownAborted = errors.New("gateway shutdown was aborted before the safe boundary")
var ErrShutdownDeferred = errors.New("gateway shutdown was deferred to preserve active work")

// SpawnRestartHelper launches the ordinary CLI restart path in a detached
// helper. This preserves launchd/systemd ownership when a daemon-originated
// model transaction needs to restart the process that is currently serving it.
//
// A helper the daemon starts is a descendant of the daemon, so the daemon never
// takes it for the person's own CLI. capability is the secret that lets it stop
// the daemon for this model change and nothing else; it travels in the helper's
// environment only, never its arguments. The person's own CLI passes none.
func SpawnRestartHelper(configPath, dataDir, changeID, capability string) error {
	exe, err := resolveDaemonExecutable("")
	if err != nil {
		return err
	}
	paths := ResolvePaths(dataDir)
	if err := os.MkdirAll(paths.RuntimeDir, 0o700); err != nil {
		return err
	}
	logFile, err := os.OpenFile(paths.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := restartHelperCommand(exe, configPath, changeID, capability)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	configureDetachedCommand(&cmd.SysProcAttr)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// restartCapabilityEnv names the helper's environment variable that carries its
// capability. The SELF_ prefix keeps it out of every tool's environment.
const restartCapabilityEnv = "SELF_GATEWAY_RESTART_CAPABILITY"

func restartHelperCommand(exe, configPath, changeID, capability string) *exec.Cmd {
	args := []string{"gateway", "restart", "--drain"}
	if strings.TrimSpace(configPath) != "" {
		args = append([]string{"--config", configPath}, args...)
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(),
		"SELF_GATEWAY_RESTART_REASON=model_change:"+strings.TrimSpace(changeID),
		"SELF_GATEWAY_RESTART_DELAY_MS=1500",
	)
	if capability = strings.TrimSpace(capability); capability != "" {
		cmd.Env = append(cmd.Env, restartCapabilityEnv+"="+capability)
	}
	return cmd
}

func StartDetached(opts StartOptions) (StartResult, error) {
	cfg, err := loadConfigOrDefault(opts.ConfigPath)
	if err != nil {
		return StartResult{}, err
	}
	dataDir := resolveDataDir(cfg)
	manager := NewManager(dataDir, ResolveAddr(cfg.Gateway.Addr))
	previousRestartEnv := append([]string(nil), opts.InheritedRestartEnvironment...)
	if rec, owned, probeErr := manager.RuntimeOwnerRecord(); probeErr != nil {
		return StartResult{}, probeErr
	} else if owned {
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
	if err := os.MkdirAll(manager.Paths.RuntimeDir, 0o700); err != nil {
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
	parentEnv := os.Environ()
	if len(opts.Environment) > 0 {
		parentEnv = opts.Environment
	}
	cmd.Env = mergeRestartEnvironment(parentEnv, previousRestartEnv)
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

// RunningRestartEnvironment returns only PATH from the currently running local
// gateway. It is intentionally narrow: restart callers may preserve tool
// discovery, but must not copy arbitrary daemon credentials, process state, or
// ambient proxy settings that disappeared from the current environment.
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
	pathIndex := -1
	currentPath := ""
	for i, entry := range current {
		if key, value, ok := strings.Cut(entry, "="); ok {
			key = strings.TrimSpace(key)
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
		}
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
	case "path":
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

// RequestStatus asks a daemon over TCP. Without the gateway token it answers
// only that it is up; use RequestStatusFor to read its detail as the person.
func RequestStatus(ctx context.Context, url string) ([]byte, int, error) {
	return RequestStatusFor(ctx, url, "")
}

// RequestStatusFor asks the daemon at url for its status. With the data
// directory of a daemon on this host the request goes to its local control
// socket, where the daemon recognizes the person's own CLI and answers in full;
// without one, or for a remote daemon, it goes over TCP.
func RequestStatusFor(ctx context.Context, url, dataDir string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ResolveURL(url)+"/v1/gateway/status", nil)
	if err != nil {
		return nil, 0, err
	}
	attachAuth(req)
	resp, err := doAdminRequest(req, dataDir)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return data, resp.StatusCode, nil
}

// useLocalControl reports whether a request for the daemon of dataDir goes to
// its local control socket: the daemon is on this host, where the kernel tells
// it who is calling. A remote daemon cannot know the caller and is reached over
// TCP with the gateway token. An empty host (":8765") binds every interface but
// still serves this host.
func useLocalControl(rawURL, dataDir string) bool {
	if strings.TrimSpace(dataDir) == "" {
		return false
	}
	return LocalGatewayURL(ResolveURL(rawURL))
}

// doAdminRequest sends a request that stops or inspects the daemon of dataDir.
// A daemon on this host is asked on its control socket; only when there is no
// socket (a daemon that predates it) does the request fall back to TCP, where a
// current daemon answers only a holder of the gateway token. A socket that
// answers is never second-guessed: a refusal there is the daemon's decision.
func doAdminRequest(req *http.Request, dataDir string) (*http.Response, error) {
	if !useLocalControl(req.URL.String(), dataDir) {
		return http.DefaultClient.Do(req)
	}
	retry := req.Clone(req.Context())
	if req.GetBody != nil {
		if body, err := req.GetBody(); err == nil {
			retry.Body = body
		}
	}
	resp, err := LocalControlClient(dataDir).Do(req)
	if err != nil && errors.Is(err, ErrLocalControlUnavailable) {
		return http.DefaultClient.Do(retry)
	}
	return resp, err
}

// RequestToolCatalogProbe invokes the daemon-owned primary provider with its
// real effective tool catalogue. Only the person's own CLI on the local
// control socket may spend quota through this diagnostic endpoint.
func RequestToolCatalogProbe(ctx context.Context, url, dataDir string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ResolveURL(url)+"/v1/gateway/tool-catalog/probe", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	attachAuth(req)
	resp, err := LocalControlClient(dataDir).Do(req)
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
	reason := strings.TrimSpace(opts.Reason)
	if opts.RequireSafeBoundary {
		modelChange := strings.HasPrefix(strings.ToLower(reason), "model_change:") && strings.TrimSpace(reason[len("model_change:"):]) != ""
		if !modelChange {
			// Older daemons do not understand wait_for_safe_boundary. The
			// service-reconcile reason makes their bounded drain defer instead of
			// interrupting active work; the restart caller can safely retry.
			reason = api.ShutdownReasonServiceReconcile
		}
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"force":                  opts.Force,
		"reason":                 reason,
		"wait_for_safe_boundary": opts.RequireSafeBoundary,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ResolveURL(opts.URL)+"/v1/gateway/shutdown", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	attachAuth(req)
	if capability := strings.TrimSpace(os.Getenv(restartCapabilityEnv)); capability != "" && modelChangeReason(reason) {
		req.Header.Set(httpapi.RestartCapabilityHeader, capability)
	}
	var requestWritten atomic.Bool
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				requestWritten.Store(true)
			}
		},
	}))
	resp, requestErr := doAdminRequest(req, opts.DataDir)
	if requestErr != nil {
		if opts.DataDir != "" {
			manager := NewManager(opts.DataDir, "")
			if _, ok := manager.RunningRecord(); !ok {
				return nil
			}
		}
		if opts.Force {
			return forceStopFromDataDir(opts.DataDir)
		}
		// A request that never reached the transport cannot have initiated a
		// shutdown. Once the full request was written, however, an EOF is
		// ambiguous: older Gateways closed their HTTP server before flushing the
		// acceptance response. Continue only when the local runtime receipt can
		// prove that exact owner subsequently disappeared or was replaced.
		if opts.DataDir == "" || !requestWritten.Load() {
			return requestErr
		}
	} else {
		if resp.StatusCode >= 400 {
			data, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if opts.Force {
				return forceStopFromDataDir(opts.DataDir)
			}
			// The daemon does not tell a refused caller where its socket is; the path
			// is this client's own to derive and to show the person.
			hint := ""
			if resp.StatusCode == http.StatusForbidden && opts.DataDir != "" {
				hint = " (this daemon's local control socket is " + LocalControlSocketPath(opts.DataDir) + ")"
			}
			return fmt.Errorf("%s: %s%s", resp.Status, string(data), hint)
		}
		// Release the shutdown request's keep-alive connection before waiting for
		// the daemon PID to disappear. Holding the response body open makes
		// http.Server.Shutdown wait for its full ten-second deadline, needlessly
		// extending the local blackout on every model change.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	deadline := time.Now().Add(timeout)
	serviceReconcile := strings.EqualFold(reason, api.ShutdownReasonServiceReconcile)
	for opts.WaitForSafeBoundary || time.Now().Before(deadline) {
		if opts.Abort != nil && opts.Abort() {
			return ErrShutdownAborted
		}
		gatewayReachable := false
		if serviceReconcile {
			if statusData, statusCode, statusErr := RequestStatusFor(ctx, opts.URL, opts.DataDir); statusErr == nil && statusCode < http.StatusBadRequest {
				var status api.GatewayStatusResponse
				if json.Unmarshal(statusData, &status) == nil {
					gatewayReachable = true
					if strings.EqualFold(strings.TrimSpace(status.State), "running") && !status.Draining {
						return ErrShutdownDeferred
					}
				}
			}
		}
		if opts.DataDir != "" {
			manager := NewManager(opts.DataDir, "")
			if rec, ok := manager.RunningRecord(); !ok || rec.PID <= 0 || (initialPID > 0 && rec.PID != initialPID) {
				if !gatewayReachable {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	if opts.Force {
		return forceStopFromDataDir(opts.DataDir)
	}
	if requestErr != nil {
		return requestErr
	}
	return nil
}

// modelChangeReason reports whether a shutdown reason names a model change, the
// one reason the restart helper's capability is good for.
func modelChangeReason(reason string) bool {
	const prefix = "model_change:"
	reason = strings.TrimSpace(reason)
	return strings.HasPrefix(strings.ToLower(reason), prefix) && strings.TrimSpace(reason[len(prefix):]) != ""
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
