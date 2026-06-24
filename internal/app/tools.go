package app

import (
	"os"

	"selfmind/internal/kernel"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/config"
	"selfmind/internal/tools"
)

// InitTools wires up the dispatcher, built-in tools, extended tools,
// the skill loader, the skill metrics middleware, and injects the session search function.
func InitTools(mem *memory.MemoryManager, cfg *config.Config, ag *kernel.Agent, skillStore *kernel.SkillStore, tenantID string) (*tools.Dispatcher, error) {
	registry := tools.NewRegistry()
	disp := tools.NewDispatcherWithRegistry(registry)
	if tenantID == "" {
		tenantID = "default"
	}

	// 1. Register auth middleware (load permissions from persistent layer)
	disp.InjectMiddleware(tools.AuthMiddleware(mem))
	disp.InjectMiddleware(tools.WorkspaceScopeMiddleware())
	disp.InjectMiddleware(tools.NewToolGuardrails().Middleware)

	tools.RegisterBuiltins(disp)
	tools.RegisterExtendedTools(disp)
	disp.RegisterTool(tools.NewSkillManageTool())
	disp.RegisterTool(tools.NewSkillsListTool())
	disp.RegisterTool(tools.NewSkillViewTool())
	disp.RegisterTool(tools.NewSkillBundleTool())
	disp.RegisterTool(tools.NewSkillCatalogTool())

	if mem != nil {
		disp.InjectSessionAccess(mem.SearchFn(tenantID), mem.RecentSessionsFn(tenantID), mem.SessionMessagesFn(tenantID))
		disp.InjectTenantSessionAccess(
			func(tenantID, query string, limit int) (interface{}, error) {
				return mem.SearchSessions(tenantID, query, limit)
			},
			func(tenantID string, limit int) (interface{}, error) {
				return mem.ListRecentSessions(tenantID, limit)
			},
			func(tenantID, sessionID string, aroundMessageID, window int) (interface{}, error) {
				return mem.GetSessionMessages(tenantID, sessionID, aroundMessageID, window)
			},
		)
		disp.RegisterTool(tools.NewMemoryTool(mem))
		tools.GetProcessRegistryForTenant(tenantID).Init(mem, tenantID)
	}

	_, _ = tools.ReloadSkillToolsForTenant(tenantID, registry)

	disp.InjectDelegateFn(MakeDelegateFn(mem, disp, cfg.Delegation))
	disp.InjectDelegateBatchFn(MakeDelegateBatchFn(mem, disp, cfg.Delegation))

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
