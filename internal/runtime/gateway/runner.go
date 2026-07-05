package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"selfmind/internal/app"
	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/delivery"
	"selfmind/internal/gateway/httpapi"
	"selfmind/internal/gateway/weixin"
	"selfmind/internal/kernel"
	"selfmind/internal/platform/config"
	"selfmind/internal/platform/log"
)

type Options struct {
	Addr          string
	Replace       bool
	DetachedChild bool
	DrainTimeout  time.Duration
	ConfigPath    string
}

func Run(ctx context.Context, opts Options) error {
	cfg, err := config.LoadConfig(config.Options{Path: opts.ConfigPath})
	if err != nil {
		cfg = &config.Config{}
	}
	applyGatewayRuntimeEnv(cfg)
	log.Init(log.Options{Level: cfg.Agent.LogLevel})

	mem, dataDir, err := app.InitStorage(cfg)
	if err != nil {
		return fmt.Errorf("app.InitStorage failed: %w", err)
	}
	defer func() {
		if mem != nil {
			_ = mem.Close()
		}
	}()

	addr := ResolveAddr(firstNonEmpty(opts.Addr, cfg.Gateway.Addr))
	// Fail closed: never expose the agent on a public interface without auth.
	gwToken := firstNonEmpty(
		strings.TrimSpace(os.Getenv("SELF_GATEWAY_TOKEN")),
		strings.TrimSpace(os.Getenv("SELF_DAEMON_TOKEN")),
		strings.TrimSpace(cfg.Gateway.Token),
	)
	if err := guardPublicBind(addr, gwToken); err != nil {
		return err
	}
	drainTimeout := opts.DrainTimeout
	if drainTimeout <= 0 {
		drainTimeout = resolveDrainTimeout(cfg.Gateway.DrainTimeout)
	}
	manager := NewManager(dataDir, addr)
	if opts.Replace {
		_ = stopExistingForReplace(ctx, manager, drainTimeout)
	}
	if err := manager.Acquire(); err != nil {
		return err
	}
	exitReason := ""
	defer func() {
		manager.Cleanup(exitReason)
	}()

	defaultTenantID := os.Getenv("SELF_TENANT_ID")
	if defaultTenantID == "" {
		defaultTenantID = control.DefaultTenantID
	}
	if err := manager.WriteStatus("starting", defaultTenantID, ""); err != nil {
		return err
	}

	controlStore, err := control.OpenStore(dataDir)
	if err != nil {
		return fmt.Errorf("control.OpenStore failed: %w", err)
	}
	defer controlStore.Close()
	// Boot-time stuck-run recovery: manager.Acquire above holds the
	// gateway.lock flock, so this is the ONLY daemon on this control.db and
	// every run still 'running' here was orphaned by a previous daemon
	// (kill/crash mid-run). Sweep them all (threshold 0, no heartbeat grace)
	// and repair tasks left 'running' with no live run, before any traffic.
	if interrupted, err := controlStore.MarkInterruptedRuns(context.Background(), 0); err != nil {
		log.Warn("gateway: stuck-run boot sweep failed", "error", err)
	} else if interrupted > 0 {
		log.Warn("gateway: recovered interrupted runs/tasks from previous daemon", "count", interrupted)
	}

	agent, err := app.InitAgent(mem, cfg, defaultTenantID)
	if err != nil {
		return fmt.Errorf("app.InitAgent failed: %w", err)
	}

	skillStore := kernel.NewSkillStore(mem)
	disp, err := app.InitTools(mem, cfg, agent, skillStore, defaultTenantID)
	if err != nil {
		return fmt.Errorf("app.InitTools failed: %w", err)
	}
	agent.SetBackend(disp)

	gwDeps, err := app.InitGateway(dataDir, mem, agent, cfg, skillStore)
	if err != nil {
		return fmt.Errorf("app.InitGateway failed: %w", err)
	}
	// Optional multi-worker execution (SELFMIND_WORKERS>1) for the daemon, where
	// concurrent CLI/IM/cron requests can actually exercise it. Default 1 = the
	// single-agent serialized path, unchanged.
	if workers, werr := app.MaybeEnableWorkerPool(gwDeps.Gateway, mem, cfg, skillStore, defaultTenantID); werr != nil {
		log.Warn("worker pool partially enabled", "workers", workers, "error", werr)
	} else if workers > 1 {
		log.Info("agent worker pool enabled", "workers", workers)
	}
	defer app.StopCron(gwDeps.CronScheduler)
	app.RegisterCronTool(disp, gwDeps.CronScheduler)
	app.InitMCP(disp, cfg)

	gatewayAPI := &httpapi.Server{
		Control:         controlStore,
		Gateway:         gwDeps.Gateway,
		DefaultTenantID: defaultTenantID,
		DrainTimeout:    drainTimeout,
		// Implicit-continuation LLM upgrade window (Fix 1) and pending-approval/
		// clarify escrow threshold (Fix 2). Both derive from config; "0" disables.
		ContinueWindow:     cfg.Intent.ContinueWindowDuration(),
		PendingNotifyAfter: cfg.Gateway.PendingNotifyAfterDuration(),
		// Smart-mode approval triage (H2): build the cheap-model judge from the
		// agent's dedicated triage provider (a cheap role kept OFF the main run
		// provider). Nil when no provider is available → smart mode asks a human.
		ApprovalJudge: app.NewApprovalJudge(agent.ApprovalJudgeProvider()),
	}
	// Periodic stuck-run recovery: while the daemon runs, mark heartbeat-dead
	// runs (and their tasks) interrupted. Runs in the coordinator's active-run
	// registry are always excluded, so this only catches runs whose executor
	// died without finalizing.
	stopStuckRunSweeper := gatewayAPI.StartStuckRunSweeper(ctx)
	defer stopStuckRunSweeper()
	var weixinAdapter *weixin.Adapter
	if cfg.Gateway.Weixin.Enabled {
		wxCfg := weixin.RuntimeConfigFrom(cfg.Gateway.Weixin, dataDir, defaultTenantID)
		weixinAdapter = weixin.NewAdapter(wxCfg, controlStore, gatewayAPI.ProcessMessage)
	}
	gatewayAPI.Delivery = newDeliveryService(controlStore, cfg, weixinAdapter)
	if gatewayAPI.Delivery != nil {
		gatewayAPI.Delivery.Start(ctx)
		defer gatewayAPI.Delivery.Stop()
	}
	// Install the cron executor now that the Server + Delivery are ready, then
	// start the scheduler. Scheduled jobs now run real agent turns and deliver
	// results to their channel (e.g. a daily summary pushed to WeChat).
	if gwDeps.CronScheduler != nil {
		gwDeps.CronScheduler.SetExecutor(httpapi.NewCronExecutor(gatewayAPI))
		if err := app.StartCron(gwDeps.CronScheduler); err != nil {
			log.Warn("gateway: cron scheduler did not start", "error", err)
		}
	}
	if weixinAdapter != nil {
		if err := weixinAdapter.Start(ctx); err != nil {
			log.Warn("gateway: weixin adapter did not start", "error", err)
		} else {
			defer weixinAdapter.Stop()
		}
	}
	// Boot drain (G1+G2): resume tasks queued behind a run that a previous
	// daemon never finished. Runs after Delivery/adapters are ready so queued
	// items launched here can route their results back.
	gatewayAPI.DrainQueuedAtBoot(ctx)
	stopCh := make(chan struct{})
	var stopOnce sync.Once
	gatewayAPI.ShutdownFunc = func() {
		stopOnce.Do(func() {
			close(stopCh)
		})
	}
	gatewayAPI.RuntimeStatusFunc = func() api.GatewayRuntimeInfo {
		return api.GatewayRuntimeInfo{
			PID:             os.Getpid(),
			Addr:            addr,
			DataDir:         dataDir,
			RuntimeDir:      manager.Paths.RuntimeDir,
			State:           gatewayAPI.GatewayState(),
			StartedAt:       manager.Started.UTC().Format(time.RFC3339),
			UpdatedAt:       time.Now().UTC().Format(time.RFC3339),
			DefaultTenantID: defaultTenantID,
		}
	}

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           gatewayAPI.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		fmt.Printf("SelfMind gateway listening on http://%s\n", addr)
		errCh <- httpServer.ListenAndServe()
	}()
	if err := manager.WriteStatus("running", defaultTenantID, ""); err != nil {
		return err
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case <-ctx.Done():
		exitReason = "context cancelled"
		gatewayAPI.RequestGatewayShutdown(drainTimeout, exitReason)
		waitForStopSignal(stopCh, drainTimeout+5*time.Second)
	case sig := <-sigCh:
		exitReason = fmt.Sprintf("signal %s", sig)
		fmt.Printf("SelfMind gateway shutting down (%s)\n", sig)
		_ = manager.WriteStatus("draining", defaultTenantID, exitReason)
		gatewayAPI.RequestGatewayShutdown(drainTimeout, exitReason)
		waitForStopSignal(stopCh, drainTimeout+5*time.Second)
	case <-stopCh:
		exitReason = "shutdown requested"
		_ = manager.WriteStatus("draining", defaultTenantID, exitReason)
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			exitReason = err.Error()
			return err
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	_ = manager.WriteStatus("stopped", defaultTenantID, exitReason)
	return nil
}

