# SelfMind

[中文说明](README.zh-CN.md) · [中文开发指南](docs/development-guide.zh-CN.md)

SelfMind is a Go-based personal AI agent runtime. The current product direction is not a one-off chatbot, but a long-running work agent that can be used from a local TUI, a local gateway, and IM/webhook channels while sharing identity, task state, workspace scope, memory, and skills.

The short version: run `selfmind` for local interactive work, run `selfmind gateway start` when you want a 24/7 local agent, and use `selfmind model` to configure OpenAI, Anthropic, Google, or a custom OpenAI-compatible endpoint without rebuilding the binary.

## Current Capabilities

Available for daily personal use:

- Local terminal UI with slash commands, tool-call status, memory, skills, checkpoints, and curator controls.
- Single user-facing binary: `selfmind`.
- Long-running gateway lifecycle: `selfmind gateway start/run/status/stop/restart`.
- Gateway client commands: `send`, `status`, `stop`, `tasks`, `workspaces`, `workspace add`, `workspace use`, `new`, `id`.
- Generic IM webhook entrypoint: `POST /v1/im/{platform}`.
- IM outbound foundation: generic webhook/Telegram delivery, long-message splitting, retry, and a durable outbound queue.
- Account binding API so CLI, WeChat, Feishu, QQ-like relays, and other channels can resolve to the same `person_id`.
- Durable task runtime state with run heartbeat, interrupted-run recovery, recent task events, and the `/events` control command.
- Workspace isolation for file, search, patch, and terminal tools.
- Long-term memory, session search, skill management, background review, and skill curator.
- Hermes-style native tool calling for OpenAI-compatible providers, with legacy text-tool fallback, repeated-failure/no-progress guardrails, and secret redaction.
- Role-based model routing through `models.roles`, so coding, memory extraction, background review, skill curation, and semantic recall can use different models.

Still first-version or planned:

- Official Feishu, WeChat, and QQ SDK adapters are not complete yet.
- Native approval buttons, official enterprise IM SDKs, rich media attachments, and full platform signing/encryption modes still need production hardening.
- SaaS admin console, tenant model-secret custody, billing policy, and queue/worker scaling are planned but not complete.

## Requirements

- Go 1.26+ for local development.
- At least one configured model provider for real AI responses.
- Official release packaging currently targets Linux server deployments: `linux-amd64` and `linux-arm64`.
- Windows and macOS can be used for local development and debugging, but are not the current release targets.

## Build And Run

Build the user-facing binary:

```sh
go build -ldflags="-s -w" -o selfmind ./cmd/selfmind
```

On Windows:

```powershell
go build -ldflags="-s -w" -o selfmind.exe ./cmd/selfmind
```

Development run:

```sh
GOWORK=off go run ./cmd/selfmind
```

Do not use:

```sh
go run cmd/selfmind/main.go
```

That command compiles only one file and can make Go treat `selfmind/internal/...` as a standard-library path. Use `go run ./cmd/selfmind` or `go build ./cmd/selfmind`.

`cmd/selfmindd` is kept only as a hidden compatibility wrapper. Users should build and run `selfmind`.

## Configuration

SelfMind uses one YAML config file. No `.env` file is required.

Default path:

```text
~/.selfmind/config.yaml
```

Use a custom path with `-f` / `--config`:

```sh
./selfmind -f ./config/config.yaml
./selfmind --config ./config/config.yaml gateway start
./selfmind -f ./config/config.yaml model
```

`SELF_CONFIG` is also supported for process managers and containers:

```sh
SELF_CONFIG=/etc/selfmind/config.yaml ./selfmind gateway run
```

When the config file is missing, SelfMind creates the default config automatically. For an explicit `-f` path, commands that need to create config, such as `selfmind model`, can initialize it.

### Configure A Model

Interactive provider and model picker:

```sh
selfmind model
```

The flow is Hermes-like:

1. Choose a provider: OpenAI, Anthropic, Google, saved custom endpoints, or `Custom endpoint (enter URL manually)`.
2. Enter or keep the API key.
3. SelfMind tries to load the provider model list live.
4. Choose a model, or enter one manually if the list cannot be loaded.
5. The choice is saved to `config.yaml`.

Useful non-interactive commands:

```sh
selfmind model current
selfmind model list
selfmind model set openai gpt-4o
selfmind model set custom:local-llm qwen2.5-coder
```

### Config Example

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

agent:
  soul: "You are SelfMind, a helpful AI assistant."
  max_iterations: 90
  max_retries: 3
  log_level: "INFO"

memory:
  auto_extract_interval: 5
  auto_extract_min_chars: 80
  semantic_recall: true
  use_memory_fence: true

