package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"selfmind/internal/app"
	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/delivery"
	"selfmind/internal/gateway/httpapi"
	"selfmind/internal/gateway/weixin"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/config"
	"selfmind/internal/platform/log"
	"selfmind/internal/tools"
)

// runScratchTTL delays scratch cleanup after a run reaches a terminal state, so
// a finished run's intermediate files stay inspectable for a while instead of
// vanishing the moment it ends.
const runScratchTTL = 24 * time.Hour

type Options struct {
	Addr         string
	Replace      bool
	DrainTimeout time.Duration
	ConfigPath   string
}

func Run(ctx context.Context, opts Options) (runErr error) {
	cfg, err := config.LoadConfig(config.Options{Path: opts.ConfigPath})
	if err != nil {
		// Fail fast: LoadConfig auto-creates the default template when the file
		// is missing, so an error here means a genuinely broken config. Booting
		// with an empty config would half-start a daemon with no providers.
		return fmt.Errorf("load config: %w", err)
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
	previousUnclean, hadUncleanExit := manager.ReconcilePreviousState()
	localControlToken, err := EnsureLocalControlToken(dataDir)
	if err != nil {
		return fmt.Errorf("initialize local control token: %w", err)
	}
	exitReason := ""
	defer func() {
		if recovered := recover(); recovered != nil {
			reason := tools.RedactSensitive(fmt.Sprintf("panic: %v", recovered))
			log.Error("gateway: top-level panic", "error", reason, "stack", string(debug.Stack()))
			manager.Crash(reason)
			panic(recovered)
		}
		if runErr != nil {
			reason := tools.RedactSensitive(runErr.Error())
			log.Error("gateway: stopped because of an error", "error", reason)
			manager.Crash(reason)
			return
		}
		manager.Cleanup(exitReason)
	}()

	defaultTenantID := os.Getenv("SELF_TENANT_ID")
	if defaultTenantID == "" {
		defaultTenantID = control.DefaultTenantID
	}
	if err := manager.WriteStatus("starting", defaultTenantID, ""); err != nil {
		return err
	}

	// Environment snapshot: take ONE reading of the operator environment at
	// start and bind runs to it through their lease. A long-lived daemon that
	// re-reads os.Environ() per command hands children a stale PATH from the
	// shell that happened to start it, and can change account or toolchain in
	// the middle of a run.
	if snapshot := tools.InstallEnvironmentSnapshot(os.Environ(), "inherited"); snapshot != nil {
		log.Info("gateway: environment snapshot installed",
			"snapshot", snapshot.ID, "generation", snapshot.Generation,
			"volatile_entries_dropped", snapshot.VolatileCount)
	}

	// Execution runtime root: run scratch and person-level toolchain caches. It
	// deliberately sits BESIDE the data directory, not inside it — this tree is
	// large, disposable, and must never be swept into artifacts, memory, search
	// indexes, or a data backup.
	runtimeRoot := filepath.Join(filepath.Dir(dataDir), "runtime")
	if err := executionenv.SetRuntimeRoot(runtimeRoot); err != nil {
		// Degrade rather than refuse to start: without scratch the sandbox falls
		// back to a private tmpfs, which is the previous behaviour.
		log.Warn("gateway: execution runtime root unavailable", "path", runtimeRoot, "error", err)
	} else if removed, sweepErr := executionenv.SweepExpiredScratch(runScratchTTL, nil, time.Now()); sweepErr != nil {
		log.Warn("gateway: run scratch sweep failed", "error", sweepErr)
	} else if removed > 0 {
		log.Info("gateway: removed expired run scratch", "count", removed, "ttl", runScratchTTL)
	}

	controlStore, err := control.OpenStore(dataDir)
	if err != nil {
		return fmt.Errorf("control.OpenStore failed: %w", err)
	}
	defer controlStore.Close()
	if hadUncleanExit {
		previousUnclean.InstanceID = previousUnclean.StableInstanceID()
		payload, _ := json.Marshal(map[string]interface{}{
			"pid": previousUnclean.PID, "state": previousUnclean.State,
			"started_at": previousUnclean.StartedAt, "heartbeat_at": previousUnclean.HeartbeatAt,
			"exit_reason": previousUnclean.ExitReason,
		})
		if inserted, recordErr := controlStore.RecordGatewayRuntimeEvent(context.Background(), control.GatewayRuntimeEvent{
			InstanceID: previousUnclean.InstanceID,
			EventType:  "gateway.unclean_exit",
			Payload:    payload,
		}); recordErr != nil {
			log.Warn("gateway: record previous unclean exit failed", "error", recordErr)
		} else if inserted {
			log.Warn("gateway: previous instance ended without a clean shutdown",
				"instance", previousUnclean.InstanceID, "reason", previousUnclean.ExitReason)
		}
	}
	stopTriageTelemetry := tools.SetTriageTelemetrySink(func(event tools.TriageAuditEvent) {
		// Diagnostics are best-effort and must never turn a busy SQLite writer
		// into approval latency on the foreground tool path.
		writeCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()
		_ = controlStore.RecordApprovalTriageAudit(writeCtx, control.ApprovalTriageEvent{
			TenantID: event.TenantID, PersonID: event.PersonID, TaskID: event.TaskID, RunID: event.RunID,
			ToolName: event.ToolName, Outcome: string(event.Outcome), RiskLevel: event.RiskLevel,
			UserAuthorization: event.Authorization, GrantKey: event.GrantKey, ProviderRoute: event.ProviderRoute,
			LatencyMS: event.Latency.Milliseconds(), ErrorClass: event.ErrorClass, PolicyVersion: event.PolicyVersion,
			Rationale: event.Rationale, LastError: event.RedactedError, At: event.At,
		})
	})
	defer stopTriageTelemetry()
	if pruned, pruneErr := controlStore.PruneApprovalTriageEvents(context.Background(), time.Now().Add(-14*24*time.Hour)); pruneErr != nil {
		log.Warn("gateway: prune approval triage history failed", "error", pruneErr)
	} else if pruned > 0 {
		log.Info("gateway: pruned approval triage history", "count", pruned)
	}
	if retention := cfg.Gateway.OutboundRetentionDuration(); retention > 0 {
		pruned, pruneErr := controlStore.PruneOutboundDeliveries(context.Background(), retention)
		if pruneErr != nil {
			log.Warn("gateway: outbound retention sweep failed", "error", pruneErr)
		} else if pruned > 0 {
			log.Info("gateway: pruned terminal outbound history", "count", pruned, "retention", retention)
		}
	}
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
	// Boot-time approval-grant review: remembered classes minted before the
	// current eligibility floor may authorise far more than the human intended
	// (a key naming a shell prologue, or a host grant with no workspace/command
	// resource). Withdraw those before any traffic; the sweep only removes
	// authority and is idempotent.
	if revoked, kept, err := app.ReviewApprovalGrants(context.Background(), controlStore); err != nil {
		log.Warn("gateway: approval grant review failed", "error", err)
	} else if revoked > 0 {
		log.Warn("gateway: withdrew over-broad approval grants", "revoked", revoked, "remaining", len(kept))
	}

	agent, err := app.InitAgent(mem, cfg, defaultTenantID, controlStore)
	if err != nil {
		return fmt.Errorf("app.InitAgent failed: %w", err)
	}

	skillStore := kernel.NewSkillStore(mem)
	disp, err := app.InitTools(mem, cfg, agent, skillStore, defaultTenantID, controlStore)
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
	if workers, werr := app.MaybeEnableWorkerPool(gwDeps.Gateway, mem, cfg, skillStore, defaultTenantID, controlStore); werr != nil {
		log.Warn("worker pool partially enabled", "workers", workers, "error", werr)
	} else if workers > 1 {
		log.Info("agent worker pool enabled", "workers", workers)
	}
	defer app.StopCron(gwDeps.CronScheduler)
	app.RegisterCronTool(disp, gwDeps.CronScheduler)
	if err := disp.ValidateInternalToolSchemas(); err != nil {
		return fmt.Errorf("validate registered tool schemas: %w", err)
	}
	app.InitMCP(disp, cfg)

	gatewayAPI := &httpapi.Server{
		Control:              controlStore,
		Gateway:              gwDeps.Gateway,
		DefaultTenantID:      defaultTenantID,
		ToolSchemaReportFunc: disp.ToolSchemaReport,
		DrainTimeout:         drainTimeout,
		LocalControlToken:    localControlToken,
		// Pending-approval/clarify escrow threshold (Fix 2); "0" disables.
		PendingNotifyAfter: cfg.Gateway.PendingNotifyAfterDuration(),
		// How long a run may park on an unanswered approval. The unattended
		// bound applies only when no endpoint is live AND no account is bound,
		// so a person who simply looked away keeps the full budget.
		ApprovalWait:           cfg.Agent.ApprovalWaitDuration(),
		ApprovalWaitUnattended: cfg.Agent.ApprovalWaitUnattendedDuration(),
		// Smart-mode approval triage (H2): build the cheap-model judge from the
		// agent's dedicated triage provider (a cheap role kept OFF the main run
		// provider). Nil when no provider is available → smart mode asks a human.
		ApprovalJudge: app.NewConfiguredApprovalJudge(mem, cfg, defaultTenantID),
		// A single explicit memory_extract-role pass handles both task-label
		// hygiene and durable fact extraction after eligible runs.
		PostRunAnalyzer: app.NewConfiguredPostRunAnalyzer(mem, cfg, defaultTenantID, controlStore),
		SkillCurator:    app.NewConfiguredSkillCurator(mem, cfg, defaultTenantID, controlStore),
		SelfEvolution: control.EvolutionPolicy{
			Enabled: cfg.Evolution.Enabled, Mode: cfg.Evolution.Mode,
			ShadowAfterObservations:  cfg.Evolution.ShadowAfterObservations,
			PromoteAfterObservations: cfg.Evolution.PromoteAfterObservations,
			MinShadowRuns:            cfg.Evolution.MinShadowRuns,
			MaxShadowFailureRate:     cfg.Evolution.MaxShadowFailureRate,
		},
		// Background memory self-organization (docs/memory-governance.zh-CN.md
		// §4): nil unless memory.governance.enabled AND its model role is
		// explicitly configured; default mode is shadow (report only).
		MemoryConsolidator: memoryConsolidatorOrNil(mem, cfg, defaultTenantID, controlStore),
		// Automatic semantic recall (Work Timeline P2): FTS sessions + task
		// label cards attached at the selector layer; query expansion only when
		// a semantic_recall role model is explicitly configured.
		Recall: httpapi.NewRecallEngine(controlStore, mem, app.SemanticRecallExpander(mem, cfg, defaultTenantID)),
		// Structured session APIs for thin clients (/v1/sessions): the daemon
		// session index, person-partitioned (ACTIVE PLAN P0-3).
		Sessions: mem,
		// Spool root for over-budget tool outputs (execution-quality W1); must
		// match the tool_output_view base dir wired in app.InitTools (both
		// derive from the same resolved data dir).
		ToolOutputDir: filepath.Join(dataDir, "tool-output"),
		// Person-partitioned store for inbound message attachments (e.g. TUI
		// clipboard-pasted images): files are copied here and the partition
		// joins the run's scope so tools can read them (httpapi/attachments.go).
		AttachmentsDir: filepath.Join(dataDir, "attachments"),
	}
	doneAfter, cancelledAfter := cfg.Tasks.AutoArchiveDurations()
	maintenanceDebounce, maintenanceMaxWait, maintenanceBatchMax := cfg.Tasks.MaintenanceBatchPolicy()
	gatewayAPI.TaskGovernance = httpapi.TaskGovernanceOptions{
		InboxEnabled:              cfg.Tasks.InboxEnabled,
		DefaultListLimit:          cfg.Tasks.ListLimit(),
		AutoArchiveDoneAfter:      doneAfter,
		AutoArchiveCancelledAfter: cancelledAfter,
	}
	gatewayAPI.PostRunMaintenance = httpapi.PostRunMaintenanceOptions{
		Debounce: maintenanceDebounce, MaxWait: maintenanceMaxWait, BatchMaxRuns: maintenanceBatchMax,
		LLMTimeout: cfg.Tasks.MaintenanceLLMCallTimeout(),
	}
	// Periodic stuck-run recovery: while the daemon runs, mark heartbeat-dead
	// runs (and their tasks) interrupted. Runs in the coordinator's active-run
	// registry are always excluded, so this only catches runs whose executor
	// died without finalizing.
	// Durable background review (W7): review requests enqueue as idempotent
	// maintenance jobs (payload-hash key) and execute in the maintenance
	// worker with bounded retries; a daemon crash no longer loses them.
	if agent.ReviewEngine != nil {
		gatewayAPI.SkillReviewer = agent.ReviewEngine
		agent.ReviewEngine.SetEnqueue(func(tenantID, payloadJSON string) bool {
			digest := sha256.Sum256([]byte(payloadJSON))
			key := "skillreview_" + hex.EncodeToString(digest[:12])
			inserted, err := controlStore.EnqueueMaintenanceJob(context.Background(), tenantID, key, httpapi.SkillReviewJobVersion, payloadJSON)
			return err == nil && inserted
		})
	}
	stopMaintenanceWorker := gatewayAPI.StartMaintenanceWorker(ctx)
	defer stopMaintenanceWorker()
	stopTaskGovernanceSweeper := gatewayAPI.StartTaskGovernanceSweeper(ctx)
	defer stopTaskGovernanceSweeper()
	stopMemoryGovernance := gatewayAPI.StartMemoryGovernance(ctx)
	defer stopMemoryGovernance()
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
	// Recovery notifications depend on Delivery. Start this worker only after
	// the outbox/router is ready so boot-recovered runs can be surfaced now.
	stopStuckRunSweeper := gatewayAPI.StartStuckRunSweeper(ctx)
	defer stopStuckRunSweeper()
	stopExternalWatchWorker := gatewayAPI.StartExternalWatchWorker(ctx)
	defer stopExternalWatchWorker()
	// Install the cron executor now that the Server + Delivery are ready, then
	// start the scheduler. Scheduled jobs now run real agent turns and deliver
	// results to their channel (e.g. a daily summary pushed to WeChat).
	if gwDeps.CronScheduler != nil {
		gwDeps.CronScheduler.SetExecutor(httpapi.NewCronExecutor(gatewayAPI, gwDeps.CronScheduler))
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
		record := manager.Snapshot()
		return api.GatewayRuntimeInfo{
			PID:             os.Getpid(),
			InstanceID:      record.InstanceID,
			Addr:            addr,
			DataDir:         dataDir,
			RuntimeDir:      manager.Paths.RuntimeDir,
			State:           gatewayAPI.GatewayState(),
			StartedAt:       record.StartedAt,
			UpdatedAt:       record.UpdatedAt,
			HeartbeatAt:     record.HeartbeatAt,
			ExitReason:      record.ExitReason,
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
	stopHeartbeat := make(chan struct{})
	defer close(stopHeartbeat)
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopHeartbeat:
				return
			case <-ticker.C:
				if err := manager.Heartbeat(); err != nil {
					log.Warn("gateway: heartbeat write failed", "error", err)
				}
			}
		}
	}()

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

// memoryConsolidatorOrNil keeps a nil *app.MemoryConsolidator from becoming a
// non-nil httpapi.MemoryConsolidator interface value.
func memoryConsolidatorOrNil(mem *memory.MemoryManager, cfg *config.Config, tenantID string, stores ...*control.Store) httpapi.MemoryConsolidator {
	if c := app.NewConfiguredMemoryConsolidator(mem, cfg, tenantID, stores...); c != nil {
		return c
	}
	return nil
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
	opts := delivery.Options{
		MaxMessageChars: cfg.Gateway.DeliveryMaxMessageChars,
		RetryAttempts:   cfg.Gateway.DeliveryRetryAttempts,
	}
	if raw := strings.TrimSpace(cfg.Gateway.DeliveryCatchUpMaxAge); raw != "" {
		if dur, err := time.ParseDuration(raw); err == nil && dur > 0 {
			opts.CatchUpMaxAge = dur
		}
	}
	return delivery.NewService(store, router, opts)
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
