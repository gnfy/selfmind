package cliapp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mattn/go-isatty"

	"selfmind/internal/buildinfo"
	"selfmind/internal/crashreport"
	"selfmind/internal/gateway/api"
	tui "selfmind/internal/gateway/cli"
	"selfmind/internal/modelchange"
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
	// additionalDirs is the invocation-local --add-dir overlay. Interactive
	// sessions send it with every new run; short-lived `send` calls use the same
	// parsed list. It never mutates the durable workspace registration.
	additionalDirs []string
	input          *bufio.Reader

	// gatewayEnsured guards the one-time local-daemon auto-start so each CLI
	// client invocation probes/starts the gateway at most once.
	gatewayEnsured bool
	// updateNotices carries the background update-check result (buffered 1)
	// into the TUI session for the in-session announcement; nil when the
	// check was skipped (disabled, dev build, or fresh cache).
	updateNotices chan tui.UpdateNotice
	// announcedUpdateVersion is the version the startup cache-based notice
	// printed, used to dedupe the in-session and exit-time announcements.
	announcedUpdateVersion string
	// interactive is true only when both stdin and stdout are attached to a
	// terminal. First-run setup must never prompt scripts, cron, or pipes.
	interactive bool
	// onboarding is the reconciled first-use state for this invocation. It is
	// populated before the TUI starts and supplies the initial workspace/model
	// summary plus the one-shot first-success receipt callback.
	onboarding *onboardingState
	// onboardingGatewayStatus is the true external HTTP seam for setup
	// readiness. Tests provide a status without binding a local port.
	onboardingGatewayStatus func(context.Context) (api.GatewayStatusResponse, error)
	// managedGatewayReconcile is the platform-service boundary used by focused
	// onboarding acceptance tests. Production leaves it nil.
	managedGatewayReconcile func() (gatewayServiceInstallReceipt, error)
	managedServiceHealthy   func() bool
	// modelChangeValidate is a focused test seam. Production uses the same
	// role-aware live probes as daemon startup and onboarding.
	modelChangeValidate modelchange.Validator
	// Model recovery must replace the process that owns the failed candidate.
	// Focused tests inject these boundaries to verify stop -> restore -> start
	// ordering without touching the developer's real background service.
	modelRecoveryStop  func() error
	modelRecoveryStart func() error
	modelRecoveryWait  func() error
	// modelManagerOnly makes `selfmind model` open the same transient manager
	// as `/model`, then exit instead of entering a chat session.
	modelManagerOnly bool
}