evolution:
  enabled: true
  mode: "auto"
  min_complexity_threshold: 3
  auto_archive_confidence: 0.8
  nudge_interval: 10

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

mcp:
  servers: []

cron:
  enabled: true

delegation:
  provider: ""
  model: ""
  api_key: ""
  max_retries: 3
  max_iterations: 50

editor:
  large_paste_chars: 1000
  large_paste_lines: 10
```

Old flat provider keys such as `providers.openai_api_key` and legacy `agent.provider` / `agent.model` are still read for compatibility, but new saves use the `model` and nested `providers` schema.

### Model Routing

The default model is configured under `model`. Role routing is configured under `models.roles`.

Current role names:

- `coding_agent`: main agent loop.
- `memory_extract`: fact and turn extraction.
- `background_review`: after-turn learning review.
- `skill_curator`: skill review and curation.
- `semantic_recall`: semantic session recall.

In personal mode these are read from local YAML. In the future SaaS mode, the same role names can be resolved from a database-backed tenant/person/workspace model policy.

## Local TUI

Start the TUI:

```sh
selfmind
```

With a custom config:

```sh
selfmind -f ./config/config.yaml
```

Common slash commands:

| Command | Purpose |
|---|---|
| `/help` | Show available TUI commands. |
| `/status` | Show provider, model, runtime, token usage, task, and gateway status. |
| `/tasks` | Show local gateway tasks. |
| `/skills` | Skill list/view/search/archive/pin/unpin/delete/stats. |
| `/memory` | List or remove long-term memory. |
| `/curator` | Check or run skill curator. |
| `/checkpoint` | Save, load, list, or delete conversation checkpoints. |
| `/migrate` | Migrate Hermes Agent skills. |
| `/model` | Show or switch the current model inside the TUI. |
| `/clear` | Clear the screen. |
| `/exit` | Exit. |

Useful keys:

| Key | Behavior |
|---|---|
| `Enter` | Submit input. |
| `Shift+Enter` | Insert newline. |
| `Ctrl+C` | Cancel the current run or exit. |
| `Ctrl+V` | Paste. Large paste is converted into a readable attachment-style block. |

## Gateway Mode

Start a long-running local gateway:

```sh
selfmind gateway start
```

Run in the foreground:

```sh
selfmind gateway run
```

Manage the gateway:

```sh
selfmind gateway status
selfmind gateway stop
selfmind gateway restart
```

Use the gateway from CLI:

```sh
selfmind send "what is the current task status?"
selfmind send --async "run tests and fix failures"
selfmind status
selfmind tasks
selfmind stop
selfmind workspaces
selfmind workspace add .
selfmind workspace use <workspace_id>
selfmind new "implement the checkout page"
```

Run as a simple gateway-backed REPL:

```sh
SELF_USE_GATEWAY=1 selfmind
```

Gateway runtime files are stored under:

```text
<storage.data_dir>/gateway/
```

Files include `gateway.pid`, `gateway_state.json`, `gateway.lock`, and `gateway.log`.

### Gateway HTTP API

If the gateway is exposed beyond localhost, set `gateway.token` in `config.yaml`. Clients can send it as `Authorization: Bearer <token>` or `X-SelfMind-Token`.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/health` | Liveness check. |
| `POST` | `/v1/message` | Send a CLI/IM/Web message. |
| `POST` | `/v1/im/{platform}` | Generic IM webhook entrypoint. |
| `POST` | `/v1/accounts/bind` | Bind another platform account to an existing person. |
| `GET` | `/v1/tasks` | List tasks for an account. |
| `GET` | `/v1/tasks/current` | Get current task and active run. |
| `GET` | `/v1/tasks/events` | List recent run/tool/learning events for the current or specified task. |
| `POST` | `/v1/workspaces/register` | Register a local workspace. |
| `GET` | `/v1/workspaces` | List workspaces. |
| `GET` | `/v1/gateway/status` | Inspect process state and active runs. |
| `POST` | `/v1/gateway/shutdown` | Request graceful shutdown. |

Channel chats are not mirrored automatically. CLI messages are not pushed to IM, and IM messages are not pushed to CLI. Shared state is task/run/workspace/memory/skill state.

When an async IM task finishes, the gateway sends the result back to the source channel if an outbound sender is configured. Otherwise the message remains pending in `control.db` until a sender is configured. Built-in senders:

- `gateway.telegram_token`: send Telegram `sendMessage` replies to the inbound `channel/chat_id`.
- `gateway.outbound_webhook_url`: POST the normalized JSON payload to a custom relay.

Long messages are split by `gateway.delivery_max_message_chars`; failed deliveries retry up to `gateway.delivery_retry_attempts`. Use `/status` for a task summary and `/events` for recent runtime events.

## Linux Release

The GitHub Actions release workflow lives at:

