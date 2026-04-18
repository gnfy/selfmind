# SelfMind vs Hermes Agent — Feature Comparison

> Reference: Hermes Agent (Python) implementation at `~/.hermes/hermes-agent/`
> Updated: 2026-04-18

---

## Legend

| Symbol | Meaning |
|--------|---------|
| ✅ | Implemented in selfmind |
| ⚠️ | Partial / stub — needs completion |
| ❌ | Not yet implemented |

---

## Core Agent Loop

| Feature | Hermes | SelfMind | Notes |
|---------|--------|----------|-------|
| Synchronous reasoning loop | ✅ `run_agent.py` | ✅ `kernel/agent.go` | Same pattern: call LLM → extract tool calls → execute → repeat |
| Max iterations budget | ✅ `max_iterations` | ✅ `max_iterations` | |
| Tool call extraction | ✅ regex on response | ✅ `ExtractToolCalls()` in `tool_call.go` | |
| Trajectory saving | ✅ `trajectory.py` | ✅ `memory.SaveTrajectory()` | |
| Context compression | ✅ `context_compressor.py` | ⚠️ `TruncateMessages()` only | Hermes uses LLM summarization; selfmind uses simple head-tail truncation |
| Prompt caching (Anthropic) | ✅ `prompt_caching.py` | ❌ | Native Anthropic cache headers not wired |
| Iteration budget tracking | ✅ `iteration_budget` | ❌ | No per-turn budget accounting |
| Session store (SQLite FTS5) | ✅ `hermes_state.py` | ✅ `memory/sqlite_provider.go` | |

---

## LLM Adapters

| Feature | Hermes | SelfMind | Notes |
|---------|--------|----------|-------|
| Anthropic | ✅ | ✅ | |
| OpenAI | ✅ | ✅ | |
| OpenRouter | ✅ | ✅ | |
| Azure OpenAI | ✅ | ❌ | |
| Google Vertex | ✅ | ❌ | |
| Bedrock (AWS) | ✅ | ❌ | |
| Groq | ✅ | ❌ | |
| Model routing (smart) | ✅ `smart_model_routing.py` | ❌ | |

---

## Tool Registry & Middleware

| Feature | Hermes | SelfMind | Notes |
|---------|--------|----------|-------|
| Central registry | ✅ `tools/registry.py` | ✅ `tools/dispatcher.go` | |
| Middleware chain | ⚠️ basic | ✅ Auth + TenantIsolation + Approval + RateLimit + Logging | Hermes has richer middleware (approval, token locks) |
| Per-tool availability check | ✅ `check_fn` | ✅ `EnvVarMiddleware` + `AuthMiddleware` | Hermes uses `check_fn` per tool; selfmind uses middleware chain with env var + permission checks |
| Tool schema validation | ✅ | ⚠️ `CoerceArgs()` only | |
| Dangerous command detection | ✅ `approval.py` | ✅ `SmartApprovalMiddleware` | Hermes blocks `rm -rf /`, `fork bombs`, etc.; selfmind detects `rm `, `chmod`, `chown`, `kill`, path traversal via `SmartApprovalMiddleware` + `ClarifyFn` |
| Background process management | ✅ `process_registry.py` | ✅ `process_registry.go` (Recover/Poll/Kill) | Hermes tracks `terminal(background=true)` procs; selfmind has full ProcessRegistry with PID recovery on boot |

---

## Built-in Tools

### File Operations

| Tool | Hermes | SelfMind | Notes |
|------|--------|----------|-------|
| `list_files` | ✅ | ✅ | |
| `read_file` | ✅ | ✅ | |
| `write_file` | ✅ | ✅ | |
| `patch` (apply diff) | ✅ `patch_parser.py` | ✅ `patch.go` (701 lines, V4A format) | Very useful — applies unified diffs |
| `search_files` | ✅ | ✅ | |
| `path_security` (path traversal check) | ✅ | ⚠️ | Basic check in `WriteFileTool` |

### Terminal & Process

| Tool | Hermes | SelfMind | Notes |
|------|--------|----------|-------|
| `execute_command` / `terminal` | ✅ `terminal_tool.py` | ✅ | |
| `background` process tracking | ✅ `process_registry.py` | ✅ `process_registry.go` | Uses ProcessRegistry with PID recovery on boot |
| SSH / Docker / Modal backends | ✅ `environments/` | ❌ | |

