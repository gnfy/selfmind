package cliapp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
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
	input      *bufio.Reader
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
	app.args = cleanedArgs
	app.configPath = configPath
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
	fmt.Fprintln(stdout, "  selfmind [--config PATH]")
	fmt.Fprintln(stdout, "  selfmind model [current|check|list|set <provider> <model>]")
	fmt.Fprintln(stdout, "  selfmind auth [login|status|logout] ...")
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
	appcore.RegisterCronTool(disp, gwDeps.CronScheduler)

	controlStore, err := control.OpenStore(dataDir)
	if err != nil {
		log.Fatal("control.OpenStore failed", "error", err)
	}

	appcore.InitMCP(disp, cfg)

	displayProvider, displayModel, _ := appcore.ResolveModelDisplay(cfg)
	ctrl := tui.NewControllerWithGateway(gwDeps.Gateway, agent, nil, displayProvider, displayModel, cfg, tenantID)
	localGateway := &httpapi.Server{
		Control:         controlStore,
		Gateway:         gwDeps.Gateway,
		DefaultTenantID: tenantID,
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

func (a *App) applyGatewayConfigEnv() {
	cfg, err := config.LoadConfig(config.Options{Path: a.configPath})
	if err != nil {
		return
	}
	setEnvIfEmpty("SELF_GATEWAY_ADDR", cfg.Gateway.Addr)
	setEnvIfEmpty("SELF_GATEWAY_URL", cfg.Gateway.URL)
	setEnvIfEmpty("SELF_GATEWAY_TOKEN", cfg.Gateway.Token)
	setEnvIfEmpty("SELF_GATEWAY_DRAIN_TIMEOUT", cfg.Gateway.DrainTimeout)
}

func setEnvIfEmpty(key, value string) {
	value = strings.TrimSpace(value)
	if value == "" || strings.TrimSpace(os.Getenv(key)) != "" {
		return
	}
	_ = os.Setenv(key, value)
}
