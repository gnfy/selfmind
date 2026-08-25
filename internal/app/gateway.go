package app

import (
	"context"
	"database/sql"
	"fmt"

	"selfmind/internal/control"
	"selfmind/internal/gateway/channel"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/identity"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/kernel/task"
	"selfmind/internal/kernel/task/cron"
	"selfmind/internal/platform/config"
	"selfmind/internal/platform/log"
	"selfmind/internal/tools"
)

// GatewayDeps holds the components that InitGateway wires together.
type GatewayDeps struct {
	IdentityMapper *identity.IdentityMapper
	TaskManager    *task.Manager
	CronScheduler  *cron.Scheduler
	Gateway        *router.Gateway
	Bridge         *channel.Bridge
}

// InitGateway builds the identity mapper, task manager, cron scheduler
// (optional), and the unified gateway.
func InitGateway(dataDir string, mem *memory.MemoryManager, agent *kernel.Agent, cfg *config.Config) (*GatewayDeps, error) {
	idMapper := identity.NewIdentityMapper(dataDir)
	taskMgr := task.NewManager(dataDir)

	var cronSched *cron.Scheduler
	if cfg.Cron.Enabled {
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
		cronSched = cron.NewScheduler(cronDB, mem)
		if err := cronSched.InitSchema(context.Background()); err != nil {
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
			expr := cfg.Cron.Canary.CronExpr
			if expr == "" {
				expr = "0 * * * *" // hourly
			}
			canary := &cron.CronJob{
				Name:      "canary",
				CronExpr:  expr,
				Prompt:    "canary:", // executor runs a trivial liveness turn
				TenantID:  control.DefaultTenantID,
				Channel:   firstNonEmpty(cfg.Cron.Canary.Channel, cfg.Cron.Canary.Platform, "cli"),
				Platform:  cfg.Cron.Canary.Platform,
				DeliverTo: cfg.Cron.Canary.DeliverTo,
				Enabled:   true,
			}
			if _, err := cronSched.EnsureJob(context.Background(), canary); err != nil {
				log.Warn("gateway: skipped canary registration", "error", err)
			}
		}

		// NOTE: do not Start() here. The scheduler must be started only after the
		// caller installs a JobExecutor (SetExecutor) so scheduled jobs run the
		// agent and deliver results instead of falling back to the marker path.
		// Callers use StartCron once wiring is complete.
	}

	var provider llm.Provider
	if agent != nil {
		provider = agent.Provider()
	}
	gw := router.NewGateway(idMapper, taskMgr, agent, provider)
	gw.SetIntentClassifier(router.NewIntentClassifierWithRules(router.IntentRuleConfig{
		Mode:            cfg.Intent.Mode,
		Rules:           cfg.Intent.Rules,
		DirectThreshold: cfg.Intent.Thresholds.Direct,
		AskThreshold:    cfg.Intent.Thresholds.Ask,
	}))
	displayProvider, displayModel, _ := ResolveModelDisplay(cfg)
	gw.SetModelDisplay(displayProvider, displayModel)
	bridge := channel.NewBridge(gw)

	return &GatewayDeps{
		IdentityMapper: idMapper,
		TaskManager:    taskMgr,
		CronScheduler:  cronSched,
		Gateway:        gw,
		Bridge:         bridge,
	}, nil
}

// RegisterCronTool registers the cron tool with the dispatcher if the
// cron scheduler is running.
func RegisterCronTool(disp *tools.Dispatcher, cronSched *cron.Scheduler) {
	if cronSched == nil {
		return
	}
	cronTool := cron.NewCronTool(cronSched)
	disp.RegisterTool(&cron.ToolAdapter{CronTool: cronTool})
}

// StartCron starts the scheduler loop. Call it after any JobExecutor is
// installed (gateway path) — or directly (CLI-only path, where jobs degrade to
// the marker fallback). Safe to call with a nil scheduler.
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
