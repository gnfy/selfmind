package cliapp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	appcore "selfmind/internal/app"
	"selfmind/internal/control"
	tui "selfmind/internal/gateway/cli"
	"selfmind/internal/gateway/httpapi"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/config"
	"selfmind/internal/platform/log"
	"selfmind/internal/tools"
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
	if configPath != "" {
		_ = os.Setenv("SELF_CONFIG", configPath)
	}
	app.applyGatewayConfigEnv()

	if handled, exitCode := app.runGatewayCommandIfRequested(); handled {
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
	fmt.Fprintln(stdout, "  selfmind model [current|check|list|set <provider> <model>]")
	fmt.Fprintln(stdout, "  selfmind auth [login|status|logout] ...")
	fmt.Fprintln(stdout, "  selfmind eval [list|run|report]")
	fmt.Fprintln(stdout, "  selfmind selfcheck [--skip-go|--skip-eval]")
	fmt.Fprintln(stdout, "  selfmind doctor [--out FILE]")
	fmt.Fprintln(stdout, "  selfmind gateway ...")
	fmt.Fprintln(stdout, "  selfmind weixin ...")
}

func (a *App) runTUI() int {
	cfg, err := config.LoadConfig(config.Options{Path: a.configPath})
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}

	log.Init(log.Options{Level: cfg.Agent.LogLevel})

	// Daemon-client mode is now the DEFAULT: the TUI runs as a thin client to a
	// single shared gateway daemon (the codex/hermes multi-terminal model), so
	// the worker pool, single auth manager, per-workspace serialization, and one
	// control.db all apply across terminals. `SELFMIND_TUI_INPROC=1` opts out
	// (build an in-process gateway, the legacy single-process path). If the
	// daemon can't be reached/started, we fall back to in-process so a
	// misconfigured or first-run environment still gets a working TUI.
	if os.Getenv("SELFMIND_TUI_INPROC") != "1" {
		if code, ok := a.tryRunTUIClient(cfg); ok {
			return code
		}
		fmt.Fprintln(a.stderr, "Falling back to in-process mode (set SELFMIND_TUI_INPROC=1 to silence).")
	}

	mem, dataDir, err := appcore.InitStorage(cfg)
	if err != nil {
		log.Fatal("app.InitStorage failed", "error", err)
	}

	tenantID := os.Getenv("SELF_TENANT_ID")
	if tenantID == "" {
		tenantID = "default"
	}

	agent, err := appcore.InitAgent(mem, cfg, tenantID)
	if err != nil {
		log.Fatal("app.InitAgent failed", "error", err)
	}

	skillStore := kernel.NewSkillStore(mem)
	disp, err := appcore.InitTools(mem, cfg, agent, skillStore, tenantID)
	if err != nil {
		log.Fatal("app.InitTools failed", "error", err)
	}

	agent.SetBackend(disp)

	gwDeps, err := appcore.InitGateway(dataDir, mem, agent, cfg, skillStore)
	if err != nil {
		log.Fatal("app.InitGateway failed", "error", err)
	}
	// Optional multi-worker execution (SELFMIND_WORKERS>1); default 1 keeps the
	// single-agent serialized path unchanged.
	if workers, err := appcore.MaybeEnableWorkerPool(gwDeps.Gateway, mem, cfg, skillStore, tenantID); err != nil {
		log.Warn("worker pool partially enabled", "workers", workers, "error", err)
	} else if workers > 1 {
		log.Info("agent worker pool enabled", "workers", workers)
	}
	appcore.RegisterCronTool(disp, gwDeps.CronScheduler)
	// Local TUI path has no gateway Server/executor; start cron so jobs still
	// run (degrading to the marker fallback) instead of never firing.
	if err := appcore.StartCron(gwDeps.CronScheduler); err != nil {
		log.Warn("cron scheduler did not start", "error", err)
	}

	controlStore, err := control.OpenStore(dataDir)
	if err != nil {
		log.Fatal("control.OpenStore failed", "error", err)
	}

	appcore.InitMCP(disp, cfg)

	displayProvider, displayModel, _ := appcore.ResolveModelDisplay(cfg)
	ctrl := tui.NewControllerWithGateway(gwDeps.Gateway, agent, nil, displayProvider, displayModel, cfg, tenantID)
	ctrl.SetSessionChannel(a.resumeChannel)
	localGateway := &httpapi.Server{
		Control:         controlStore,
		Gateway:         gwDeps.Gateway,
		DefaultTenantID: tenantID,
		// Smart-mode approval triage (H2): cheap-model judge off the main run
		// provider. Nil when unavailable → smart mode asks a human.
		ApprovalJudge: appcore.NewApprovalJudge(agent.ApprovalJudgeProvider()),
	}
	ctrl.SetMessageProcessor(localGateway.ProcessMessage)
	disp.InjectClarifyHandler(ctrl.ClarifyHandler())
	ctrl.SetSessionSearchFn(mem.SearchFn(tenantID))

	memFn := func() (*memory.MemoryManager, string, string) { return mem, tenantID, "cli" }
	msgFn := func() ([]byte, error) {
		msgs, err := tui.GetCheckpointMessages()
		if err != nil || msgs == nil {
			return nil, err
		}
		return json.Marshal(msgs)
	}
	wrappedMemFn := func() (*memory.MemoryManager, string, string, error) {
		m, t, c := memFn()
		return m, t, c, nil
	}
	checkpointTool := tools.NewCheckpointTool(wrappedMemFn, msgFn)
	disp.RegisterTool(checkpointTool)
	ctrl.SetCheckpointFns(memFn, msgFn)

	ctrl.SetCleanupFn(func() {
		appcore.StopCron(gwDeps.CronScheduler)
		if controlStore != nil {
			controlStore.Close()
		}
		if mem != nil {
			mem.Close()
		}
	})

	ctrl.Start()
	printResumeHint(a.stdout, ctrl.SessionChannel())
	fmt.Fprintln(a.stdout, "Goodbye!")
	return 0
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
