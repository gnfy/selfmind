# SelfMind Development Guide

[中文开发指南](development-guide.zh-CN.md) · [中文使用说明](../README.zh-CN.md)

This guide documents the current engineering shape of SelfMind after the personal-to-SaaS, single-binary gateway, native tool calling, learning loop, and dynamic model configuration work.

## Product Boundary

SelfMind currently targets a daily-usable personal agent:

- One user-facing binary: `selfmind`.
- Local TUI for interactive work.
- Long-running local gateway for 24/7 task execution.
- CLI, IM, and webhook channels resolve to the same person through gateway identity binding.
- Channel chat logs are isolated; task/run/workspace/memory/skill state is shared.
- Personal mode reads model/provider policy from local `config.yaml`.
- Future SaaS mode should resolve the same model/provider policy from database-backed tenant/person/workspace stores.

`selfmind` is the single binary. Run the daemon as `selfmind gateway run`.

## Directory Map

```text
cmd/selfmind/              user-facing binary entrypoint
internal/cliapp/           top-level CLI app router
  root.go                  global -f/--config handling and mode dispatch
  model_commands.go        selfmind model picker/list/set/current
  gateway_commands.go      selfmind gateway run/start/status/stop/restart
  client_commands.go       send/status/tasks/workspace gateway client commands
internal/app/              application wiring
  agent.go                 provider construction, model gateway, review engines
  storage.go               storage bootstrap
  tools.go                 tool registration and middleware
internal/platform/config/  YAML config schema, defaults, compatibility, save
internal/kernel/           agent loop, memory, review, native tool calls
internal/kernel/llm/       provider adapters and role-based model gateway
internal/modelruntime/     provider profiles, credential resolution, model catalog/cache
internal/tools/            built-in tools and tool middleware
internal/gateway/          TUI, HTTP API, router, delivery, channel adapters
internal/control/          control.db identity/workspace/task/run state
internal/runtime/gateway/  gateway process runner, pid, lock, state, start/stop
packaging/                 Linux package scripts and systemd templates
docs/                      architecture and development docs
```

Dependency direction should stay simple:

- `kernel` must not depend on `tools`, `gateway`, or `server`.
- `kernel.Agent` uses `AgentBackend` for tools.
- `app` wires concrete storage, LLM providers, tools, memory, and gateway.
- `cliapp` owns command routing and user-facing CLI behavior.
- `gateway/httpapi` owns HTTP request handling and model-free control commands.

## Architecture Constraints

Future development and AI-assisted edits must read:

- [SelfMind Architecture Constraints](architecture-constraints.md)
- [SelfMind 架构约束](architecture-constraints.zh-CN.md)
- [SelfMind 上下文生命周期与 P0-P2 落地](context-lifecycle.zh-CN.md)

Key rules:

- `internal/gateway/cli/controller.go` only orchestrates TUI state. Do not keep adding large UI logic there.
- Temporary pages such as `/help`, detail pages, list pages, and search pages should use `internal/ui/components/Pager` or a similar reusable surface.
- Slash command dispatch, help text, and editor hints should move toward one shared registry.
- Avoid new global mutable state shared across tenants or tests.
- New providers, tools, HTTP handlers, and TUI components should be split by responsibility instead of being packed into existing large files.
- Durable context selection must go through `gateway/httpapi/context_selector.go` and `kernel.TaskRuntimeContext`; do not append raw task events, artifacts, or memory snippets directly in unrelated handlers.

## Configuration

Default config path:

```text
~/.selfmind/config.yaml
```

Global custom config path:

```sh
selfmind -f ./config/config.yaml
selfmind --config ./config/config.yaml gateway start
```

Environment config path:

```sh
SELF_CONFIG=/etc/selfmind/config.yaml selfmind gateway run
```

SelfMind does not require `.env`. Environment variable expansion is supported inside YAML values, for example `${OPENAI_API_KEY}`.

### Current YAML Schema