func applyGatewayRuntimeEnv(cfg *config.Config) {
	if cfg == nil {
		return
	}
	setEnvIfEmpty("SELF_GATEWAY_TOKEN", cfg.Gateway.Token)
	setEnvIfEmpty("SELF_GATEWAY_ADDR", cfg.Gateway.Addr)
	setEnvIfEmpty("SELF_GATEWAY_URL", cfg.Gateway.URL)
	setEnvIfEmpty("SELF_GATEWAY_DRAIN_TIMEOUT", cfg.Gateway.DrainTimeout)
	// Feishu inbound webhook signature/verification is validated in the httpapi
	// layer from these env vars; bridge them from config so a single config file
	// drives both inbound verification and the outbound sender.
	setEnvIfEmpty("SELF_FEISHU_ENCRYPT_KEY", cfg.Gateway.Feishu.EncryptKey)
	setEnvIfEmpty("SELF_FEISHU_VERIFICATION_TOKEN", cfg.Gateway.Feishu.VerificationToken)
}

func newDeliveryService(store *control.Store, cfg *config.Config, weixinSender delivery.Sender) *delivery.Service {
	if store == nil || cfg == nil {
		return nil
	}
	var defaultSender delivery.Sender
	if strings.TrimSpace(cfg.Gateway.OutboundWebhookURL) != "" {
		defaultSender = &delivery.WebhookSender{
			URL:   cfg.Gateway.OutboundWebhookURL,
			Token: cfg.Gateway.OutboundWebhookToken,
		}
	}
	router := delivery.NewRouter(defaultSender)
	if strings.TrimSpace(cfg.Gateway.TelegramToken) != "" {
		router.Register("telegram", &delivery.TelegramSender{Token: cfg.Gateway.TelegramToken})
	}
	if weixinSender != nil {
		router.Register("weixin", weixinSender)
	}
	if cfg.Gateway.Wechat.Enabled && strings.TrimSpace(cfg.Gateway.Wechat.AppID) != "" {
		router.Register("wechat", &delivery.WechatSender{
			AppID:     cfg.Gateway.Wechat.AppID,
			AppSecret: cfg.Gateway.Wechat.AppSecret,
			BaseURL:   cfg.Gateway.Wechat.BaseURL,
		})
	}
	if cfg.Gateway.Feishu.Enabled && strings.TrimSpace(cfg.Gateway.Feishu.AppID) != "" {
		feishuSender := &delivery.FeishuSender{
			AppID:     cfg.Gateway.Feishu.AppID,
			AppSecret: cfg.Gateway.Feishu.AppSecret,
			BaseURL:   cfg.Gateway.Feishu.BaseURL,
		}
		router.Register("feishu", feishuSender)
		router.Register("lark", feishuSender)
	}
	if cfg.Gateway.QQ.Enabled && strings.TrimSpace(cfg.Gateway.QQ.AppID) != "" {
		router.Register("qq", &delivery.QQSender{
			AppID:   cfg.Gateway.QQ.AppID,
			Secret:  cfg.Gateway.QQ.Secret,
			BaseURL: cfg.Gateway.QQ.BaseURL,
		})
	}
	return delivery.NewService(store, router, delivery.Options{
		MaxMessageChars: cfg.Gateway.DeliveryMaxMessageChars,
		RetryAttempts:   cfg.Gateway.DeliveryRetryAttempts,
	})
}

