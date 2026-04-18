package main

import (
	"encoding/json"
	"fmt"
	"os"

	"selfmind/internal/app"
	"selfmind/internal/gateway/cli"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/config"
	"selfmind/internal/tools"
)

func main() {
	cfg, err := config.LoadConfig()
	if err != nil {
		cfg = &config.Config{}
	}

	mem, dataDir, err := app.InitStorage(cfg)
	if err != nil {
		println("[FATAL] app.InitStorage:", err.Error())
		os.Exit(1)
	}

	agent, err := app.InitAgent(mem, cfg)
	if err != nil {
		println("[FATAL] app.InitAgent:", err.Error())
		os.Exit(1)
	}

	disp, err := app.InitTools(mem, cfg, agent)
	if err != nil {
		println("[FATAL] app.InitTools:", err.Error())
		os.Exit(1)
	}

	agent.SetBackend(disp)

	gwDeps, err := app.InitGateway(dataDir, mem, agent, cfg)
	if err != nil {
		println("[FATAL] app.InitGateway:", err.Error())
		os.Exit(1)
	}
	app.RegisterCronTool(disp, gwDeps.CronScheduler)

	app.InitMCP(disp, cfg)

	ctrl := cli.NewControllerWithGateway(gwDeps.Gateway, agent, nil, cfg.Agent.Provider, cfg.Agent.Model)
	ctrl.SetSessionSearchFn(mem.SearchFn("default"))

	memFn := func() (*memory.MemoryManager, string, string) { return mem, "user1", "cli" }
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