### Web

| Tool | Hermes | SelfMind | Notes |
|------|--------|----------|-------|
| `web_search` | ✅ | ✅ | |
| `web_extract` | ✅ | ✅ | |
| Browser automation | ✅ `browser_tool.py` (Browserbase, Firecrawl, BrowserUse) | ⚠️ partial | selfmind has `browser_navigate`, `browser_snapshot`, `browser_click`, `browser_type`, `browser_scroll`, `browser_vision`, `browser_console`, `browser_get_images`, `browser_back`, `browser_press` but no `browser_hold_click`, iframe support, or multi-tab |

### Vision & Media

| Tool | Hermes | SelfMind | Notes |
|------|--------|----------|-------|
| `vision_analyze` | ✅ | ✅ | |
| `image_generate` | ✅ | ❌ | DALL-E / Stable Diffusion integration |
| `text_to_speech` | ✅ | ✅ | |

### Skills

| Tool | Hermes | SelfMind | Notes |
|------|--------|----------|-------|
| `skill_view` | ✅ | ✅ | |
| `skill_manage` | ✅ | ✅ | Hermes supports create/edit/archive; selfmind supports create/patch/edit/delete |
| `skills_list` | ✅ | ⚠️ | `SkillLoader.ListAll()` exists but not registered as a tool |
| Skill hub (browse/install) | ✅ `skills_hub.py` | ❌ | Remote skill registry |

### Memory

| Tool | Hermes | SelfMind | Notes |
|------|--------|----------|-------|
| `memory` (save/recall facts) | ✅ `memory_tool.py` | ❌ | Long-term memory facts with embedding search |
| `session_search` | ✅ | ✅ | FTS5 full-text search |
| Checkpoints | ✅ `checkpoint_manager.py` | ✅ | |

### Planning & Tasks

| Tool | Hermes | SelfMind | Notes |
|------|--------|----------|-------|
| `todo` | ✅ | ✅ | |
| `clarify` (ask user questions) | ✅ `clarify_tool.py` | ✅ stub + TUI callback | selfmind has clarify stub + `clarify_callback.go` + TUI integration; `RegisterClarifyCallback()` wired in controller |
| Goal tracking | ✅ | ❌ | Hermes tracks task goals and progress |

### Code Execution

| Tool | Hermes | SelfMind | Notes |
|------|--------|----------|-------|
| `execute_code` | ✅ | ✅ | |
| `delegate_task` | ✅ | ✅ | |

### Cron

| Tool | Hermes | SelfMind | Notes |
|------|--------|----------|-------|
| `cronjob` | ✅ | ✅ | |

### Platform Messaging

| Tool | Hermes | SelfMind | Notes |
|------|--------|----------|-------|
| `send_message` | ✅ | ❌ | Cross-platform message sending |

### Home Assistant

| Tool | Hermes | SelfMind | Notes |
|------|--------|----------|-------|
| `ha_list_entities` | ✅ | ❌ | |
| `ha_get_state` | ✅ | ❌ | |
| `ha_list_services` | ✅ | ❌ | |
| `ha_call_service` | ✅ | ❌ | |

### Other

| Tool | Hermes | SelfMind | Notes |
|------|--------|----------|-------|
| `interrupt` (pause/resume) | ✅ | ❌ | |
| `mixture_of_agents` | ✅ | ❌ | Advanced reasoning ensemble |
| RL training (`rl_training_tool`) | ✅ | ❌ | |
| `osv_check` (vulnerability scan) | ✅ | ❌ | |

---

## Platform Adapters

| Platform | Hermes | SelfMind | Notes |
|----------|--------|----------|-------|
| CLI / TUI | ✅ `cli.py` (Rich + prompt_toolkit) | ✅ Bubble Tea | Hermes has richer UI (KawaiiSpinner, skin engine) |
| Telegram | ✅ | ✅ `telegram/adapter.go` (webhook + long polling) | selfmind has full adapter with HandleMessage + identity mapping |
| Discord | ✅ | ❌ | |
| Slack | ✅ | ❌ | |
| WhatsApp | ✅ | ❌ | |
| WeChat | ❌ | ⚠️ stub | |
| DingTalk | ❌ | ❌ | |
| Home Assistant | ✅ | ❌ | |
| Signal | ✅ | ❌ | |
| VS Code (ACP) | ✅ `acp_adapter/` | ❌ | |

---