```text
.github/workflows/release.yml
```

It supports automatic tag releases and manual `workflow_dispatch` runs. Current release artifacts:

- `selfmind-<tag>-linux-amd64.tar.gz`
- `selfmind-<tag>-linux-arm64.tar.gz`
- `SHA256SUMS.txt`

The package includes `selfmind`, install/uninstall scripts, a systemd service template, README, and docs.

The Linux installer creates `/etc/selfmind/config.yaml`, `/var/lib/selfmind/data`, and a `selfmind` system user. The systemd unit starts the gateway with:

```sh
/usr/local/bin/selfmind -f /etc/selfmind/config.yaml gateway run
```

Edit `/etc/selfmind/config.yaml` to configure the model provider before starting the service.

## Development

Run tests:

```sh
GOWORK=off go test ./...
```

If you use the local bundled Go toolchain from this workspace on Windows:

```powershell
$env:PATH="$env:USERPROFILE\.cache\selfmind-tools\go1.26.3\go\bin;" + $env:PATH
$env:GOWORK='off'
go test ./...
```

Important directories:

```text
cmd/selfmind/              user-facing binary entrypoint
cmd/selfmindd/             hidden compatibility wrapper
internal/cliapp/           selfmind command router, model command, gateway/client commands
internal/app/              application wiring for storage, agent, tools, gateway
internal/platform/config/  YAML config loading, defaults, save, compatibility
internal/kernel/           agent loop, memory/review, native tool calls
internal/kernel/llm/       provider adapters and model gateway
internal/tools/            built-in tools and tool middleware
internal/gateway/          TUI, HTTP API, router, channel adapters
internal/control/          durable identity/workspace/task/run state
internal/runtime/gateway/  gateway pid/lock/state/start/stop runner
docs/                      architecture and development docs
packaging/                 Linux install scripts and service templates
```

### Add A Tool

1. Implement `tools.Tool` in `internal/tools`.
2. Register it in `internal/app/tools.go` or the relevant tool bundle.
3. Add workspace/approval/process middleware coverage if the tool touches files, shell, network, memory, or skills.
4. Add tests for the tool and any dispatcher behavior.

Tool runtime rules:

- Prefer `tools.NewRegistry()` plus `tools.NewDispatcherWithRegistry(...)` for application paths. `tools.NewDispatcher()` and `tools.GlobalRegistry()` are legacy compatibility paths.
- Do not register tenant, workspace, MCP, or skill tools into the global registry from new code.
- Dispatcher argument coercion is strict. Invalid integers, numbers, or booleans should return an error instead of silently becoming `0` or `false`.
- Clarify and approval callbacks should be injected through the dispatcher/registry context when possible.

### Add A Model Provider

Most new OpenAI-compatible providers do not need code changes. Run:

```sh
selfmind model
```

Then choose `Custom endpoint (enter URL manually)`.

Only add a Go provider adapter when the provider has a different protocol family. The current core families are:

- OpenAI Chat Completions and OpenAI-compatible endpoints.
- Anthropic Messages API.
- Google Gemini via OpenAI-compatible endpoint.
- Generic OpenAI-compatible custom endpoints.

For a new protocol family:

1. Implement `llm.Provider` in `internal/kernel/llm`.
2. Extend `internal/platform/config/loader.go` only if the generic endpoint schema is not enough.
3. Wire construction in `internal/app/agent.go`.
4. Add tests for streaming, native tool calls if supported, and `models.roles` routing.

### Add An IM Platform

Keep the boundary Hermes-like:

```text
platform adapter
  verify signature
  parse platform payload
  normalize inbound message
  optionally send outbound message
        |
        v
gateway
  identity binding
  workspace/task/run state
  agent dispatch
```

Do not store task state or memory ownership inside the platform adapter. The gateway owns identity, workspace, task, run, handoff, and approval state.

### Add A Gateway Command

1. Add model-free control behavior in `internal/gateway/httpapi/server.go` when possible.
2. Add a CLI wrapper in `internal/cliapp` for common user commands.
3. Keep DTOs in `internal/gateway/api`.
4. Update README and `docs/development-guide.md`.
5. Add HTTP and CLI tests.

## Design Constraints

- `kernel` should not depend on `tools`, `gateway`, or `server`.
- Agent accesses tools through `AgentBackend`.
- Workspace-scoped tools must honor `allowed_roots`.
- Gateway task and identity state is stored in `control.db`.
- Memory/session state is stored under `storage.data_dir`.
- Skills are tenant-scoped under `~/.selfmind/<tenant>/skills`.
- Personal mode uses local YAML. SaaS mode should keep the same model-role concepts but move tenant/person/workspace provider policy and secrets into a database-backed store.

## License

MIT