```yaml
model:
  provider: "openai"
  default: "gpt-4o"

providers:
  openai:
    api_key: "${OPENAI_API_KEY}"
    base_url: "https://api.openai.com/v1"
    protocol: "openai_chat"
  anthropic:
    api_key: "${ANTHROPIC_API_KEY}"
    base_url: "https://api.anthropic.com"
    protocol: "anthropic_messages"
  google:
    api_key: "${GOOGLE_API_KEY}"
    base_url: "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"
    protocol: "openai_compatible"
  custom:
    - name: "ollama"
      base_url: "http://localhost:11434/v1"
      api_key: ""
      protocol: "openai_compatible"
      model: "llama3"
      models:
        llama3:
          context_length: 8192

provider_profiles:
  minimax:
    api_key: "${MINIMAX_API_KEY}"
    base_url: "https://api.minimax.io/anthropic"
    protocol: "anthropic_messages"
    model: "MiniMax-M3"
  kimi-coding:
    api_key: "${KIMI_CODING_API_KEY}"
    base_url: "https://api.kimi.com/coding"
    protocol: "anthropic_messages"
    model: "kimi-for-coding"

auth:
  credentials_file: "~/.selfmind/auth.json"

storage:
  type: "sqlite"
  data_dir: "~/.selfmind/data"

gateway:
  addr: "127.0.0.1:8765"
  token: ""
  drain_timeout: "30s"
  outbound_webhook_url: ""
  outbound_webhook_token: ""
  telegram_token: "${TELEGRAM_BOT_TOKEN}"
  delivery_max_message_chars: 3500
  delivery_retry_attempts: 3

models:
  source: "local"
  roles:
    coding_agent:
      provider: "openai"
      model: "gpt-4o"
    memory_extract:
      provider: "google"
      model: "gemini-1.5-flash"
    background_review:
      provider: "google"
      model: "gemini-1.5-flash"
    skill_curator:
      provider: "google"
      model: "gemini-1.5-flash"
    semantic_recall:
      provider: "google"
      model: "gemini-1.5-flash"
```

Compatibility rules in `internal/platform/config/loader.go`:

- Legacy `agent.provider` and `agent.model` are read and normalized into `model.provider` and `model.default`.
- Legacy flat provider keys such as `providers.openai_api_key`, `providers.openrouter_api_key`, and `providers.minimax_api_key` are still read.
- New saves write the nested provider schema or `provider_profiles`, and do not write the legacy flat keys.
- `LoadConfig(config.Options{Path: ...})` accepts explicit paths.
- `LoadConfig(config.Options{Path: ..., CreateIfMissing: true})` is used by commands that can initialize a new file, such as `selfmind model`.

## Model Provider System

Read [SelfMind Provider Runtime](provider-runtime.md) before adding or changing model providers. Provider integration now follows the `ProviderProfile + Resolver + Adapter + ProviderQuirks` boundary. Kimi, MiniMax, OpenAI, Anthropic, OpenRouter, Google, DeepSeek, Z.AI, and Alibaba Coding Plan all use the same pattern.

User-facing command:

```sh
selfmind model
```

Interactive flow:

1. Choose Main, Background, or an optional background-role override.
2. Choose a provider and model; prompt for a missing API key when required.
3. Validate the completed selection automatically, using live discovery with cache and static fallbacks for model choices.
4. Review all edits and apply them as one daemon-owned transaction.
4. Let the user choose a model or enter one manually.
5. Save to `config.yaml`.

Implemented provider protocol families:

| Provider | Protocol | Adapter |
|---|---|---|
| OpenAI | Chat Completions / native tools | `llm.OpenAIAdapter` |
| Anthropic | Messages API | `llm.AnthropicAdapter` |
| Google | OpenAI-compatible Gemini endpoint | `llm.GeminiAdapter` |
| Codex CLI | Responses-compatible Codex endpoint | `llm.ResponsesAdapter` |
| Claude Code | Anthropic Messages with external OAuth | `llm.AnthropicAdapter` |
| Gemini CLI | OpenAI-compatible Gemini endpoint with external OAuth | `llm.GeminiAdapter` |
| Qwen CLI | OpenAI-compatible endpoint with external OAuth | `llm.GenericOpenAIAdapter` |
| Provider profile | OpenAI-compatible, Anthropic Messages, or Responses | selected by `modelruntime.Runtime.Protocol` |
| Custom | OpenAI-compatible endpoint | `llm.GenericOpenAIAdapter` |

