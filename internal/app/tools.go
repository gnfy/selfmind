package app

import (
	"os"
	"path/filepath"
	"strings"

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
	registerConfiguredSecrets(cfg)
	kernel.SetAgentEventRedactor(tools.RedactSensitive)

	// Redaction is outermost so every model/event/artifact result surface sees
	// the masked form, including errors returned by inner policy middleware.
	disp.InjectMiddleware(tools.RedactionMiddleware())
	// 1. Register auth middleware (load permissions from persistent layer)
	disp.InjectMiddleware(tools.AuthMiddleware(mem))
	disp.InjectMiddleware(tools.WorkspaceScopeMiddleware())
	disp.InjectMiddleware(tools.NewToolGuardrails().Middleware)
	disp.InjectMiddleware(tools.ExecutionCapabilityMiddleware())
	disp.InjectMiddleware(tools.EvidenceMiddleware())

	tools.RegisterBuiltins(disp)
	// Exec sandbox (P0-D): process-wide Linux policy. Auto mode prefers bwrap;
	// unavailable best-effort isolation is reported as a host fallback, while
	// required mode fails closed. Explicit host calls remain approval-gated.
	tools.SetExecSandbox(cfg.ExecSandbox.Enabled, cfg.ExecSandbox.Required, cfg.ExecSandbox.AllowNetwork)
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

// registerConfiguredSecrets feeds opaque daemon-owned credentials into the
// shared output redactor. Registration stores values in memory only; it does
// not make credentials available to tool child processes.
func registerConfiguredSecrets(cfg *config.Config) {
	if cfg == nil {
		return
	}
	registerEndpoint := func(endpoint config.ProviderEndpoint) {
		tools.RegisterSensitiveValue(endpoint.APIKey)
		registerSensitiveHeaders(endpoint.Headers)
	}
	registerEndpoint(cfg.Providers.OpenAI)
	registerEndpoint(cfg.Providers.Anthropic)
	registerEndpoint(cfg.Providers.Google)
	for _, endpoint := range cfg.ProviderProfiles {
		registerEndpoint(endpoint)
	}
	for _, provider := range cfg.Providers.Custom {
		tools.RegisterSensitiveValue(provider.APIKey)
	}
	for _, role := range cfg.Models.Roles {
		tools.RegisterSensitiveValue(role.APIKey)
		registerSensitiveHeaders(role.Headers)
	}
	registerSensitiveHeaders(cfg.Model.Headers)
	for _, value := range []string{
		cfg.Providers.AnthropicAPIKey,
		cfg.Providers.OpenAIAPIKey,
		cfg.Providers.OpenRouterAPIKey,
		cfg.Providers.GeminiAPIKey,
		cfg.Providers.MiniMaxAPIKey,
		cfg.Gateway.Token,
		cfg.Gateway.OutboundWebhookToken,
		cfg.Gateway.TelegramToken,
		cfg.Gateway.Weixin.Token,
		cfg.Gateway.Wechat.AppSecret,
		cfg.Gateway.Wechat.Token,
		cfg.Gateway.Feishu.AppSecret,
		cfg.Gateway.Feishu.VerificationToken,
		cfg.Gateway.Feishu.EncryptKey,
		cfg.Gateway.QQ.Secret,
		cfg.Gateway.QQ.Token,
		cfg.Delegation.APIKey,
		cfg.Web.APIKey,
	} {
		tools.RegisterSensitiveValue(value)
	}
}

func registerSensitiveHeaders(headers map[string]string) {
	for name, value := range headers {
		lower := strings.ToLower(strings.TrimSpace(name))
		if strings.Contains(lower, "authorization") ||
			strings.Contains(lower, "token") ||
			strings.Contains(lower, "api-key") ||
			strings.Contains(lower, "apikey") ||
			strings.Contains(lower, "secret") ||
			strings.Contains(lower, "cookie") {
			tools.RegisterSensitiveValue(value)
			fields := strings.Fields(value)
			if len(fields) == 2 {
				switch strings.ToLower(fields[0]) {
				case "bearer", "basic", "token":
					tools.RegisterSensitiveValue(fields[1])
				}
			}
		}
	}
}
