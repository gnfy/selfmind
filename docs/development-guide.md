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

`cmd/selfmindd` is a hidden compatibility wrapper. Do not document it as a user entrypoint.

## Directory Map

```text
cmd/selfmind/              user-facing binary entrypoint
cmd/selfmindd/             hidden gateway compatibility wrapper
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
- Legacy flat provider keys such as `providers.openai_api_key` are read into nested providers.
- New saves write the nested provider schema and do not write the legacy flat keys.
- `LoadConfig(config.Options{Path: ...})` accepts explicit paths.
- `LoadConfig(config.Options{Path: ..., CreateIfMissing: true})` is used by commands that can initialize a new file, such as `selfmind model`.

## Model Provider System

User-facing command:

```sh
selfmind model
selfmind model current
selfmind model list
selfmind model set openai gpt-4o
```

Interactive flow:

1. Show provider list: OpenAI, Anthropic, Google, saved custom endpoints, and `Custom endpoint (enter URL manually)`.
2. Prompt for API key.
3. Fetch remote model list when the provider exposes one.
4. Let the user choose a model or enter one manually.
5. Save to `config.yaml`.

Implemented provider protocol families:

| Provider | Protocol | Adapter |
|---|---|---|
| OpenAI | Chat Completions / native tools | `llm.OpenAIAdapter` |
| Anthropic | Messages API | `llm.AnthropicAdapter` |
| Google | OpenAI-compatible Gemini endpoint | `llm.GeminiAdapter` |
| Custom | OpenAI-compatible endpoint | `llm.GenericOpenAIAdapter` |

Most new vendors should use the custom OpenAI-compatible entry and should not require a code change. Add a new Go adapter only for a genuinely different protocol family.

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
- `cmd/selfmindd/main.go` calls the same runner as a hidden compatibility wrapper.

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
- `task_events` stores events such as `run.started`, `tool.started`, `tool.completed`, `learning.review`, `run.finished`, and `run.cancelled`.
- `outbound_messages` is the IM delivery queue. Without a sender, messages remain `pending`; with a sender, the delivery worker retries them.
- `/status` is for a compact task summary; `/events` is for recent runtime events.

Workspace-scoped tools must honor `allowed_roots`. Do not bypass `WorkspaceScopeMiddleware` for file, search, patch, terminal, or process tools.

## Tool Calling

SelfMind now follows the Hermes-style native tool-call contract:

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

An `Agent` instance is serialized with `runMu` because it owns one `EventChannel`. Gateway paths that need true parallel active runs should construct separate agent instances per run or introduce a per-run event sink before removing that lock.

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
- `internal/tools/skill_manage.go`
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

## Add A Tool

1. Implement `tools.Tool`.
2. Register it in `internal/app/tools.go`.
3. Add schema validation and danger scanning if needed.
4. Ensure workspace scope applies when touching files or shell.
5. Add dispatcher/tool tests.

Tool schemas should be structured JSON-friendly objects. Avoid ad hoc string parsing when a structured API is available.

## Add A Model Provider

First decide whether code is actually needed:

- OpenAI-compatible vendor: use `selfmind model` and custom endpoint.
- New protocol family: add a Go adapter.

For a new adapter:

1. Implement `llm.Provider`.
2. Support `Chat`, `StreamChat`, and `ChatCompletion`.
3. Preserve tool-call behavior if the provider supports tools.
4. Add construction in `internal/app/agent.go`.
5. Extend config only if nested `ProviderEndpoint` is not enough.
6. Add tests for streaming, errors, native tools, and role routing.

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

Useful checks:

```sh
go test ./internal/platform/config ./internal/cliapp ./internal/runtime/gateway
go build ./cmd/selfmind
git diff --check
```

Manual smoke checks:

```sh
selfmind -f ./tmp/config.yaml model set openai gpt-test
selfmind -f ./tmp/config.yaml model current
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