Most new vendors should use either `provider_profiles` or a built-in `ProviderProfile` on an existing OpenAI-compatible or Anthropic-compatible protocol family. Do not hardcode vendor logic in app, CLI, or IM adapters. Add a new Go adapter only for a genuinely different protocol family.

Implementation boundary:

- `internal/modelruntime/profile.go` owns built-in provider metadata, aliases, protocol family, env var names, model-list mode, and fallback models.
- `internal/modelruntime/resolver.go` converts YAML, env vars, SelfMind auth JSON, and selected external CLI auth into a resolved `Runtime`.
- `internal/modelruntime/catalog.go` fetches/caches model lists. It must not construct LLM adapters.
- `internal/app/agent.go` only converts a resolved `Runtime` into an adapter and wires role routing. Do not add provider-specific credential discovery there.
- `ProviderQuirks` carries provider-specific wire behavior such as auth header, tool schema, thinking mode, system message mode, and User-Agent.
- `internal/cliapp/model_commands.go` owns the user-facing provider/model picker. Keep `Custom endpoint (enter URL manually)` as the fourth option for backwards-compatible scripted input.

External auth reuse is P2 and intentionally limited to Codex CLI, Claude Code, Gemini CLI, Qwen CLI, and SelfMind-owned OAuth providers such as MiniMax OAuth. `Runtime.TokenGetter` is the per-request token source; `Runtime.TokenRefresher` is the force-refresh hook that protocol adapters may call once after a provider returns an auth failure. Do not add best-effort reuse for random vendor apps unless there is a stable local auth format and a product decision to support it.

### Role-Based Model Routing

`internal/kernel/llm/model_gateway.go` contains the lightweight role router. The current role names are stable and should be reused in future SaaS model policy storage:

- `coding_agent`
- `memory_extract`
- `background_review`
- `skill_curator`
- `semantic_recall`

`internal/app/agent.go` builds the default provider and role-specific providers. If a role cannot be constructed, it falls back to the default provider.

Future SaaS direction:

- Introduce a model policy store interface.
- Personal mode implementation reads local YAML.
- SaaS implementation reads tenant/person/workspace/provider policy from DB.
- Adapters should receive resolved provider config, not reach into global YAML directly.

## Gateway Runtime

User commands:

```sh
selfmind gateway run
selfmind gateway start
selfmind gateway status
selfmind gateway stop
selfmind gateway restart
```

Implementation:

- `internal/cliapp/gateway_commands.go` parses user commands.
- `internal/runtime/gateway/runner.go` owns initialization, HTTP server, signals, shutdown, and runtime status.
- `internal/runtime/gateway/client.go` starts detached child processes.
- `internal/runtime/gateway/state.go` manages pid/state/lock files.

Runtime files live under:

```text
<storage.data_dir>/gateway/
  gateway.pid
  gateway_state.json
  gateway.lock
  gateway.log
```

Gateway config is read from YAML:

```yaml
gateway:
  addr: "127.0.0.1:8765"
  token: ""
  drain_timeout: "30s"
```

`SELF_GATEWAY_*` environment variables remain compatible and override only the current process environment. Old `SELF_DAEMON_*` names are still supported for compatibility, but `SELF_GATEWAY_*` wins.

