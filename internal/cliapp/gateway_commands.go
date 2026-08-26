package cliapp

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	appcore "selfmind/internal/app"
	"selfmind/internal/gateway/api"
	"selfmind/internal/modelchange"
	"selfmind/internal/platform/config"
	"selfmind/internal/promptassets"
	gatewayrt "selfmind/internal/runtime/gateway"
)

func (a *App) runGatewayCommandIfRequested() (bool, int) {
	if len(a.args) < 2 || a.args[1] != "gateway" {
		return false, 0
	}
	args := a.args[2:]
	action := "run"
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		action = args[0]
		args = args[1:]
	}

	switch action {
	case "run":
		return true, a.gatewayRun(args)
	case "start":
		return true, a.gatewayStart(args)
	case "status":
		return true, a.gatewayStatus(args)
	case "stop":
		return true, a.gatewayStop(args)
	case "restart":
		return true, a.gatewayRestart(args)
	case "service":
		return true, a.gatewayService(args)
	default:
		fmt.Fprintf(a.stderr, "unknown gateway command: %s\n", action)
		printGatewayUsage(a.stderr)
		return true, 2
	}
}

func (a *App) gatewayRun(args []string) int {
	fs := flag.NewFlagSet("selfmind gateway run", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	replace := fs.Bool("replace", false, "replace a running gateway")
	addr := fs.String("addr", "", "listen address")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := gatewayrt.Run(a.ctx, gatewayrt.Options{
		Addr:       *addr,
		Replace:    *replace,
		ConfigPath: a.configPath,
	}); err != nil {
		if errors.Is(err, modelchange.ErrRecoveryRequired) {
			fmt.Fprintln(a.stderr, err)
			// launchd/systemd restart only on non-zero exit. A recovery circuit is
			// deliberate terminal state, so supervised processes exit cleanly
			// instead of creating an infinite restart storm.
			if strings.TrimSpace(os.Getenv("XPC_SERVICE_NAME")) != "" || strings.TrimSpace(os.Getenv("INVOCATION_ID")) != "" {
				return 0
			}
			return 1
		}
		if errors.Is(err, gatewayrt.ErrAlreadyRunning) {
			fmt.Fprintln(a.stderr, err)
			return 1
		}
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	return 0
}

func (a *App) gatewayStart(args []string) int {
	fs := flag.NewFlagSet("selfmind gateway start", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	replace := fs.Bool("replace", false, "replace a running gateway")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if handled, message, err := gatewayServiceStartIfInstalled(a.configPath); handled {
		if err != nil {
			fmt.Fprintln(a.stderr, err)
			return 1
		}
		fmt.Fprintln(a.stdout, message)
		return 0
	}
	result, err := gatewayrt.StartDetached(gatewayrt.StartOptions{Replace: *replace, ConfigPath: a.configPath})
	if err != nil {
		if errors.Is(err, gatewayrt.ErrAlreadyRunning) {
			fmt.Fprintln(a.stderr, err)
			return 1
		}
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	fmt.Fprintf(a.stdout, "SelfMind gateway started (pid %d).\nLog: %s\n", result.PID, result.LogPath)
	return 0
}

func (a *App) gatewayStatus(args []string) int {
	fs := flag.NewFlagSet("selfmind gateway status", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	jsonOut := fs.Bool("json", false, "print raw JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, cancel := contextWithTimeout(a.ctx, 3*time.Second)
	defer cancel()
	data, statusCode, err := gatewayrt.RequestStatus(ctx, a.gatewayURL())
	if err == nil && statusCode < 400 {
		if *jsonOut {
			fmt.Fprintln(a.stdout, string(data))
			return 0
		}
		var status api.GatewayStatusResponse
		if err := json.Unmarshal(data, &status); err != nil {
			fmt.Fprintln(a.stdout, string(data))
			return 0
		}
		a.printGatewayStatus(status)
		a.printPromptCustomizationHint()
		return 0
	}

	dataDir := a.gatewayDataDir()
	manager := gatewayrt.NewManager(dataDir, "")
	if rec, ok := manager.RunningRecord(); ok {
		if *jsonOut {
			_ = json.NewEncoder(a.stdout).Encode(rec)
			return 0
		}
		if rec.HeartbeatStale(time.Now()) {
			fmt.Fprintf(a.stdout, "SelfMind gateway process exists (pid %d), but its heartbeat is stale and HTTP is unreachable. Run `selfmind doctor`.\n", rec.PID)
		} else {
			fmt.Fprintf(a.stdout, "SelfMind gateway appears to be running (pid %d, addr %s), but HTTP status is unavailable.\n", rec.PID, rec.Addr)
		}
		return 1
	}
	if rec, readErr := gatewayrt.ReadStatusRecord(manager.Paths.StatePath); readErr == nil && rec.State == "crashed" {
		if *jsonOut {
			_ = json.NewEncoder(a.stdout).Encode(rec)
			return 1
		}
		fmt.Fprintf(a.stdout, "SelfMind gateway stopped unexpectedly: %s\n", rec.ExitReason)
		return 1
	}
	if *jsonOut {
		fmt.Fprintln(a.stdout, `{"state":"stopped"}`)
		return 0
	}
	fmt.Fprintln(a.stdout, "SelfMind gateway is not running.")
	return 1
}

func (a *App) gatewayStop(args []string) int {
	fs := flag.NewFlagSet("selfmind gateway stop", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	force := fs.Bool("force", false, "force-kill if graceful shutdown fails")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	serviceInstalled := false
	if gatewayServiceSupported() {
		var err error
		serviceInstalled, _, err = gatewayServiceStatus()
		if err != nil {
			fmt.Fprintln(a.stderr, err)
			return 1
		}
	}
	dataDir := a.gatewayDataDir()
	timeout := gatewayrt.ResolveDrainTimeout() + 10*time.Second
	ctx, cancel := contextWithTimeout(a.ctx, timeout)
	defer cancel()
	if err := gatewayrt.RequestShutdown(ctx, gatewayrt.StopOptions{
		URL:     a.gatewayURL(),
		DataDir: dataDir,
		Force:   *force,
		Timeout: timeout,
		Reason:  strings.TrimSpace(os.Getenv("SELF_GATEWAY_RESTART_REASON")),
	}); err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	if serviceInstalled {
		handled, message, err := gatewayServiceStopIfInstalled()
		if !handled {
			fmt.Fprintln(a.stderr, "SelfMind background service disappeared while stopping.")
			return 1
		}
		if err != nil {
			fmt.Fprintln(a.stderr, err)
			return 1
		}
		fmt.Fprintln(a.stdout, message)
		return 0
	}
	fmt.Fprintln(a.stdout, "SelfMind gateway stopped.")
	return 0
}

func (a *App) gatewayRestart(args []string) int {
	return a.gatewayRestartWithEnvironment(args, nil)
}

// gatewayRestartWithEnvironment restarts the daemon, optionally starting it with
// a supplied environment instead of this process's. `selfmind env refresh` uses
// it to actually ADOPT a freshly sampled login-shell environment: a plain
// restart inherits the CLI's own environment, which is the stale one.
func (a *App) gatewayRestartWithEnvironment(args []string, environment []string) int {
	fs := flag.NewFlagSet("selfmind gateway restart", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	force := fs.Bool("force", false, "force-kill if graceful shutdown fails")
	drain := fs.Bool("drain", false, "explicitly wait for a safe turn boundary (the default restart behavior)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	restartReason := strings.TrimSpace(os.Getenv("SELF_GATEWAY_RESTART_REASON"))
	modelChangeID, modelRestart := modelChangeReasonID(restartReason)
	if modelRestart {
		delay := 1500 * time.Millisecond
		if raw := strings.TrimSpace(os.Getenv("SELF_GATEWAY_RESTART_DELAY_MS")); raw != "" {
			if millis, parseErr := strconv.Atoi(raw); parseErr == nil && millis >= 0 && millis <= 30_000 {
				delay = time.Duration(millis) * time.Millisecond
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-a.ctx.Done():
			timer.Stop()
			return 1
		case <-timer.C:
		}
	}
	_ = drain // Accepted for an explicit upgrade command; restart already drains by default.
	if err := validatePromptWorkspaceForRestart(a.configPath); err != nil {
		fmt.Fprintf(a.stderr, "Prompt validation failed; the running gateway was not restarted: %v\n", err)
		fmt.Fprintln(a.stderr, "Fix the active prompt workspace, then run `selfmind prompt validate`.")
		return 1
	}
	dataDir := a.gatewayDataDir()
	// Capture before RequestShutdown removes the old PID record. A restart may
	// be invoked from an updater or IDE that lacks the login shell's PATH or
	// proxy variables; the new daemon must retain its tool and network paths.
	inheritedRestartEnv := gatewayrt.RunningRestartEnvironment(dataDir)
	timeout := gatewayrt.ResolveDrainTimeout() + 10*time.Second
	var modelChanges *modelchange.Service
	if modelRestart {
		modelChanges = &modelchange.Service{ConfigPath: a.configPath}
	}
	ctx, cancel := contextWithTimeout(a.ctx, timeout)
	if modelRestart {
		cancel()
		ctx, cancel = context.WithCancel(a.ctx)
	}
	defer cancel()
	serviceInstalled := false
	if gatewayServiceSupported() {
		var err error
		serviceInstalled, _, err = gatewayServiceStatus()
		if err != nil {
			fmt.Fprintln(a.stderr, err)
			return 1
		}
	}
	if err := gatewayrt.RequestShutdown(ctx, gatewayrt.StopOptions{
		URL:                 a.gatewayURL(),
		DataDir:             dataDir,
		Force:               *force,
		Timeout:             timeout,
		Reason:              restartReason,
		WaitForSafeBoundary: modelRestart,
		Abort: func() bool {
			if modelChanges == nil {
				return false
			}
			status, inspectErr := modelChanges.Inspect()
			if inspectErr != nil {
				return false
			}
			if status.Pending == nil || !strings.EqualFold(status.Pending.ID, modelChangeID) {
				return true
			}
			switch status.Pending.Status {
			case modelchange.StatusValidating, modelchange.StatusAwaitingSafeBoundary,
				modelchange.StatusCommitting, modelchange.StatusDraining:
				return false
			default:
				return true
			}
		},
	}); err != nil {
		if modelRestart && errors.Is(err, gatewayrt.ErrShutdownAborted) {
			fmt.Fprintf(a.stdout, "Model change %s was cancelled or replaced before restart.\n", modelChangeID)
			return 0
		}
		if modelChanges != nil {
			_, _ = modelChanges.MarkRecoveryRequired(modelChangeID, err)
		}
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	if modelRestart {
		if _, err := modelChanges.BeginDraining(modelChangeID); err != nil {
			fmt.Fprintln(a.stderr, err)
			return 1
		}
		if _, err := modelChanges.MarkRestarting(modelChangeID, gatewayServiceKind()); err != nil {
			fmt.Fprintln(a.stderr, err)
			return 1
		}
	}
	if serviceInstalled {
		if handled, message, err := gatewayServiceRestartIfInstalled(a.configPath); handled {
			if err != nil {
				if modelChanges != nil {
					_, _ = modelChanges.MarkRecoveryRequired(modelChangeID, err)
				}
				fmt.Fprintln(a.stderr, err)
				return 1
			}
			if message != "" {
				fmt.Fprintln(a.stdout, message)
			}
			if modelChanges != nil {
				return a.waitForModelRestart(modelChanges, modelChangeID)
			}
			return 0
		}
	}
	result, err := gatewayrt.StartDetached(gatewayrt.StartOptions{
		Replace:                     true,
		ConfigPath:                  a.configPath,
		InheritedRestartEnvironment: inheritedRestartEnv,
		Environment:                 environment,
	})
	if err != nil {
		if modelChanges != nil {
			_, _ = modelChanges.MarkRecoveryRequired(modelChangeID, err)
		}
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	fmt.Fprintf(a.stdout, "SelfMind gateway restarted (pid %d).\nLog: %s\n", result.PID, result.LogPath)
	if modelChanges != nil {
		return a.waitForModelRestart(modelChanges, modelChangeID)
	}
	return 0
}

func modelChangeReasonID(reason string) (string, bool) {
	const prefix = "model_change:"
	reason = strings.TrimSpace(reason)
	if !strings.HasPrefix(strings.ToLower(reason), prefix) {
		return "", false
	}
	id := strings.TrimSpace(reason[len(prefix):])
	return id, id != ""
}

func (a *App) waitForModelRestart(service *modelchange.Service, changeID string) int {
	deadline := time.Now().Add(120 * time.Second)
	warnAt := time.Now().Add(30 * time.Second)
	warned := false
	for time.Now().Before(deadline) {
		status, err := service.Inspect()
		if err == nil {
			if status.Pending != nil && status.Pending.ID == changeID && status.Pending.Status == modelchange.StatusRecoveryRequired {
				fmt.Fprintf(a.stderr, "Model change %s requires recovery: %s\n", changeID, status.Pending.Failure)
				return 1
			}
			for i := len(status.History) - 1; i >= 0; i-- {
				change := status.History[i]
				if change.ID != changeID {
					continue
				}
				switch change.Status {
				case modelchange.StatusApplied:
					fmt.Fprintf(a.stdout, "Model change %s applied and gateway healthy.\n", changeID)
					return 0
				case modelchange.StatusRolledBack:
					fmt.Fprintf(a.stderr, "Model change %s was rolled back: %s\n", changeID, change.Failure)
					return 1
				case modelchange.StatusConflict, modelchange.StatusFailed, modelchange.StatusCancelled, modelchange.StatusSuperseded:
					fmt.Fprintf(a.stderr, "Model change %s ended as %s: %s\n", changeID, change.Status, change.Failure)
					return 1
				}
			}
		}
		if !warned && time.Now().After(warnAt) {
			warned = true
			fmt.Fprintf(a.stderr, "Model change %s is taking longer than 30 seconds; still waiting for gateway health.\n", changeID)
		}
		select {
		case <-a.ctx.Done():
			return 1
		case <-time.After(200 * time.Millisecond):
		}
	}
	cause := fmt.Errorf("gateway did not become healthy within 120 seconds")
	status, err := service.MarkRecoveryRequired(changeID, cause)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	if status.Pending != nil {
		fmt.Fprintf(a.stderr, "Model change %s requires recovery: %s\n", changeID, status.Pending.Failure)
	}
	return 1
}

func validatePromptWorkspaceForRestart(configPath string) error {
	cfg, err := config.LoadConfig(config.Options{Path: configPath})
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if _, err := appcore.LoadPromptSnapshot(cfg); err != nil {
		return err
	}
	return nil
}

func (a *App) gatewayService(args []string) int {
	if !gatewayServiceSupported() {
		fmt.Fprintln(a.stderr, "Operating-system background service management is unavailable; SelfMind can still run on demand.")
		return 1
	}
	action := "status"
	if len(args) > 0 {
		action = args[0]
	}
	switch action {
	case "install":
		if err := gatewayServicePreflight(); err != nil {
			fmt.Fprintln(a.stderr, err)
			return 1
		}
		installed, _, err := gatewayServiceStatus()
		if err != nil {
			fmt.Fprintln(a.stderr, err)
			return 1
		}
		// A legacy detached gateway and an OS service must never own the same runtime
		// at once. Drain the detached process before registering the service.
		if !installed {
			timeout := gatewayrt.ResolveDrainTimeout() + 10*time.Second
			ctx, cancel := contextWithTimeout(a.ctx, timeout)
			err = gatewayrt.RequestShutdown(ctx, gatewayrt.StopOptions{
				URL:     a.gatewayURL(),
				DataDir: a.gatewayDataDir(),
				Timeout: timeout,
			})
			cancel()
			if err != nil {
				fmt.Fprintln(a.stderr, err)
				return 1
			}
		}
		path, err := gatewayServiceInstall(a.configPath)
		if err != nil {
			fmt.Fprintln(a.stderr, err)
			return 1
		}
		fmt.Fprintf(a.stdout, "SelfMind background service installed and started.\nDefinition: %s\n", path)
		return 0
	case "status":
		_, message, err := gatewayServiceStatus()
		if err != nil {
			fmt.Fprintln(a.stderr, err)
			return 1
		}
		fmt.Fprintln(a.stdout, message)
		return 0
	case "uninstall":
		timeout := gatewayrt.ResolveDrainTimeout() + 10*time.Second
		ctx, cancel := contextWithTimeout(a.ctx, timeout)
		err := gatewayrt.RequestShutdown(ctx, gatewayrt.StopOptions{
			URL:     a.gatewayURL(),
			DataDir: a.gatewayDataDir(),
			Timeout: timeout,
		})
		cancel()
		if err != nil {
			fmt.Fprintln(a.stderr, err)
			return 1
		}
		_, message, err := gatewayServiceUninstall()
		if err != nil {
			fmt.Fprintln(a.stderr, err)
			return 1
		}
		fmt.Fprintln(a.stdout, message)
		return 0
	default:
		fmt.Fprintf(a.stderr, "unknown gateway service command: %s\n", action)
		fmt.Fprintln(a.stderr, "usage: selfmind gateway service [install|status|uninstall]")
		return 2
	}
}

func (a *App) printGatewayStatus(status api.GatewayStatusResponse) {
	runtime := status.Runtime
	fmt.Fprintf(a.stdout, "SelfMind gateway: %s\n", status.State)
	if runtime.BuildFingerprint != "" {
		fmt.Fprintf(a.stdout, "build: %s\n", runtime.BuildFingerprint)
	}
	if runtime.PID > 0 {
		fmt.Fprintf(a.stdout, "pid: %d\n", runtime.PID)
	}
	if runtime.InstanceID != "" {
		fmt.Fprintf(a.stdout, "instance: %s\n", runtime.InstanceID)
	}
	if runtime.HeartbeatAt != "" {
		fmt.Fprintf(a.stdout, "heartbeat: %s\n", runtime.HeartbeatAt)
	}
	if runtime.Addr != "" {
		fmt.Fprintf(a.stdout, "addr: http://%s\n", runtime.Addr)
	}
	if runtime.RuntimeDir != "" {
		fmt.Fprintf(a.stdout, "runtime: %s\n", runtime.RuntimeDir)
	}
	if status.StoreSchema.CurrentVersion > 0 {
		fmt.Fprintf(a.stdout, "control schema: v%d (binary supports v%d)\n", status.StoreSchema.Version, status.StoreSchema.CurrentVersion)
	}
	if status.MCP.Configured > 0 {
		fmt.Fprintf(a.stdout, "mcp servers: %d connected / %d configured, %d failed\n", status.MCP.Connected, status.MCP.Configured, status.MCP.Failed)
		for index, failure := range status.MCP.Failures {
			if index == 3 {
				fmt.Fprintf(a.stdout, "- ... and %d more failure(s); run selfmind doctor --verbose\n", len(status.MCP.Failures)-index)
				break
			}
			fmt.Fprintf(a.stdout, "- %s: %s\n", oneLine(failure.Name, 40), oneLine(failure.Error, 160))
		}
	}
	if status.ActiveRunCount > 0 {
		fmt.Fprintf(a.stdout, "active runs: %d\n", status.ActiveRunCount)
		for _, run := range status.ActiveRuns {
			fmt.Fprintf(a.stdout, "- person=%s task=%s run=%s elapsed=%ds %s\n", run.PersonID, run.TaskID, run.RunID, run.ElapsedSeconds, run.Summary)
		}
	} else {
		fmt.Fprintln(a.stdout, "active runs: 0")
	}
}

func (a *App) printPromptCustomizationHint() {
	_, root, err := a.loadPromptCommandConfig()
	if err != nil {
		return
	}
	spec, ok := promptassets.Spec(promptassets.FileAgent)
	if !ok {
		return
	}
	path := filepath.Join(root, filepath.FromSlash(spec.RelativePath))
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		fmt.Fprintf(a.stdout, "personal preferences: not configured; run `selfmind prompt edit agent` (%s)\n", path)
	}
}

func (a *App) gatewayDataDir() string {
	cfg, err := gatewayrt.LoadConfigForCLI(a.configPath)
	if err != nil {
		return appcore.ResolveDataDir(nil)
	}
	return appcore.ResolveDataDir(cfg)
}

func printGatewayUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: selfmind gateway [run|start|status|stop|restart|service]")
}