var Version = buildinfo.Version

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
	if isVersionCommand(args) {
		fmt.Fprintf(stdout, "SelfMind %s\n", buildinfo.Display())
		return 0
	}

	app := &App{
		ctx:         ctx,
		args:        args,
		stdin:       stdin,
		stdout:      stdout,
		stderr:      stderr,
		interactive: interactiveTerminal(stdin, stdout),
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
	cleanedArgs, additionalDirs, err := splitAddDirFlags(cleanedArgs)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	app.args = cleanedArgs
	app.configPath = configPath
	app.resumeChannel = resumeChannel
	app.additionalDirs = additionalDirs
	if handled, exitCode := app.extractTaskResumeCommand(); handled && exitCode != 0 {
		return exitCode
	}
	if configPath != "" {
		_ = os.Setenv("SELF_CONFIG", configPath)
	}
	app.applyGatewayConfigEnv()

	if handled, exitCode := app.runSetupCommandIfRequested(); handled {
		return exitCode
	}
	if handled, exitCode := app.runUpdateCommandIfRequested(); handled {
		return exitCode
	}
	if handled, exitCode := app.runUninstallCommandIfRequested(); handled {
		return exitCode
	}
	if handled, exitCode := app.runFeedbackCommandIfRequested(); handled {
		return exitCode
	}
	if handled, exitCode := app.runGatewayCommandIfRequested(); handled {
		return exitCode
	}
	if handled, exitCode := app.runEnvCommandIfRequested(); handled {
		return exitCode
	}
	if handled, exitCode := app.runConfigCommandIfRequested(); handled {
		return exitCode
	}
	if handled, exitCode := app.runPromptCommandIfRequested(); handled {
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
	if handled, exitCode := app.runDocsCommandIfRequested(); handled {
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
	if len(app.args) > 1 {
		fmt.Fprintf(app.stderr, "unknown command %q\n", app.args[1])
		fmt.Fprintln(app.stderr, "Run `selfmind --help` to see available commands.")
		return 2
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

func isVersionCommand(args []string) bool {
	if len(args) != 2 {
		return false
	}
	switch args[1] {
	case "-v", "--version", "version":
		return true
	default:
		return false
	}
}

var documentedCLIUsages = []string{
	"selfmind [--config PATH] [--resume SESSION_ID] [--add-dir DIR]...",
	"selfmind --version",
	"selfmind setup [--non-interactive] [--skip-model] [--skip-gateway] [--check-model]",
	"selfmind update [check] [--channel latest|next] [--force] [--no-restart]",
	"selfmind uninstall --prepare [--purge-data --yes]",
	"selfmind feedback [--out FILE|--send] [--repo OWNER/REPO] [--include-crash] <message>",
	"selfmind send [--async] [--mode MODE] [--add-dir DIR]... <message>",
	"selfmind status",
	"selfmind usage",
	"selfmind report daily [--since 24h]",
	"selfmind watchers [active|attention|recent|all [page]|<n|id>|cancel <n|id>]",
	"selfmind tasks [done|archived|all|<keyword>]",
	"selfmind task <n|task_id> [runs|rename <name>|pin|unpin|complete|archive|merge <dst>]",
	"selfmind resume <n|task_id|run_id>",
	"selfmind workspaces",
	"selfmind ws [list|add|use|trust|untrust|grants|observe|revoke|<n|workspace_id>] ...",
	"selfmind approvals",
	"selfmind approve [token]",
	"selfmind reject [token]",
	"selfmind stop",
	"selfmind id",
	"selfmind new [title]",
	"selfmind config [doctor|upgrade]",
	"selfmind env [show|refresh]",
	"selfmind model",
	"selfmind prompt [list|show|edit|diff|validate|test|reset|apply] ...",
	"selfmind auth [login|status|logout] ...",
	"selfmind doctor [--verbose] [--out FILE] [--probe-models]",
	"selfmind docs [check|index]",
	"selfmind selfcheck [--fast | --profile local-full|local-fast|ci] [--skip-go] [--skip-eval] [--eval-dir DIR]",
	"selfmind eval [list|run|report|repair|scorecard|capture|clean]",
	"selfmind maintenance [replay|migrate-memory|migrate-skills|cleanup-person-partitions|prune-skill-candidate-refs|migrate-task-references|memory-audit|memory-dedup|task-audit|reset-work-history|restore-control] ...",
	"selfmind gateway [run|start|status|stop|restart|service] ...",
	"selfmind weixin [login|status] ...",
}

func printTopLevelHelp(stdout io.Writer) {
	fmt.Fprintln(stdout, "SelfMind")
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Usage:")
	for _, usage := range documentedCLIUsages {
		fmt.Fprintf(stdout, "  %s\n", usage)
	}
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Full command reference: docs/command-reference.md")
}

func (a *App) runTUI() int {
	cfg, err := config.LoadConfig(config.Options{Path: a.configPath})
	if err != nil {
		fmt.Fprintf(a.stderr, "SelfMind config error: %v\n", err)
		fmt.Fprintln(a.stderr, "Run `selfmind doctor` for details.")
		return 1
	}

	cfg, code := a.prepareTUIConfig(cfg)
	if code != 0 {
		return code
	}

	log.Init(log.Options{Level: cfg.Agent.LogLevel})
	a.printStartupHealthWarnings()
	if path, ok := crashreport.ConsumeNotice(); ok {
		fmt.Fprintf(a.stderr, "SelfMind recovered from a previous crash. Report: %s\n", path)
		fmt.Fprintln(a.stderr, "Review it locally, or send it explicitly with `selfmind feedback --include-crash <message>`.")
	}
	a.printUpdateNotice(cfg)

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

func (a *App) prepareTUIConfig(cfg *config.Config) (*config.Config, int) {
	if a.modelManagerOnly {
		// `selfmind model` repairs model configuration and its receipt, so an
		// incomplete or stale onboarding receipt must not intercept the sole
		// model-management entrypoint with the legacy setup picker.
		return cfg, 0
	}
	return a.ensureOnboarding(cfg, onboardingOptions{})
}

func interactiveTerminal(stdin io.Reader, stdout io.Writer) bool {
	in, inOK := stdin.(*os.File)
	out, outOK := stdout.(*os.File)
	if !inOK || !outOK {
		return false
	}
	return (isatty.IsTerminal(in.Fd()) || isatty.IsCygwinTerminal(in.Fd())) &&
		(isatty.IsTerminal(out.Fd()) || isatty.IsCygwinTerminal(out.Fd()))
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

const maxCLIAdditionalDirs = 8

// splitAddDirFlags extracts repeatable --add-dir flags for both the
// interactive TUI and `selfmind send`. Resolve them in the client process: a
// relative path belongs to the shell that launched SelfMind, never to the
// daemon's cwd. The daemon repeats canonical validation before granting scope.
func splitAddDirFlags(args []string) ([]string, []string, error) {
	if len(args) == 0 {
		return args, nil, nil
	}
	cleaned := []string{args[0]}
	var rawDirs []string
	literal := false
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if literal {
			cleaned = append(cleaned, arg)
			continue
		}
		if arg == "--" {
			literal = true
			cleaned = append(cleaned, arg)
			continue
		}
		switch {
		case arg == "--add-dir":
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("--add-dir requires a directory")
			}
			rawDirs = append(rawDirs, args[i+1])
			i++
		case strings.HasPrefix(arg, "--add-dir="):
			rawDirs = append(rawDirs, strings.TrimPrefix(arg, "--add-dir="))
		default:
			cleaned = append(cleaned, arg)
		}
	}
	if len(rawDirs) > maxCLIAdditionalDirs {
		return nil, nil, fmt.Errorf("--add-dir may be specified at most %d times", maxCLIAdditionalDirs)
	}
	seen := make(map[string]struct{}, len(rawDirs))
	dirs := make([]string, 0, len(rawDirs))
	for _, raw := range rawDirs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return nil, nil, fmt.Errorf("--add-dir requires a non-empty directory")
		}
		absolute, err := filepath.Abs(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve --add-dir %q: %w", raw, err)
		}
		info, err := os.Stat(absolute)
		if err != nil {
			return nil, nil, fmt.Errorf("inspect --add-dir %q: %w", raw, err)
		}
		if !info.IsDir() {
			return nil, nil, fmt.Errorf("--add-dir %q is not a directory", raw)
		}
		canonical, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve --add-dir %q: %w", raw, err)
		}
		canonical = filepath.Clean(canonical)
		if _, ok := seen[canonical]; ok {
			continue
		}
		seen[canonical] = struct{}{}
		dirs = append(dirs, canonical)
	}
	return cleaned, dirs, nil
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
