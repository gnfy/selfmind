package cliapp

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	appcore "selfmind/internal/app"
	"selfmind/internal/gateway/api"
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
		return 0
	}

	dataDir := a.gatewayDataDir()
	manager := gatewayrt.NewManager(dataDir, "")
	if rec, ok := manager.RunningRecord(); ok {
		if *jsonOut {
			_ = json.NewEncoder(a.stdout).Encode(rec)
			return 0
		}
		fmt.Fprintf(a.stdout, "SelfMind gateway appears to be running (pid %d, addr %s), but HTTP status is unavailable.\n", rec.PID, rec.Addr)
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
	}); err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	if serviceInstalled {
		handled, message, err := gatewayServiceStopIfInstalled()
		if !handled {
			fmt.Fprintln(a.stderr, "SelfMind launchd service disappeared while stopping.")
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
	fs := flag.NewFlagSet("selfmind gateway restart", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	force := fs.Bool("force", false, "force-kill if graceful shutdown fails")
	drain := fs.Bool("drain", false, "explicitly wait for a safe turn boundary (the default restart behavior)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	_ = drain // Accepted for an explicit upgrade command; restart already drains by default.
	dataDir := a.gatewayDataDir()
	timeout := gatewayrt.ResolveDrainTimeout() + 10*time.Second
	ctx, cancel := contextWithTimeout(a.ctx, timeout)
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
		URL:     a.gatewayURL(),
		DataDir: dataDir,
		Force:   *force,
		Timeout: timeout,
	}); err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	if serviceInstalled {
		if handled, message, err := gatewayServiceRestartIfInstalled(a.configPath); handled {
			if err != nil {
				fmt.Fprintln(a.stderr, err)
				return 1
			}
			if message != "" {
				fmt.Fprintln(a.stdout, message)
			}
			return 0
		}
	}
	result, err := gatewayrt.StartDetached(gatewayrt.StartOptions{Replace: true, ConfigPath: a.configPath})
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	fmt.Fprintf(a.stdout, "SelfMind gateway restarted (pid %d).\nLog: %s\n", result.PID, result.LogPath)
	return 0
}

func (a *App) gatewayService(args []string) int {
	if !gatewayServiceSupported() {
		fmt.Fprintln(a.stderr, "launchd service management is only available on macOS.")
		return 1
	}
	action := "status"
	if len(args) > 0 {
		action = args[0]
	}
	switch action {
	case "install":
		installed, _, err := gatewayServiceStatus()
		if err != nil {
			fmt.Fprintln(a.stderr, err)
			return 1
		}
		// A legacy detached gateway and launchd must never own the same runtime
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
		fmt.Fprintf(a.stdout, "SelfMind launchd service installed and started.\nPlist: %s\n", path)
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
	if runtime.Addr != "" {
		fmt.Fprintf(a.stdout, "addr: http://%s\n", runtime.Addr)
	}
	if runtime.RuntimeDir != "" {
		fmt.Fprintf(a.stdout, "runtime: %s\n", runtime.RuntimeDir)
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
