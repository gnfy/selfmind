package cliapp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"selfmind/internal/platform/config"
	"selfmind/internal/platform/log"
)

type App struct {
	ctx        context.Context
	args       []string
	stdin      io.Reader
	stdout     io.Writer
	stderr     io.Writer
	configPath string
	// resumeChannel is the session id from `selfmind --resume <uuid>`; empty
	// starts a fresh session. Chats are channel-local, so resuming means
	// reusing that channel's conversation history.
	resumeChannel string
	// resumeTaskRef is the task/list reference from `selfmind resume <task>`.
	// It is pinned before the TUI starts, then the normal interactive session
	// continues with that task as the next-turn context.
	resumeTaskRef string
	input         *bufio.Reader

	// gatewayEnsured guards the one-time local-daemon auto-start so each CLI
	// client invocation probes/starts the gateway at most once.
	gatewayEnsured bool
}

func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(args) == 0 {
		args = []string{"selfmind"}
	}
	if stdin == nil {
		stdin = os.Stdin
	}
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	if isTopLevelHelp(args) {
		printTopLevelHelp(stdout)
		return 0
	}

	app := &App{
		ctx:    ctx,
		args:   args,
		stdin:  stdin,
		stdout: stdout,
		stderr: stderr,
	}
	cleanedArgs, configPath, err := splitGlobalConfigFlag(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	cleanedArgs, resumeChannel, err := splitResumeFlag(cleanedArgs)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	app.args = cleanedArgs
	app.configPath = configPath
	app.resumeChannel = resumeChannel
	if handled, exitCode := app.extractTaskResumeCommand(); handled && exitCode != 0 {
		return exitCode
	}
	if configPath != "" {
		_ = os.Setenv("SELF_CONFIG", configPath)
	}
	app.applyGatewayConfigEnv()

	if handled, exitCode := app.runGatewayCommandIfRequested(); handled {
		return exitCode
	}
	if handled, exitCode := app.runConfigCommandIfRequested(); handled {
		return exitCode
	}
	if handled, exitCode := app.runModelCommandIfRequested(); handled {
		return exitCode
	}
	if handled, exitCode := app.runAuthCommandIfRequested(); handled {
		return exitCode
	}
	if handled, exitCode := app.runEvalCommandIfRequested(); handled {
		return exitCode
	}
	if handled, exitCode := app.runSelfcheckCommandIfRequested(); handled {
		return exitCode
	}
	if handled, exitCode := app.runMaintenanceCommandIfRequested(); handled {
		return exitCode
	}
	if handled, exitCode := app.runDoctorCommandIfRequested(); handled {
		return exitCode
	}
	if handled, exitCode := app.runWeixinCommandIfRequested(); handled {
		return exitCode
	}
	if handled, exitCode := app.runGatewayClientIfRequested(); handled {
		return exitCode
	}
	return app.runTUI()
}

func isTopLevelHelp(args []string) bool {
	if len(args) < 2 {
		return false
	}
	arg := args[1]
	return arg == "-h" || arg == "--help" || arg == "help"
}

func printTopLevelHelp(stdout io.Writer) {
	fmt.Fprintln(stdout, "SelfMind")
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Usage:")
	fmt.Fprintln(stdout, "  selfmind [--config PATH] [--resume SESSION_ID]")
	fmt.Fprintln(stdout, "  selfmind resume <n|task_id>")
	fmt.Fprintln(stdout, "  selfmind tasks [open|done|archived|all|<keyword>]")
	fmt.Fprintln(stdout, "  selfmind config [doctor|upgrade]")
	fmt.Fprintln(stdout, "  selfmind model [current|check|list|set <provider> <model>]")
	fmt.Fprintln(stdout, "  selfmind auth [login|status|logout] ...")
	fmt.Fprintln(stdout, "  selfmind eval [list|run|report]")
	fmt.Fprintln(stdout, "  selfmind selfcheck [--skip-go|--skip-eval]")
	fmt.Fprintln(stdout, "  selfmind maintenance replay [--limit N]")
	fmt.Fprintln(stdout, "  selfmind maintenance migrate-memory [--apply] [--data-dir DIR]")
	fmt.Fprintln(stdout, "  selfmind doctor [--out FILE] [--probe-models]")
	fmt.Fprintln(stdout, "  selfmind gateway ...")
	fmt.Fprintln(stdout, "  selfmind weixin ...")
}