func resolveDrainTimeout(configValue string) time.Duration {
	if value := strings.TrimSpace(configValue); value != "" && strings.TrimSpace(os.Getenv("SELF_GATEWAY_DRAIN_TIMEOUT")) == "" {
		if d, err := parseDurationSeconds(value); err == nil && d > 0 {
			return d
		}
	}
	return ResolveDrainTimeout()
}

func parseDurationSeconds(value string) (time.Duration, error) {
	if d, err := time.ParseDuration(value); err == nil {
		return d, nil
	}
	return time.ParseDuration(value + "s")
}

func setEnvIfEmpty(key, value string) {
	value = strings.TrimSpace(value)
	if value == "" || strings.TrimSpace(os.Getenv(key)) != "" {
		return
	}
	_ = os.Setenv(key, value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func stopExistingForReplace(ctx context.Context, manager *Manager, drainTimeout time.Duration) error {
	rec, ok := manager.RunningRecord()
	if !ok {
		return nil
	}
	url := "http://" + rec.Addr
	stopCtx, cancel := context.WithTimeout(ctx, drainTimeout+10*time.Second)
	defer cancel()
	_ = RequestShutdown(stopCtx, StopOptions{
		URL:     url,
		DataDir: manager.Paths.DataDir,
		Force:   true,
		Timeout: drainTimeout + 5*time.Second,
	})
	deadline := time.Now().Add(drainTimeout + 10*time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(rec.PID) {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return terminateProcess(rec.PID, true)
}

func waitForStopSignal(stopCh <-chan struct{}, timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-stopCh:
	case <-timer.C:
	}
}