Gateway HTTP control endpoints:

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/v1/gateway/status` | process state and active runs |
| `POST` | `/v1/gateway/shutdown` | graceful draining shutdown |
| `GET` | `/v1/tasks/events` | recent events for the current or specified task |

Shutdown enters draining state, rejects new runs, waits for active runs, and can be force-stopped by CLI when requested.
The shutdown endpoint flushes its complete acceptance response before allowing
the runtime owner to close the HTTP server. For upgrade compatibility, the CLI
also reconciles a lost response only when the request was fully written and the
local runtime receipt proves that the original owner disappeared or was
replaced; an EOF while that owner remains is still an error.

## Identity, Tasks, Workspaces

`internal/control/store.go` owns the local control-plane database.

Concepts:

```text
tenant_id       personal default or SaaS tenant
person_id       same human across all channels
account_id      platform account binding
workspace_id    project/work directory
task_id         durable task state
run_id          one agent execution attempt
event_id        auditable task transition
```

Runtime state rules:

- `task_runs.heartbeat_at` is refreshed every 10 seconds; `MarkInterruptedRuns` marks stale running runs as `interrupted` after a gateway restart.
- `task_events` stores events such as `run.started`, `tool.started`, `tool.completed`, `learning.review`, `learning.memory.saved`, `learning.skill.updated`, `approval.approved`, `approval.rejected`, `run.finished`, and `run.cancelled`.
- `approval_requests` stores durable approval records. Gateway control commands and HTTP handlers should read/write this table rather than storing approval state in IM adapters.
- `outbound_messages` is the IM delivery queue. Without a sender, messages remain `pending`; with a sender, the delivery worker retries them.
- `/status` is for a compact task card backed by `api.RunOutcome` and the latest handoff; `/events` is for recent runtime events.
- Run completion should save a structured handoff: summary, done items, next steps, changed files, test status, and risks. Do not make IM or CLI status cards parse arbitrary assistant prose directly.

Workspace-scoped tools must honor `allowed_roots`. Do not bypass `WorkspaceScopeMiddleware` for file, search, patch, terminal, or process tools.

## Tool Calling

SelfMind follows a native-first tool-call contract:

- `ChatRequest.Tools` is sent to OpenAI-compatible providers as native `tools`.
- Streaming OpenAI-compatible responses accumulate `delta.tool_calls`.
- Assistant tool calls are stored in `Message.ToolCalls`.
- Tool result messages use `Message.ToolCallID` and `Message.Name`.
- If native tools are rejected, OpenAI-compatible adapters retry without tools and the agent falls back to the legacy `[TOOL:name:{...}]` pattern.
- Only explicitly read-only batches are parallelized. Mutating, terminal, memory, skill, delegation, process-control, and patch tools run sequentially.

Key files:

- `internal/kernel/native_tool_call.go`
- `internal/kernel/agent.go`
- `internal/kernel/llm/adapters.go`
- `internal/tools/dispatcher.go`
- `internal/tools/guardrails.go`
- `internal/tools/security.go`

Tool stability rules:

- `ToolGuardrails` blocks repeated same-tool/same-argument failures within a run.
- Read-only tools also get no-progress detection when the same arguments keep returning the same result.
- Tool logs, gateway delivery errors, and event payloads should pass through `tools.RedactSensitive`.
- Mutating, terminal, memory, skill, delegation, process-control, and patch tools must stay sequential and should not be added to the parallel-safe allowlist.

### Tool Runtime Isolation

New application paths should create an isolated registry:

```go
registry := tools.NewRegistry()
dispatcher := tools.NewDispatcherWithRegistry(registry)
```

Rules:

- `tools.NewDispatcher()` and `tools.GlobalRegistry()` are legacy compatibility paths only.
- Per-tenant, per-workspace, MCP, and dynamically loaded skill tools should be registered through the active dispatcher/registry.
- `MCPToolManager` must connect and disconnect tools through its dispatcher, not the global registry.
- `Registry.Dispatch` and `Dispatcher.Dispatch` both coerce and validate arguments before execution.
- Argument coercion is intentionally strict: malformed integers, numbers, and booleans fail fast.
- `ClarifyFn` remains as a TUI compatibility fallback, but new approval/clarify integrations should inject a handler through the dispatcher registry.

### Agent Concurrency

An `Agent` instance is still serialized with `runMu`, but gateway event streaming no longer mutates the shared `Agent.EventChannel`. Gateway paths must install a per-run event sink with `kernel.WithEventChannel(ctx, ch)` and then consume the resulting stream events through `router.HandleWithEvents`.

`Agent.EventChannel` remains a legacy fallback for local TUI paths. Do not temporarily replace it in gateway, IM, or future Web code. Gateway paths that need true parallel active runs should construct separate agent instances per run or introduce a worker pool after the per-run event sink contract is preserved.

`syncTurn` uses a bounded background queue instead of spawning an unbounded goroutine for every assistant turn. High-frequency conversations should prefer dropping stale sync snapshots over piling up memory-sync workers.

## Learning Loop

The intended "gets smarter with use" loop is:

```text
conversation/task
  -> memory and session persistence
  -> skill usage and metrics
  -> final response
  -> background review
  -> memory save / skill create / skill patch / skip
  -> curator lifecycle
