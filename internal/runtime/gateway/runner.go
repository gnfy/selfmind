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
	if interrupted, err := controlStore.MarkInterruptedRuns(context.Background(), 30*time.Second); err == nil && interrupted > 0 {
		log.Warn("gateway: marked stale running tasks as interrupted", "count", interrupted)
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
	defer app.StopCron(gwDeps.CronScheduler)
	app.RegisterCronTool(disp, gwDeps.CronScheduler)
	app.InitMCP(disp, cfg)

	gatewayAPI := &httpapi.Server{
		Control:         controlStore,
		Gateway:         gwDeps.Gateway,
		Delivery:        newDeliveryService(controlStore, cfg),
		DefaultTenantID: defaultTenantID,
		DrainTimeout:    drainTimeout,
	}
	if gatewayAPI.Delivery != nil {
		gatewayAPI.Delivery.Start(ctx)
		defer gatewayAPI.Delivery.Stop()
	}
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
}

func newDeliveryService(store *control.Store, cfg *config.Config) *delivery.Service {
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
