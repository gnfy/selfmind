package app

import (
	"context"
	"database/sql"
	"fmt"
	"sync"

	"selfmind/internal/control"
	"selfmind/internal/gateway/httpapi"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/kernel/task/cron"
	"selfmind/internal/platform/config"
	"selfmind/internal/platform/log"
	"selfmind/internal/promptassets"
	"selfmind/internal/tools"
)

// GatewayOptions are the dependencies every agent worker shares.
type GatewayOptions struct {
	DataDir  string
	Memory   *memory.MemoryManager
	Config   *config.Config
	TenantID string
	Prompts  *promptassets.Snapshot
	// Control enables the daemon-owned tools, external-effect claims and the
	// durable background review queue on every worker.
	Control *control.Store
	// Workers is the number of identical agent workers; below one builds one.
	// The daemon derives it from ResolveWorkerCount.
	Workers     int
	RequestGate *llm.RequestGate

	// workerBuilt sees each worker right after it is built. Only tests set
	// it, to make one worker differ and prove that startup refuses the pool.
	workerBuilt func(index int, worker AgentWorker)
}

// GatewayDeps holds the components that InitGateway wires together.
type GatewayDeps struct {
	CronScheduler *cron.Scheduler
	Gateway       *router.Gateway
	// Workers[0] is the primary agent. It also serves management dispatch,
	// model probes and durable review execution; Runs may land on any worker.
	Workers []AgentWorker
	// MCP owns the single connection per configured server that every
	// worker's MCP tools share. Nil when no server is configured.
	MCP *tools.MCPToolManager
	// skills keeps every worker's per-Skill tools one shared set, so a Skill
	// change made through any worker reaches all of them.
	skills *tools.SkillToolManager

	closeOnce sync.Once
}

// InitGateway is the one construction path for the daemon's agent execution:
// the cron scheduler (optional), every agent worker with its complete tool
// catalogue, one shared set of per-Skill tools, one shared MCP connection per
// configured server, and the gateway that schedules Runs onto those workers.
// Both the daemon and the eval harness build through it, so neither can
// assemble a worker the other lacks. Identity, tasks, and delivery stay owned
// by gateway/httpapi and control.db.
func InitGateway(opts GatewayOptions) (_ *GatewayDeps, err error) {
	if opts.Config == nil {
		return nil, fmt.Errorf("gateway configuration is required")
	}
	cronSched, err := initCronScheduler(opts.DataDir, opts.Memory, opts.Config)
	if err != nil {
		return nil, err
	}
	deps := &GatewayDeps{CronScheduler: cronSched}
	defer func() {
		if err != nil {
			deps.Close()
		}
	}()

	count := max(opts.Workers, 1)
	deps.Workers = make([]AgentWorker, 0, count)
	dispatchers := make([]*tools.Dispatcher, 0, count)
	for index := 0; index < count; index++ {
		worker, buildErr := buildAgentWorker(opts, cronSched)
		if buildErr != nil {
			return nil, fmt.Errorf("build agent worker %d: %w", index+1, buildErr)
		}
		if opts.workerBuilt != nil {
			opts.workerBuilt(index, worker)
		}
		deps.Workers = append(deps.Workers, worker)
		dispatchers = append(dispatchers, worker.Dispatcher)
	}
	deps.skills = tools.NewSkillToolManager(dispatchers...)
	deps.MCP = initMCP(opts.Config, dispatchers, opts.Control)
	if err := validateIdenticalWorkers(deps.Workers, deps.MCP, deps.skills); err != nil {
		return nil, err
	}

	primary := deps.Workers[0].Agent
	gw := router.NewGateway(primary, primary.Provider())
	gw.SetIntentClassifier(router.NewIntentClassifierWithRules(router.IntentRuleConfig{
		Mode:            opts.Config.Intent.Mode,
		Rules:           opts.Config.Intent.Rules,
		DirectThreshold: opts.Config.Intent.Thresholds.Direct,
		AskThreshold:    opts.Config.Intent.Thresholds.Ask,
	}))
	displayProvider, displayModel, _ := ResolveModelDisplay(opts.Config)
	gw.SetModelDisplay(displayProvider, displayModel)
	extra := make([]*kernel.Agent, 0, count-1)
	for _, worker := range deps.Workers[1:] {
		extra = append(extra, worker.Agent)
	}
	gw.EnableWorkerPool(extra)
	deps.Gateway = gw
	return deps, nil
}

