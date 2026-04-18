# SelfMind Refactoring Roadmap

## Phase 1: Core Kernel (Reasoning & Context)
- [x] Initialize Go module structure (`go.mod`, directories)
- [x] Implement `Agent` struct (core execution loop)
- [x] Implement `ContextEngine` (prompt building, token management)
- [x] Setup `SQLite` memory provider (Tenant-isolated storage)

## Phase 2: Tool System (Foundation)
- [x] Define `Tool` interface and registry system
- [x] Implement `Middleware` chain (Auth -> Permission -> Approval)
- [x] Port `file_tools.py` as a reference
- [ ] Port `approval.py` for safety checks (stub exists, not full dangerous-command detection)

## Phase 3: Gateway & Platform (Messaging)
- [x] Define `PlatformAdapter` interface
- [x] Integrate `BubbleTea` for CLI TUI
- [x] Setup Config & Logger (Viper/Slog)

## Phase 4: Integration & QA
- [x] Port unit tests from `tests/`
- [x] Run end-to-end integration tests (CLI loop)
- [ ] Validate performance (concurrent tool execution)

---

## Status Summary (as of 2026-04-15)

### Completed
- Build: `CGO_ENABLED=0` static binary (16MB), zero vet warnings, all tests passing
- LLM Adapters: Real HTTP calls to Anthropic/OpenAI/OpenRouter APIs
- Memory: Multi-tenant SQLite with trajectory storage; `SaveTrajectory` fully wired
- Tools: 6 built-in tools (list_files, read_file, write_file, execute_command, search_files, get_current_time)
- Middleware: Auth/TenantIsolation/Approval/RateLimit/Logging chains fully implemented
- SkillLoader: YAML front matter parser supports arrays (trigger, parameters, examples)
- ReflectionEngine: LLM-driven skill archiving with name-extracted filenames
- PlatformAdapter: CLI and Mock implementations
- ContextEngine: Token management, message truncation, tool definition formatting
- TUI: Bubble Tea controller with sidebar/status/editor components

### P0/P1/P2 — All Cleared

### Remaining (P3)
- Config-driven startup (read API keys and settings from config.yaml instead of hardcoded mock values)
- Phase 4: Concurrent tool execution performance validation

### New: Multi-Channel Architecture (2026-04-15)
- [x] Channel-isolated history: `trajectories` table + `channel` column
- [x] `IdentityMapper`: platform+platformID → unified_uid binding
- [x] `TaskManager`: global tasks with `current_task` pointer + casual_summaries
- [x] `IntentClassifier`: lightweight rule-based intent routing (Task/Casual/Continue) with ~70 regex/keyword patterns
- [x] StorageProvider/MemoryManager/ContextEngine/Agent signatures updated with `channel` param
- [x] `Gateway`: unified message handler integrating identity + intent + task + agent
- [x] `NewControllerWithGateway`: CLI controller now supports full gateway routing
- [x] `/tasks` slash command: list global tasks from any channel
- [x] `wechat.Adapter`: WeChat message adapter skeleton (platform binding via openid)

### UI Improvements (2026-04-15)
- [x] common.go: complete styles rewrite with crush-inspired dark theme palette
- [x] Chat bubbles: UserBubble (green) / AssistantBubble (surface) / ToolBubble (bordered)
- [x] Sidebar: semantic item/title/muted styles with panel border
- [x] Status bar: semantic label/value/good/warning/error styles
- [x] Editor: Editor.Panel wrapper for proper layout composition
- [x] Welcome screen: plain string, not lipgloss.Style (editor compatibility)
- [x] `st.Main` field added to Styles struct with default assignment

### Config-Driven Startup (2026-04-16)
- [x] `config.yaml` now fully wired: API keys, agent max_iterations, evolution settings, MCP servers, delegation
- [x] `EvolutionConfig` fields (`Enabled`, `Mode`, `MinComplexityThreshold`, `AutoArchiveConfidence`) read from config
- [x] `DelegationConfig` added: `provider`, `model`, `api_key` for sub-agent delegation
- [x] `MCPConfig` added: `servers[]` array; each server has `name`, `transport`, `command`, `args`, `url`, `headers`, `auth`, `env_filter`
- [x] Graceful shutdown: SIGINT/SIGTERM triggers `mem.Close()` before exit
- [x] `MemoryManager.Close()` method added (interface assertion pattern)

### MCP Client Integration (2026-04-16)
- [x] `MCPToolManager` starts all configured MCP servers on boot
- [x] Discovered tools auto-registered to dispatcher via `WrapTool()`
- [x] Supports both `stdio` and `http` transport
- [x] Config wired in `main.go` via `mcpManager.Connect()` loop

### Delegate Tool (2026-04-16)
- [x] `DelegateTool.Execute()` wired via `disp.InjectDelegateFn()`
- [x] `makeDelegateFn()` spawns `hermes chat -q` subprocess with goal + context
- [x] Respects `delegation.provider`, `delegation.model`, `delegation.api_key` from config.yaml
- [x] Passes `toolsets` as `--toolsets` CLI arg to hermes subprocess
