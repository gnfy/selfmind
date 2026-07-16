package app

import (
	"os"
	"path/filepath"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/config"
	"selfmind/internal/tools"
)

// InitTools wires up the dispatcher, built-in tools, extended tools,
// the skill loader, the skill metrics middleware, and injects the session search function.
func InitTools(mem *memory.MemoryManager, cfg *config.Config, ag *kernel.Agent, skillStore *kernel.SkillStore, tenantID string, controlStores ...*control.Store) (*tools.Dispatcher, error) {
	registry := tools.NewRegistry()
	disp := tools.NewDispatcherWithRegistry(registry)
	if tenantID == "" {
		tenantID = "default"
	}

	// 1. Register auth middleware (load permissions from persistent layer)
	disp.InjectMiddleware(tools.AuthMiddleware(mem))
	disp.InjectMiddleware(tools.WorkspaceScopeMiddleware())
	disp.InjectMiddleware(tools.NewToolGuardrails().Middleware)
	disp.InjectMiddleware(tools.EvidenceMiddleware())

	tools.RegisterBuiltins(disp)
	tools.RegisterExtendedTools(disp, tools.WebSearchOptions{
		Backend: cfg.Web.SearchBackend,
		APIKey:  cfg.Web.APIKey,
	})
	if len(controlStores) > 0 && controlStores[0] != nil {
		disp.RegisterTool(tools.NewExternalWatchTool(controlStores[0]))
	}
	// Read-back for spooled large tool outputs (W1): the base dir must match
	// the gateway sink's spool dir, both derived from the same resolved data
	// dir, so every dispatcher (daemon, worker pool, eval) can resolve the
	// artifacts the coordinator writes.
	disp.RegisterTool(tools.NewToolOutputViewTool(filepath.Join(ResolveDataDir(cfg), "tool-output")))
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