```

Current pieces:

- `internal/kernel/fact_extractor.go`
- `internal/kernel/turn_extractor.go`
- `internal/kernel/reflection.go`
- `internal/kernel/background_review.go`
- `internal/tools/learning_audit.go`
- `internal/tools/skill_manage.go`
- `internal/tools/skill_runtime.go`
- `internal/tools/skill_bundles.go`
- `internal/tools/skill_catalog.go`
- `internal/tools/skill_curator.go`
- `internal/tools/session_search.go`
- `internal/kernel/memory/sqlite_provider.go`

Rules to preserve:

- Stable user preferences and durable project facts belong in memory.
- Reusable workflows, checklists, and procedures belong in skills.
- Temporary task progress should not become long-term memory.
- If a used skill is stale or corrected by the user, patch that skill instead of creating duplicates.
- Transient provider outages, one-off tool failures, and speculative failure causes should not become memory or skills.
- New skills created by the agent should use `source=agent-created`; session-specific details belong in `references/`, not the main `SKILL.md`.
- Curator should manage only agent-created skills unless explicitly told otherwise.
- Skill discovery should be progressive: use `skills_list` for compact metadata, `skill_view` for full `SKILL.md` or linked files, and `skill_manage` only for mutation.
- Skill mutation should hot-reload the active registry when possible. Do not require a restart after `create`, `install`, `archive`, or `restore`.
- Skill slash commands use bundle-first resolution, then skill resolution. Bundles live under `~/.selfmind/<tenant>/skill-bundles/`.
- Catalog installs are marked `catalog-installed` and tracked in `~/.selfmind/<tenant>/skills/.catalog/lock.json`; curator must not auto-govern them unless explicitly marked `agent-created`.
- Catalog install must reject existing directory and legacy `.md` collisions unless the user explicitly passes `--force`; force must back up the previous copy under `skills/.catalog/backups/` before replacing it.
- Patch operations should provide fuzzy matching and actionable failure context rather than returning a bare "not found".
- Memory and skill mutations should write tenant-scoped learning audit records under `~/.selfmind/<tenant>/learning/`. Use the shared audit helpers instead of creating one-off history files in individual tools.
- `skill_manage(action=history, name=...)` should read from the learning audit history and remain the first user-facing view for skill change history.
- `skill_manage(action=undo, change_id=...)` and `memory(action=undo, change_id=...)` are the supported rollback surfaces for durable learning changes. Do not add channel-specific undo logic that bypasses these tools.
- TUI commands such as `/skills history`, `/skills undo`, `/memory history`, `/memory remove`, and `/memory undo` should dispatch through the same tools so manual user actions still produce audit records.

Skill surfaces:

```text
skill_catalog   -> official/local/url install and audit
skills_list     -> compact metadata only
skill_view      -> SKILL.md or linked file content
skill_bundle    -> bundle CRUD
skill_manage    -> skill mutation and hot reload
/skill-name     -> user-facing direct invocation
/bundle-name    -> user-facing multi-skill invocation
```

## Add A Tool

1. Implement `tools.Tool`.
2. Register it in `internal/app/tools.go`.
3. Add schema validation and danger scanning if needed.
4. Ensure workspace scope applies when touching files or shell.
5. Add dispatcher/tool tests.

Tool schemas should be structured JSON-friendly objects. Avoid ad hoc string parsing when a structured API is available.

## Add A Model Provider

First decide whether code is actually needed:

- One-off OpenAI-compatible vendor: use `selfmind model` and custom endpoint.
- Reusable vendor with known base URL/protocol: add or document a `provider_profiles.<id>` YAML entry.
- Built-in vendor metadata, auth env vars, and live model-list behavior: update `internal/modelruntime/profile.go` and `catalog.go`.
- New protocol family: add a Go adapter.

For a new adapter:

1. Implement `llm.Provider`.
2. Support `Chat`, `StreamChat`, and `ChatCompletion`.
3. Preserve tool-call behavior if the provider supports tools.
4. Add runtime/profile metadata and credential resolution tests in `internal/modelruntime`.
5. Add only protocol-to-adapter construction in `internal/app/agent.go`.
6. Extend config only if `ProviderEndpoint` / `provider_profiles` is not enough.
7. Add tests for streaming, errors, native tools, auth resolution, model catalog, and role routing.

## Add An IM Platform

Keep platform adapters thin:

```text
platform adapter
  verify signature
  parse platform payload
  normalize inbound message
  optionally implement sender
        |
        v