func (a *App) runTUI() int {
	cfg, err := config.LoadConfig(config.Options{Path: a.configPath})
	if err != nil {
		fmt.Fprintf(a.stderr, "SelfMind config error: %v\n", err)
		fmt.Fprintln(a.stderr, "Run `selfmind doctor` for details.")
		return 1
	}

	log.Init(log.Options{Level: cfg.Agent.LogLevel})
	a.printStartupHealthWarnings()

	// Daemon-only: the TUI is ALWAYS a thin client to the single gateway
	// daemon (ACTIVE PLAN P0-3). The in-process agent path was removed — every
	// entrance (CLI, IM, cron, HTTP) executes inside the daemon, so one worker
	// pool, one auth manager, one control.db owner apply across terminals. If
	// the daemon cannot be reached or started, we fail with actionable guidance
	// instead of silently running a divergent local agent (the old fallback
	// split memory/session state across partitions).
	code, ok := a.tryRunTUIClient(cfg)
	if !ok {
		fmt.Fprintln(a.stderr, "SelfMind could not connect to its gateway daemon.")
		fmt.Fprintln(a.stderr, "Try: `selfmind gateway status`, `selfmind gateway run` (foreground logs), or `selfmind doctor`.")
		return 1
	}
	return code
}

func splitGlobalConfigFlag(args []string) ([]string, string, error) {
	if len(args) == 0 {
		return args, "", nil
	}
	cleaned := []string{args[0]}
	configPath := ""
	for i := 1; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-f" || arg == "--config":
			if i+1 >= len(args) {
				return nil, "", fmt.Errorf("%s requires a path", arg)
			}
			configPath = args[i+1]
			i++
		case strings.HasPrefix(arg, "--config="):
			configPath = strings.TrimPrefix(arg, "--config=")
		case strings.HasPrefix(arg, "-f="):
			configPath = strings.TrimPrefix(arg, "-f=")
		default:
			cleaned = append(cleaned, arg)
		}
	}
	return cleaned, configPath, nil
}

// splitResumeFlag extracts `--resume <uuid>` / `--resume=<uuid>` (and the short
// `-r`) from the TUI args, returning the remaining args and the resume session
// id. The flag only affects the interactive TUI; subcommands ignore it because
// they never see it here.
func splitResumeFlag(args []string) ([]string, string, error) {
	if len(args) == 0 {
		return args, "", nil
	}
	cleaned := []string{args[0]}
	resume := ""
	for i := 1; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-r" || arg == "--resume":
			if i+1 >= len(args) {
				return nil, "", fmt.Errorf("%s requires a session id", arg)
			}
			resume = args[i+1]
			i++
		case strings.HasPrefix(arg, "--resume="):
			resume = strings.TrimPrefix(arg, "--resume=")
		case strings.HasPrefix(arg, "-r="):
			resume = strings.TrimPrefix(arg, "-r=")
		default:
			cleaned = append(cleaned, arg)
		}
	}
	return cleaned, strings.TrimSpace(resume), nil
}

func (a *App) applyGatewayConfigEnv() {
	cfg, err := config.LoadConfig(config.Options{Path: a.configPath})
	if err != nil {
		return
	}
	setEnvIfEmpty("SELF_GATEWAY_ADDR", cfg.Gateway.Addr)
	setEnvIfEmpty("SELF_GATEWAY_URL", cfg.Gateway.URL)
	setEnvIfEmpty("SELF_GATEWAY_TOKEN", cfg.Gateway.Token)
	setEnvIfEmpty("SELF_GATEWAY_DRAIN_TIMEOUT", cfg.Gateway.DrainTimeout)

	// Flight recorder is configured in YAML (flight_recorder.*); push it into the
	// env the low-level recorder reads. An explicit env var still wins (override).
	if cfg.FlightRecorder.Enabled {
		setEnvIfEmpty("SELFMIND_FLIGHT_RECORDER", "1")
	}
	setEnvIfEmpty("SELFMIND_FLIGHT_DIR", cfg.FlightRecorder.Dir)
	if cfg.FlightRecorder.Keep > 0 {
		setEnvIfEmpty("SELFMIND_FLIGHT_KEEP", strconv.Itoa(cfg.FlightRecorder.Keep))
	}
}

// printResumeHint prints the Claude-Code-style resume line on exit so the user
// can reopen this exact conversation. The session id is the channel this run
// used; chats are channel-local, so `--resume <id>` reattaches to its history.
func printResumeHint(w io.Writer, sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	fmt.Fprintln(w, "Resume this session with:")
	fmt.Fprintf(w, "  selfmind --resume %s\n", sessionID)
}

func setEnvIfEmpty(key, value string) {
	value = strings.TrimSpace(value)
	if value == "" || strings.TrimSpace(os.Getenv(key)) != "" {
		return
	}
	_ = os.Setenv(key, value)
}
