package main

import (
	"encoding/json"
	"fmt"
	"os"

	"selfmind/internal/app"
	"selfmind/internal/gateway/cli"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/config"
	"selfmind/internal/platform/log"
	"selfmind/internal/tools"
)

func main() {
	cfg, err := config.LoadConfig()
	if err != nil {
		cfg = &config.Config{}
	}

	log.Init(log.Options{Level: cfg.Agent.LogLevel})

	mem, dataDir, err := app.InitStorage(cfg)
	if err != nil {
		log.Fatal("app.InitStorage failed", "error", err)
	}

	agent, err := app.InitAgent(mem, cfg)
	if err != nil {
		log.Fatal("app.InitAgent failed", "error", err)
	}

	skillStore := kernel.NewSkillStore(mem)
	disp, err := app.InitTools(mem, cfg, agent, skillStore)
	if err != nil {
		log.Fatal("app.InitTools failed", "error", err)
	}

	agent.SetBackend(disp)

	gwDeps, err := app.InitGateway(dataDir, mem, agent, cfg, skillStore)
	if err != nil {
		log.Fatal("app.InitGateway failed", "error", err)
	}
	app.RegisterCronTool(disp, gwDeps.CronScheduler)

	app.InitMCP(disp, cfg)

	// CLI 默认租户：从环境变量读取，支持多实例隔离
	tenantID := os.Getenv("SELF_TENANT_ID")
	if tenantID == "" {
		tenantID = "user1"
	}

	ctrl := cli.NewControllerWithGateway(gwDeps.Gateway, agent, nil, cfg.Agent.Provider, cfg.Agent.Model, cfg)
	ctrl.SetSessionSearchFn(mem.SearchFn("default"))

	memFn := func() (*memory.MemoryManager, string, string) { return mem, tenantID, "cli" }
	msgFn := func() ([]byte, error) {
		msgs, err := cli.GetCheckpointMessages()
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
		app.StopCron(gwDeps.CronScheduler)
		if mem != nil {
			mem.Close()
		}
	})

	ctrl.Start()
	fmt.Println("Goodbye!")
	os.Exit(0)
}