gateway
  identity binding
  workspace/task/run state
  agent dispatch
  delivery enqueue/retry/split
```

The gateway owns person identity, task state, workspace binding, active run policy, memory, skills, and the outbound queue. Platform adapters should not own those concepts.

The generic webhook entrypoint is:

```text
POST /v1/im/{platform}
```

Production platform adapters should be added in this order:

1. Inbound signature/decryption/deduplication.
2. Payload normalization into `api.MessageRequest`.
3. A sender that implements `delivery.Sender` and is registered in `delivery.Router`.
4. Reuse delivery long-message splitting, retry, and pending queue behavior.
5. Add native approval buttons and rich media attachments when the platform supports them.

## Add A Gateway Command

1. Add model-free pre-agent command behavior in `internal/gateway/httpapi/server.go` when possible.
2. Add a CLI wrapper in `internal/cliapp` if users should invoke it directly.
3. Put request/response DTOs in `internal/gateway/api`.
4. Add HTTP and CLI tests.
5. Update README and this guide.

## Testing

Run all tests:

```sh
GOWORK=off go test ./...
```

Run the product gate in two local tiers:

```sh
selfmind selfcheck --fast   # edit loop: build/test + recorded non-slow cases
selfmind selfcheck          # before push: build/test + every recorded case
```

Provider responses replay offline, but tool calls use the host toolchain. A
missing Go binary, repository root, eval directory, or case-required command is
an unavailable environment (exit `2`), not a pass. CI deliberately runs only
cases marked with `ci.required/reason/platforms`; local full remains the
authoritative behavior gate.

For a regression introduced within a known range:

```sh
git bisect start HEAD <known-good-tag>
git bisect run bash scripts/bisect-selfcheck.sh
git bisect reset
```

The script maps build or environment failures to bisect exit `125` (skip), uses
a temporary HOME, and runs `local-fast` unless passed `full`.

Useful checks:

```sh
go test ./internal/platform/config ./internal/cliapp ./internal/runtime/gateway
go build ./cmd/selfmind
git diff --check
```

Manual smoke checks:

```sh
selfmind -f ./tmp/config.yaml model
selfmind -f ./tmp/config.yaml gateway run
selfmind -f ./tmp/config.yaml gateway status
selfmind -f ./tmp/config.yaml gateway stop
```

## Release

Release workflow:

```text
.github/workflows/release.yml
```

It builds only `selfmind` for Linux:

- `linux-amd64`
- `linux-arm64`

It supports both tag-triggered release and manual workflow dispatch.

Linux install scripts create `/etc/selfmind/config.yaml` and the systemd unit runs:

```sh
/usr/local/bin/selfmind -f /etc/selfmind/config.yaml gateway run
```

## Future SaaS Notes

Do not fork personal and SaaS architecture. Keep one binary with multiple modes:

```text
selfmind
selfmind gateway ...
selfmind server ...
selfmind worker ...
selfmind migrate ...
```

Recommended SaaS evolution:

1. Add DB-backed model policy and secret store behind interfaces.
2. Keep local YAML implementation as the personal-mode store.
3. Add durable worker queue for long-running runs.
4. Add outbound IM senders and approval callbacks.
5. Add tenant isolation for workspaces, data dirs, rate limits, and model budgets.
6. Add web admin and observability, but keep the gateway/task/run contracts stable.
