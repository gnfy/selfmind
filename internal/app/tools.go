package app

import (
	"os"
	"path/filepath"

	"selfmind/internal/kernel"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/config"
	"selfmind/internal/tools"
)

// InitTools wires up the dispatcher, built-in tools, extended tools,
// the skill loader, the skill metrics middleware, and injects the session search function.
func InitTools(mem *memory.MemoryManager, cfg *config.Config, ag *kernel.Agent, skillStore *kernel.SkillStore) (*tools.Dispatcher, error) {
	disp := tools.NewDispatcher()

	// 1. Register auth middleware (load permissions from persistent layer)
	disp.InjectMiddleware(tools.AuthMiddleware(mem))

	tools.RegisterBuiltins(disp)
	tools.RegisterExtendedTools(disp)

	if mem != nil {
		disp.InjectSessionSearch(mem.SearchFn("default"))
		disp.RegisterTool(tools.NewMemoryTool(mem))
		tools.GetProcessRegistry().Init(mem)
	}

	home, _ := os.UserHomeDir()
	skillsDir := filepath.Join(home, ".selfmind", "skills")
	skillLoader := tools.NewSkillLoader(skillsDir, tools.GlobalRegistry())
	skillLoader.LoadAll()

	disp.InjectDelegateFn(MakeDelegateFn(mem, disp, cfg.Delegation))

	// 2. Register approval middleware
	root, _ := os.Getwd()
	disp.InjectMiddleware(tools.SmartApprovalMiddleware(root))

	// 3. Register Vision LLM
	disp.InjectVisionLLM(ag)

	// 4. Register skill metrics middleware (tracks call/fail counts for skill:* tools)
	if skillStore != nil {
		disp.InjectMiddleware(tools.SkillMetricsMiddleware(skillStore))
	}

	return disp, nil
}
