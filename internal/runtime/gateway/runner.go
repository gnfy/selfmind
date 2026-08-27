package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"selfmind/internal/app"
	"selfmind/internal/buildinfo"
	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/delivery"
	"selfmind/internal/gateway/httpapi"
	"selfmind/internal/gateway/weixin"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/modelchange"
	"selfmind/internal/platform/config"
	"selfmind/internal/platform/log"
	"selfmind/internal/promptassets"
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
	dataDir := app.ResolveDataDir(cfg)
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
	manager.ConfigPath = cfg.Path
	if opts.Replace {
		_ = stopExistingForReplace(ctx, manager, drainTimeout)
	}
	if err := manager.Acquire(); err != nil {
		return err
	}
	previousUnclean, hadUncleanExit := manager.ReconcilePreviousState()
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

	// Model transitions are reconciled only after this process owns
	// gateway.lock. launchd/systemd may briefly start competing processes; they
	// must not each increment attempts or mutate the same candidate transaction.
	modelChanges := &modelchange.Service{ConfigPath: cfg.Path, Validate: app.ValidateModelChange}
	modelStatus, modelRolledBack, err := modelChanges.ReconcileStartup(ctx)
	if err != nil {
		return fmt.Errorf("reconcile model configuration: %w", err)
	}
	modelStartupPending := modelStatus.Pending != nil && modelStatus.Pending.Status == modelchange.StatusStarting
	modelStartupHealthy := false
	defer func() {
		if !modelStartupPending || modelStartupHealthy {
			return
		}
		cause := runErr
		if cause == nil {
			cause = fmt.Errorf("gateway did not reach the healthy listener boundary")
		}
		if _, _, recoveryErr := modelChanges.FailStarting(cause); recoveryErr != nil {
			log.Error("gateway: record model startup recovery failed", "error", tools.RedactSensitive(recoveryErr.Error()))
		} else {
			log.Warn("gateway: model startup requires recovery", "error", tools.RedactSensitive(cause.Error()))
		}
	}()
	// A model-attributable startup probe may restore the last healthy routes.
	// Reload before constructing providers so no module retains the rejected
	// candidate.
	cfg, err = config.LoadConfig(config.Options{Path: opts.ConfigPath})
	if err != nil {
		return fmt.Errorf("reload reconciled config: %w", err)
	}
	if modelRolledBack {
		latest := "model candidate"
		if n := len(modelStatus.History); n > 0 {
			latest = modelStatus.History[n-1].ID
		}
		log.Warn("gateway: rejected model change and restored the last running routes", "change", latest)
	}

	mem, initializedDataDir, err := app.InitStorage(cfg)
	if err != nil {
		return fmt.Errorf("app.InitStorage failed: %w", err)
	}
	dataDir = initializedDataDir
	defer func() {
		if mem != nil {
			_ = mem.Close()
		}
	}()
	localControlToken, err := EnsureLocalControlToken(dataDir)
	if err != nil {
		return fmt.Errorf("initialize local control token: %w", err)
	}
	prompts, promptStatus := app.InspectRuntimePromptSnapshot(cfg, dataDir)
	if promptStatus.Source != app.PromptSourceActive {
		log.Warn("gateway: invalid prompt workspace; continuing with safe fallback",
			"source", promptStatus.Source,
			"error_kind", promptStatus.ActiveErrorKind,
			"error", tools.RedactSensitive(promptStatus.ActiveError),
			"prompt_root", promptStatus.ActiveRoot)
	}

	defaultTenantID := os.Getenv("SELF_TENANT_ID")
	if defaultTenantID == "" {
		defaultTenantID = control.DefaultTenantID
	}
	if err := manager.WriteStatus("starting", defaultTenantID, ""); err != nil {
		return err
	}
	promptStatus = app.ActivateRuntimePromptSnapshot(prompts, promptStatus, dataDir)
	if promptStatus.ActivationError != "" {
		// The selected snapshot is validated and safe to use. Only future durable
		// prompt pinning is degraded, so keep foreground endpoints available and
		// make the cache failure visible instead of refusing service.
		log.Warn("gateway: selected prompt snapshot could not be activated durably",
			"source", promptStatus.Source,
			"error", tools.RedactSensitive(promptStatus.ActivationError),
			"prompt_root", promptStatus.ActiveRoot)
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
	recordPromptSnapshotLoaded(controlStore, manager.Snapshot().InstanceID, prompts, promptStatus)
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

	agent, err := app.InitAgent(mem, cfg, defaultTenantID, prompts, controlStore)
	if err != nil {
		return fmt.Errorf("app.InitAgent failed: %w", err)
	}

	disp, err := app.InitTools(mem, cfg, agent, defaultTenantID, prompts, controlStore)
	if err != nil {
		return fmt.Errorf("app.InitTools failed: %w", err)
	}
	agent.SetBackend(disp)

	gwDeps, err := app.InitGateway(dataDir, mem, agent, cfg)
	if err != nil {
		return fmt.Errorf("app.InitGateway failed: %w", err)
	}
	// Optional multi-worker execution (SELFMIND_WORKERS>1) for the daemon, where
	// concurrent CLI/IM/cron requests can actually exercise it. Default 1 = the
	// single-agent serialized path, unchanged.
	if workers, werr := app.MaybeEnableWorkerPool(gwDeps.Gateway, mem, cfg, defaultTenantID, prompts, controlStore); werr != nil {
		log.Warn("worker pool partially enabled", "workers", workers, "error", werr)
	} else if workers > 1 {
		log.Info("agent worker pool enabled", "workers", workers)
	}
	defer app.StopCron(gwDeps.CronScheduler)
	app.RegisterCronTool(disp, gwDeps.CronScheduler)
	if err := disp.ValidateInternalToolSchemas(); err != nil {
		return fmt.Errorf("validate registered tool schemas: %w", err)
	}
	skillStorage, err := app.ResolveSkillStorage(cfg)
	if err != nil {
		return fmt.Errorf("resolve skill storage: %w", err)
	}
	mcpManager := app.InitMCP(disp, cfg)
	var mcpHealthFunc func() tools.MCPHealthSnapshot
	if mcpManager != nil {
		mcpHealthFunc = mcpManager.Health
		defer func() {
			if err := mcpManager.Close(); err != nil {
				log.Warn("gateway: MCP shutdown failed", "error", err)
			}
		}()
	}

	gatewayAPI := &httpapi.Server{
		Control:                controlStore,
		Gateway:                gwDeps.Gateway,
		DefaultTenantID:        defaultTenantID,
		PromptSnapshotHash:     prompts.Hash(),
		ToolSchemaReportFunc:   disp.ToolSchemaReport,
		ToolCatalogPreviewFunc: agent.ProviderToolCatalogPreview,
		ToolCatalogProbeFunc: func(ctx context.Context) api.ProviderToolCatalogProbeResponse {
			probe := agent.ProbeProviderToolCatalog(ctx)
			response := api.ProviderToolCatalogProbeResponse{
				OK: probe.Err == nil, Provider: cfg.EffectiveProvider(), Model: probe.Model,
				LatencyMS: probe.Latency.Milliseconds(), Catalog: probe.Catalog,
			}
			if probe.Err != nil {
				response.Error = tools.RedactSensitive(probe.Err.Error())
			}
			return response
		},
		ModelProbeFunc: func(ctx context.Context, role string) api.ModelProbeResponse {
			runtime, err := app.ResolveModelRuntime(ctx, cfg, role)
			if err != nil {
				return api.ModelProbeResponse{Role: role, Error: tools.RedactSensitive(err.Error())}
			}
			probe := app.ProbeResolvedModelForRole(ctx, runtime, role)
			response := api.ModelProbeResponse{
				OK: probe.Err == nil, Role: role, Provider: runtime.Provider,
				Model: runtime.Model, LatencyMS: probe.Latency.Milliseconds(),
			}
			if probe.Err != nil {
				response.Error = tools.RedactSensitive(probe.Err.Error())
			}
			return response
		},
		ModelChanges: modelChanges,
		ModelRestartFunc: func(changeID string) error {
			return SpawnRestartHelper(cfg.Path, dataDir, changeID)
		},
		MCPHealthFunc:     mcpHealthFunc,
		SkillStorage:      skillStorage,
		DrainTimeout:      drainTimeout,
		LocalControlToken: localControlToken,
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
		PostRunAnalyzer: app.NewConfiguredPostRunAnalyzer(mem, cfg, defaultTenantID, prompts, controlStore),
		SkillCurator:    app.NewConfiguredSkillCurator(mem, cfg, defaultTenantID, controlStore, prompts),
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
		MemoryConsolidator: memoryConsolidatorOrNil(mem, cfg, defaultTenantID, prompts, controlStore),
		// Automatic semantic recall (Work Timeline P2): FTS sessions + task
		// label cards attached at the selector layer; query expansion only when
		// a semantic_recall role model is explicitly configured.
		Recall: httpapi.NewRecallEngine(controlStore, mem, app.SemanticRecallExpander(mem, cfg, defaultTenantID, prompts)),
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
		agent.ReviewEngine.SetEnqueue(func(_ string, payloadJSON string) bool {
			digest := sha256.Sum256([]byte(payloadJSON))
			key := "skillreview_" + hex.EncodeToString(digest[:12])
			inserted, err := controlStore.EnqueueMaintenanceJob(context.Background(), defaultTenantID, key, httpapi.SkillReviewJobVersion, payloadJSON)
			return err == nil && inserted
		})
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	defer listener.Close()
	stopTaskGovernanceSweeper := gatewayAPI.StartTaskGovernanceSweeper(ctx)
	defer stopTaskGovernanceSweeper()
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
	if weixinAdapter != nil {
		if err := weixinAdapter.Start(ctx); err != nil {
			log.Warn("gateway: weixin adapter did not start", "error", err)
		} else {
			defer weixinAdapter.Stop()
		}
	}
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
			PID:                   os.Getpid(),
			InstanceID:            record.InstanceID,
			Addr:                  addr,
			DataDir:               dataDir,
			RuntimeDir:            manager.Paths.RuntimeDir,
			State:                 gatewayAPI.GatewayState(),
			StartedAt:             record.StartedAt,
			UpdatedAt:             record.UpdatedAt,
			HeartbeatAt:           record.HeartbeatAt,
			ExitReason:            record.ExitReason,
			DefaultTenantID:       defaultTenantID,
			ConfigPath:            cfg.Path,
			ServiceManager:        strings.TrimSpace(os.Getenv("SELFMIND_SERVICE_MANAGER")),
			ServiceGeneration:     strings.TrimSpace(os.Getenv("SELFMIND_SERVICE_GENERATION")),
			ModelRouteFingerprint: modelchange.SnapshotFromConfig(cfg).Fingerprint(),
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
		errCh <- httpServer.Serve(listener)
	}()
	if err := manager.WriteStatus("running", defaultTenantID, ""); err != nil {
		return err
	}
	if err := waitHealthy(ctx, localGatewayHealthURL(addr), 3*time.Second); err != nil {
		return fmt.Errorf("verify gateway health before applying model change: %w", err)
	}
	healthyModelStatus, err := modelChanges.MarkStartupHealthy()
	if err != nil {
		return fmt.Errorf("commit healthy model startup: %w", err)
	}
	modelStartupHealthy = healthyModelStatus.ModelReady()
	if modelStartupHealthy {
		// Every model-backed background executor starts only after the same
		// startup probes + listener health boundary that releases foreground
		// work. A control-plane-only daemon leaves their durable jobs untouched.
		stopMaintenanceWorker := gatewayAPI.StartMaintenanceWorker(ctx)
		defer stopMaintenanceWorker()
		stopMemoryGovernance := gatewayAPI.StartMemoryGovernance(ctx)
		defer stopMemoryGovernance()
		if gwDeps.CronScheduler != nil {
			gwDeps.CronScheduler.SetExecutor(httpapi.NewCronExecutor(gatewayAPI, gwDeps.CronScheduler))
			if err := app.StartCron(gwDeps.CronScheduler); err != nil {
				log.Warn("gateway: cron scheduler did not start", "error", err)
			}
		}
	}
	// Resume durable work only after the candidate transaction is applied.
	// Otherwise queued requests could begin on a route whose listener later
	// fails the health gate and requires recovery.
	if modelStartupHealthy {
		gatewayAPI.DrainQueuedAtBoot(ctx)
	} else {
		log.Warn("gateway: model readiness is incomplete; queued work remains parked", "hint", "run `selfmind model`")
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

	if gatewayAPI.ActiveRunCount() == 0 {
		// Safe-boundary shutdown has no foreground work left. Close persistent
		// SSE observers immediately so they do not hold Server.Shutdown open for
		// its full deadline; TUI clients already reconnect from durable cursors.
		_ = httpServer.Close()
	} else {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = httpServer.Shutdown(shutdownCtx)
		cancel()
	}
	_ = manager.WriteStatus("stopped", defaultTenantID, exitReason)
	return nil
}

func localGatewayHealthURL(addr string) string {
	host, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return "http://" + strings.TrimSpace(addr)
	}
	switch strings.TrimSpace(host) {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, port)
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
func memoryConsolidatorOrNil(mem *memory.MemoryManager, cfg *config.Config, tenantID string, prompts *promptassets.Snapshot, store *control.Store) httpapi.MemoryConsolidator {
	if c := app.NewConfiguredMemoryConsolidator(mem, cfg, tenantID, prompts, store); c != nil {
		return c
	}
	return nil
}

func recordPromptSnapshotLoaded(store *control.Store, instanceID string, snapshot *promptassets.Snapshot, status app.PromptSnapshotStatus) {
	if store == nil || snapshot == nil || strings.TrimSpace(instanceID) == "" {
		return
	}
	type fileSummary struct {
		ID         string   `json:"id"`
		Hash       string   `json:"hash"`
		Customized bool     `json:"customized"`
		Sections   []string `json:"custom_sections,omitempty"`
	}
	files := make([]fileSummary, 0, len(snapshot.Files()))
	for _, state := range snapshot.Files() {
		entry := fileSummary{ID: state.ID, Hash: state.Hash, Customized: state.Customized}
		for name, value := range state.Sections {
			if value.Mode != promptassets.ModeDefault {
				entry.Sections = append(entry.Sections, name)
			}
		}
		sort.Strings(entry.Sections)
		files = append(files, entry)
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"catalog_version":   promptassets.CatalogVersion,
		"snapshot_hash":     snapshot.Hash(),
		"build_fingerprint": buildinfo.Fingerprint(),
		"root":              snapshot.Root(),
		"source":            status.Source,
		"degraded":          status.Degraded(),
		"active_error_kind": status.ActiveErrorKind,
		"active_error":      tools.RedactSensitive(status.ActiveError),
		"fallback_error":    tools.RedactSensitive(status.FallbackError),
		"activation_error":  tools.RedactSensitive(status.ActivationError),
		"files":             files,
	})
	eventTypes := []string{"prompt.snapshot.loaded"}
	if status.Degraded() {
		eventTypes = append(eventTypes, "prompt.workspace.degraded")
	}
	for _, eventType := range eventTypes {
		if _, err := store.RecordGatewayRuntimeEvent(context.Background(), control.GatewayRuntimeEvent{
			InstanceID: instanceID,
			EventType:  eventType,
			Payload:    payload,
		}); err != nil {
			log.Warn("gateway: record prompt snapshot event failed", "event_type", eventType, "error", err)
		}
	}
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