// Close releases the shared MCP connections, then stops the cron scheduler.
// It is safe to call more than once.
func (d *GatewayDeps) Close() {
	if d == nil {
		return
	}
	d.closeOnce.Do(func() {
		if err := d.MCP.Close(); err != nil {
			log.Warn("gateway: MCP shutdown failed", "error", err)
		}
		StopCron(d.CronScheduler)
	})
}

// initCronScheduler opens the cron store when cron is enabled. The scheduler
// is not started here: the caller installs a JobExecutor first and then calls
// StartCron, so scheduled jobs run the agent instead of the marker fallback.
func initCronScheduler(dataDir string, mem *memory.MemoryManager, cfg *config.Config) (*cron.Scheduler, error) {
	if !cfg.Cron.Enabled {
		return nil, nil
	}
	cronDB, err := sql.Open("sqlite", dataDir+"/cron.db?_journal=WAL&_sync=NORMAL")
	if err != nil {
		return nil, fmt.Errorf("open cron db: %w", err)
	}
	// Same SQLite hygiene as control.db: single writer connection with a
	// busy timeout, so cron writes never race the scheduler goroutine into
	// "database is locked".
	cronDB.SetMaxOpenConns(1)
	if _, err := cronDB.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL; PRAGMA busy_timeout=5000;`); err != nil {
		cronDB.Close()
		return nil, fmt.Errorf("configure cron db: %w", err)
	}
	cronSched := cron.NewScheduler(cronDB, mem)
	if err := cronSched.InitSchema(context.Background()); err != nil {
		cronDB.Close()
		return nil, fmt.Errorf("init cron schema: %w", err)
	}
	// Apply the configured timezone before scheduling any job, so "0 8 * * *"
	// means 08:00 in that zone (e.g. Asia/Shanghai), not UTC.
	if err := cronSched.SetTimezone(cfg.Cron.Timezone); err != nil {
		log.Warn("gateway: invalid cron timezone, using system local", "error", err)
	}

	// Register the liveness canary (W0.3) when configured. It alerts the
	// chosen channel only on failure, so a broken deploy pings you.
	if cfg.Cron.Canary.Enabled {
		if err := registerCanary(context.Background(), cronSched, cfg.Cron.Canary); err != nil {
			log.Warn("gateway: skipped canary registration", "error", err)
		}
	}
	return cronSched, nil
}

// canarySystemKey identifies the canary row the daemon registers. Matching
// by name would take over any job someone else named "canary".
const canarySystemKey = "canary"

// registerCanary keeps the one canary row in line with the configuration. A
// canary an earlier release registered by name alone is recognised only by
// its exact name and bare prompt.
func registerCanary(ctx context.Context, sched *cron.Scheduler, cfg config.CanaryConfig) error {
	expr := cfg.CronExpr
	if expr == "" {
		expr = "0 * * * *" // hourly
	}
	canary := &cron.CronJob{
		Name:      "canary",
		CronExpr:  expr,
		Prompt:    "canary:", // executor runs a trivial liveness turn
		TenantID:  control.DefaultTenantID,
		Channel:   firstNonEmpty(cfg.Channel, cfg.Platform, "cli"),
		Platform:  cfg.Platform,
		DeliverTo: cfg.DeliverTo,
		SystemKey: canarySystemKey,
		Enabled:   true,
	}
	if err := sched.ClaimLegacySystemJob(ctx, canary.SystemKey, canary.Name, canary.Prompt); err != nil {
		return err
	}
	_, err := sched.EnsureJob(ctx, canary)
	return err
}

// registerCronTool gives a worker the cron tool when the scheduler exists.
// Every worker's tool is bound to the same scheduler, and every call to the
// person of the Run that makes it.
func registerCronTool(disp *tools.Dispatcher, cronSched *cron.Scheduler, store *control.Store) {
	if cronSched == nil {
		return
	}
	var authority cron.JobAuthority
	if store != nil {
		authority = httpapi.NewScheduleAuthority(store)
	}
	disp.RegisterTool(&cron.ToolAdapter{CronTool: cron.NewCronTool(cronSched, authority)})
}

// StartCron starts the scheduler loop. Call it after any JobExecutor is
// installed. Safe to call with a nil scheduler.
func StartCron(cronSched *cron.Scheduler) error {
	if cronSched == nil {
		return nil
	}
	return cronSched.Start(context.Background())
}

// StopCron gracefully shuts down the cron scheduler.
func StopCron(cronSched *cron.Scheduler) {
	if cronSched != nil {
		cronSched.Stop(context.Background())
	}
}
