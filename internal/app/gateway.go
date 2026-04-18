package app

import (
	"context"
	"database/sql"
	"fmt"

	"selfmind/internal/gateway/channel"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/identity"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/kernel/task"
	"selfmind/internal/kernel/task/cron"
	"selfmind/internal/platform/config"
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
		cronDB, err := sql.Open("sqlite", dataDir+"/cron.db")
		if err != nil {
			return nil, fmt.Errorf("open cron db: %w", err)
		}
		cronSched = cron.NewScheduler(cronDB, mem)
		if err := cronSched.InitSchema(context.Background()); err != nil {
			return nil, fmt.Errorf("init cron schema: %w", err)
		}
		if err := cronSched.Start(context.Background()); err != nil {
			return nil, fmt.Errorf("start cron scheduler: %w", err)
		}

	}

	gw := router.NewGateway(idMapper, taskMgr, agent, nil)
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

// StopCron gracefully shuts down the cron scheduler.
func StopCron(cronSched *cron.Scheduler) {
	if cronSched != nil {
		cronSched.Stop(context.Background())
	}
}