## Agent Capabilities

| Feature | Hermes | SelfMind | Notes |
|---------|--------|----------|-------|
| Reflection / self-evolution | ✅ `ReflectionEngine` | ✅ `reflection.go` | Both analyze task history and archive new skills |
| Skill auto-archive | ✅ | ✅ | |
| Context window management | ✅ `context_engine.py` | ✅ `context_engine.go` | |
| Token estimation | ✅ `model_metadata.py` | ⚠️ rough 4-char/token | Hermes uses per-model exact counts |
| System prompt builder | ✅ `prompt_builder.py` | ⚠️ `BuildSystemPrompt()` | Hermes has soul/manifest分层 |
| Multi-tenant isolation | ✅ profiles | ✅ `tenantID` in all calls | Hermes uses profile-level isolation |

---

## CLI Features

| Feature | Hermes | SelfMind | Notes |
|---------|--------|----------|-------|
| Slash commands | ✅ 20+ commands | ✅ 6+ commands | `/help`, `/status`, `/model`, `/tasks`, `/exit`, `/skills` (skill hub) |
| Command autocomplete | ✅ | ❌ | Tab completion for slash commands |
| Skin/theme engine | ✅ `skin_engine.py` | ❌ | 4 built-in skins + YAML custom |
| Interactive setup wizard | ✅ `setup.py` | ❌ | |
| Model catalog | ✅ `models.py` | ❌ | Provider model lists |
| `/model` switch | ✅ | ⚠️ | |
| `/skills` hub | ✅ | ❌ | |
| `/tools` config | ✅ | ❌ | Per-platform tool enable/disable |

---

## Configuration

| Feature | Hermes | SelfMind | Notes |
|---------|--------|----------|-------|
| YAML config | ✅ | ✅ | |
| Environment variables | ✅ | ⚠️ partial | |
| Config migration | ✅ versioned | ❌ | |
| Profile support (multi-instance) | ✅ | ❌ | |
| Plugin system | ✅ `plugins/` | ❌ | |

---

## Missing in SelfMind (Priority Order)

### P0 — Core parity

1. **`patch` tool** — ✅ Already implemented (V4A format, 701 lines)
2. **`clarify` tool** — ✅ Already implemented (TUI callback wired via `RegisterClarifyCallback()`)
3. **`memory` tool** — Long-term fact storage with semantic search.
4. **Dangerous command approval** — ✅ Already implemented (`SmartApprovalMiddleware` + `ClarifyFn`)

### P1 — Tool parity

5. **Background process tracking** — ✅ Already implemented (`process_registry.go` with Recover/Poll/Kill)
6. **Image generation** — DALL-E / Stable Diffusion tool.
7. **`skills_list` tool** — `SkillLoader.ListAll()` exists, needs registration as tool.
8. **`send_message` tool** — Cross-platform outbound messaging.
9. **Home Assistant tools** — `ha_list_entities`, `ha_get_state`, `ha_list_services`, `ha_call_service`.
10. **SSH/Docker/Modal terminal backends** — Remote execution environments.

### P2 — CLI / UX parity

11. **Skin engine** — Themeable CLI appearance.
12. **Interactive setup wizard** — `hermes setup` equivalent.
13. **Model catalog** — Provider model lists.
14. **Command autocomplete** — Slash command tab completion.
15. **Per-platform tool enable/disable** — `hermes tools` equivalent.

### P3 — Advanced features

16. **Prompt caching** — Anthropic prompt caching headers.
17. **ACP adapter** — VS Code / Zed / JetBrains integration.
18. **Mixture of agents** — Advanced reasoning ensemble.
19. **Browser hold-click / iframe / multi-tab** — Full browser automation.
20. **Profile support** — Multi-instance isolation.

---

## Architecture Differences

| Aspect | Hermes | SelfMind |
|--------|--------|----------|
| Language | Python 3 | Go 1.26+ |
| Build | pip install / source | Static binary (`CGO_ENABLED=0`) |
| Concurrency | async/await (`asyncio`) | Goroutines + channels |
| DB | SQLite (aiosqlite for async) | SQLite (modernc.org/sqlite) |
| UI | Rich + prompt_toolkit | Bubble Tea + Lip Gloss |
| Tool discovery | Import-time registration | Runtime registry |
| Middleware | Decorator chain | Functional chain |
| Config | YAML + .env + profiles | YAML only |
