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
// the skill loader, and injects the session search function.
func InitTools(mem *memory.MemoryManager, cfg *config.Config, ag *kernel.Agent) (*tools.Dispatcher, error) {
	disp := tools.NewDispatcher()

	// 1. 注册认证中间件（从持久化层加载权限）
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

	// Register Approval Middleware
	root, _ := os.Getwd()
	disp.InjectMiddleware(tools.SmartApprovalMiddleware(root))

	// Register Vision LLM
	disp.InjectVisionLLM(ag)

	return disp, nil
}
