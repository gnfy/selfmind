# SelfMind

[中文说明](README.zh-CN.md) · [中文开发指南](docs/development-guide.zh-CN.md)

SelfMind is a Go-based personal AI agent runtime. The current product direction is not a one-off chatbot, but a long-running work agent that can be used from a local TUI, a local gateway, and IM/webhook channels while sharing identity, task state, workspace scope, memory, and skills.

The short version: run `selfmind` for local interactive work, run `selfmind gateway start` when you want a 24/7 local agent, and use `selfmind model` to configure built-in providers, reuse selected coding-CLI logins, or add a custom OpenAI-compatible endpoint without rebuilding the binary.

## Current Capabilities

Available for daily personal use:

- Local terminal UI with slash commands, tool-call status, memory, skills, checkpoints, and curator controls.
- Single user-facing binary: `selfmind`.
- Long-running gateway lifecycle: `selfmind gateway start/run/status/stop/restart`.
- Gateway client commands: `send`, `status`, `stop`, `tasks`, `workspaces`, `approvals`, `approve`, `reject`, `workspace add`, `workspace use`, `new`, `id`.
- Generic IM webhook entrypoint: `POST /v1/im/{platform}`.
- IM outbound foundation: generic webhook/Telegram delivery, long-message splitting, retry, and a durable outbound queue.
- Account binding API so CLI, WeChat, Feishu, QQ-like relays, and other channels can resolve to the same `person_id`.
- Durable task runtime state with run heartbeat, interrupted-run recovery, recent task events, and gateway `/events` control responses.
- Structured run outcomes and handoffs for compact `/status` task cards across CLI and IM channels.
- Workspace isolation for file, search, patch, and terminal tools.
- Long-term memory, session search, skill management, background review, and skill curator.
- Tenant learning audit records for memory and skill mutations, including skill/memory history and basic undo.
- Native tool calling for OpenAI-compatible providers, with legacy text-tool fallback, repeated-failure/no-progress guardrails, and secret redaction.
- Simple primary/auxiliary model routing, with optional `models.roles` overrides for advanced tuning.
- Dynamic model runtime with provider profiles, live model-list fetching, local model-list cache, and best-effort auth reuse for Codex CLI, Claude Code, Gemini CLI, and Qwen CLI. Codex CLI and the SelfMind-owned MiniMax OAuth profile additionally auto-refresh expired access tokens.
- Built-in IM adapters: Telegram, personal/enterprise WeChat (Weixin, iLink protocol with built-in QR login via `selfmind weixin login`), Feishu/Lark, and QQ official bot. WeChat does inbound polling + media; Feishu/QQ use the generic webhook for inbound and a delivery sender for outbound. If an iLink session expires, run `selfmind weixin login` again; the running gateway hot-reloads the refreshed credentials and resumes polling without a restart.
- MCP client over stdio/HTTP (JSON-RPC) with multi-server connections and on-demand tool registration.
- Extended built-in tools beyond file/terminal: `web_search`, `web_extract`, `execute_code`, and parallel multi-agent `delegate_task`.

Still first-version or planned:

- Official Feishu and QQ SDK adapters are not started yet. The WeChat Official Account adapter handles inbound passive replies and can push outbound customer-service messages, but does not yet implement message encryption/decryption.
- Native approval buttons, official enterprise IM SDKs, rich media attachments, and full platform signing/encryption modes still need production hardening.
- SaaS admin console, tenant model-secret custody, billing policy, and queue/worker scaling are planned but not complete.

## Requirements

- Go 1.26+ for local development.
- Node.js 18+ when installing the release through npm.
- At least one configured model provider for real AI responses.
- Official npm releases target Linux x64/arm64, macOS x64/arm64, and WSL.
- Native Windows is not supported; use WSL on Windows.
- Linux provides the strongest execution isolation through bubblewrap. macOS
  currently uses approval-controlled host execution and fails closed when
  strict isolated execution is requested.

## Install With npm

The npm release supports Linux x64/arm64, macOS x64/arm64, and WSL. It
installs the matching native SelfMind binary behind a small Node.js launcher.

```sh
npm install --global @selfmind/cli@latest
selfmind
```

On the first interactive launch, `selfmind` opens guided model setup before it
starts the daemon or TUI. You can reuse a supported coding-agent login or enter
an API key. Run `selfmind setup` later to reconfigure the installation, and
`selfmind doctor` for detailed diagnostics.

Check for updates and upgrade without interrupting an active turn:

```sh
selfmind update check
selfmind update
```

Prerelease builds use `selfmind@next`. See
[`docs/npm-distribution.md`](docs/npm-distribution.md) for package topology,
release operations, uninstall behavior, and feedback privacy.

On macOS, `selfmind setup` installs a per-user launchd service. Inspect or
remove it with:

```sh
selfmind gateway service status
selfmind gateway service uninstall
```

`selfmind gateway start` reuses an already running LaunchAgent. Use
`selfmind gateway restart --drain` for upgrades so the active turn reaches a
safe boundary before the daemon is replaced.

Report a problem locally, or explicitly create a GitHub Issue in the official
`gnfy/selfmind` repository:

```sh
selfmind feedback "describe what happened"
gh auth login --hostname github.com
selfmind feedback --send "describe what happened"
```

SelfMind saves a private, redacted local report before submission and never
stores your GitHub token. If `gh` is missing or its login has expired, the
report is preserved and SelfMind prints recovery instructions and a pre-filled
manual Issue URL.

## Connect WeChat

SelfMind's primary WeChat integration is `gateway.weixin`, which uses the iLink
protocol for personal or enterprise WeChat. It is different from the
`gateway.wechat` Official Account adapter and does not require manually copying
an App ID or token. Start with the built-in QR login:

```sh
selfmind weixin login --timeout 8m
# Scan the terminal QR code in WeChat and confirm the login.
selfmind gateway restart --drain
selfmind weixin status
```

The login command saves the account credentials in your local SelfMind config
and enables `gateway.weixin`. By default it also binds the scanned WeChat user
to the current CLI `person_id`, switches direct messages to `allowlist`, and
adds only that WeChat sender. CLI and WeChat therefore share tasks, memory,
workspaces, approvals, and continuation without manually copying user IDs.
`selfmind weixin status` reports `cross_endpoint_identity: ready` when this
binding is complete.

The first login needs a gateway restart so the new adapter is started. If an
existing iLink session later expires, run `selfmind weixin login` again; the
running gateway reloads the refreshed credential file without a restart.

Send `/id`, `/status`, or an ordinary request in the private WeChat chat to
verify inbound messages and replies. Group messages are disabled by default.
Use `--owner-person-id person_xxx` only to bind to a specific existing person.
Use `--no-bind` only when you deliberately want WeChat to remain a separate
identity; it leaves existing identity-policy settings unchanged. Do not use
`owner_person_id` with `dm_policy: open`: any admitted sender could otherwise
inherit your person-level tasks, memory, workspace access, and approvals. Do
not publish `config.yaml` or the Weixin credential files. See the [command
reference](docs/command-reference.md) for advanced login flags and the
[live-device checklist](docs/weixin-live-test.md) for delivery, approval,
media, and session-recovery tests.

## Build And Run

Build the user-facing binary:

```sh
go build -ldflags="-s -w" -o selfmind ./cmd/selfmind
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

## Configuration

SelfMind uses one YAML config file. No `.env` file is required.

> **Full field reference:** see [`docs/config-reference.md`](docs/config-reference.md)
> for every section, default value, and when to change it. The essentials are
> below.

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

The flow is:

1. Choose a provider: OpenAI, Anthropic, Google, `Custom endpoint (enter URL manually)`, coding-CLI login reuse entries, or other built-in provider profiles.
2. Enter or keep the API key. For Codex CLI, Claude Code, Gemini CLI, and Qwen CLI entries, SelfMind tries to reuse the existing CLI login instead.
3. SelfMind tries to load the provider model list live and caches the list locally.
4. Choose a model, or enter one manually if the list cannot be loaded.
5. The choice is saved to `config.yaml`.

Useful non-interactive commands:

```sh
selfmind model current
selfmind model list
selfmind model set openai gpt-4o
selfmind model set codex-cli gpt-5.6-sol --reasoning xhigh
selfmind model set custom:local-llm qwen2.5-coder
```

### Config Example

```yaml
# One primary foreground model. Omit reasoning or use "auto" to keep the
# provider/model default.
models:
  source: "local"
  primary:
    provider: "openai"
    model: "gpt-4o"
    reasoning: "auto"
  # One default for approval triage, memory, recall, summaries, and skill work.
  auxiliary:
    provider: "google"
    model: "gemini-1.5-flash"
  # Optional advanced exceptions. Missing fields inherit auxiliary.
  roles:
    fast_classifier: { model: "gemini-2.0-flash-lite" }

# Core provider config for first-class providers such as OpenAI, Anthropic,
# and Google.
providers:
  openai:
    # ${ENV_NAME} values are expanded from the process environment.
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
  # One-off or local custom endpoints, such as Ollama, a local model gateway,
  # or a temporary enterprise proxy. Select it as custom:ollama.
  custom:
    - name: "ollama"
      base_url: "http://localhost:11434/v1"
      api_key: ""
      protocol: "openai_compatible"
      model: "llama3"
      # Optional model metadata for local or custom endpoints.
      models:
        llama3:
          context_length: 8192

# Extensible provider registry for MiniMax, Kimi, DeepSeek, Z.AI, OpenRouter,
# internal model gateways, and future compatible vendors.
provider_profiles:
  minimax:
    api_key: "${MINIMAX_API_KEY}"
    base_url: "https://api.minimax.io/anthropic"
    protocol: "anthropic_messages"
  kimi-coding:
    api_key: "${KIMI_CODING_API_KEY}"
    base_url: "https://api.kimi.com/coding/v1"
    protocol: "openai_compatible"

# Optional SelfMind-owned JSON credential file that keeps secrets out of YAML.
auth:
  credentials_file: "~/.selfmind/auth.json"

# Local data directory. SQLite stores sessions, memory, tasks, and run state.
storage:
  type: "sqlite"
  data_dir: "~/.selfmind/data"

# Long-running gateway. CLI and IM/webhook channels share task state through it.
gateway:
  addr: "127.0.0.1:8765"
  # Empty is fine for local-only use. Set a token for servers or exposed ports.
  token: ""
  drain_timeout: "30s"
  # Presence = recent keyboard input, not an open window: after this long
  # without a keystroke the TUI stops counting as "attached", so approval and
  # result pushes route to your preferred IM again. "0" disables (an open TUI
  # always counts as attached).
  presence_idle_timeout: "5m"
  # An unanswered approval/question is re-pushed to your preferred IM this long
  # after it was raised, if you have since left the CLI and it was never sent.
  # The escrow sweep runs every 60s, so effective latency is this + up to 60s.
  # "0" disables escrow.
  pending_notify_after: "2m"
  # Terminal delivery history is retained for 14 days. Pending, retryable, and
  # IM-session recovery rows are never pruned. "0" disables pruning.
  outbound_retention: "336h"
  # Generic outbound webhook for custom IM relays.
  outbound_webhook_url: ""
  outbound_webhook_token: ""
  # Telegram reply token. Other platforms can integrate through gateway webhooks.
  telegram_token: "${TELEGRAM_BOT_TOKEN}"
  delivery_max_message_chars: 3500
  delivery_retry_attempts: 3

# Main agent loop settings.
agent:
  soul: "You are SelfMind, a helpful AI assistant."
  max_iterations: 90
  action_tool_budget: 12
  action_tool_budget_step: 6
  action_tool_budget_limit: 64
  max_budget_extensions: 9
  max_retries: 3
  log_level: "INFO"
  # LLM transport resilience (Package Zero). Absent/0 = sensible defaults.
  llm_max_retries: 5            # streaming/non-streaming call attempts
  llm_retry_base: "300ms"       # exponential backoff base (base*2^(n-1))
  llm_retry_cap: "30s"          # backoff ceiling; jitter [0.9,1.1) applied
  llm_stream_idle_timeout: "180s" # abort a stalled SSE stream (retryable) so the loop reconnects

# Long-term memory and semantic recall.
memory:
  auto_extract_interval: 5
  auto_extract_min_chars: 80
  semantic_recall: true
  use_memory_fence: true

# Reversible work-label governance. "0" disables an archive class.
tasks:
  inbox_enabled: true
  default_list_limit: 10
  auto_archive_done_after: "720h"
  auto_archive_cancelled_after: "168h"
  maintenance_model_role: "memory_extract"
  maintenance_debounce: "5m"
  maintenance_max_wait: "15m"
  maintenance_batch_max_runs: 10
  maintenance_soft_probe_initial: "15m"
  maintenance_soft_probe_max: "1h"

# Background learning/review settings for memory and skill evolution.
evolution:
  enabled: true
  mode: "auto"
  min_complexity_threshold: 3
  auto_archive_confidence: 0.8
  nudge_interval: 10

# MCP servers. Empty by default.
mcp:
  servers: []

# Intent routing. Ordinary language always reaches the agent; these knobs only
# tune explicit-command matching and continuation detection.
intent:
  mode: "hybrid"

# Cron entrypoint. Personal mode can keep the default.
cron:
  enabled: true

# Multi-agent delegation settings. Empty values use the default model policy.
delegation:
  provider: ""
  model: ""
  api_key: ""
  max_retries: 3
  max_iterations: 50

# Large-paste detection thresholds for the TUI editor.
editor:
  large_paste_chars: 1000
  large_paste_lines: 10

# Web search backend. Local DuckDuckGo scraping is unreliable (anti-bot
# blocks, GFW); set one hosted key for reliable results. Leave both empty for
# best-effort DuckDuckGo. Pick ONE backend and give ITS key.
#   tavily (recommended, https://tavily.com) | brave | serper | firecrawl | searxng
web:
  search_backend: "tavily"
  api_key: "tvly-xxxxxxxx"   # searxng: put the instance URL here instead

# Flight recorder: save each real turn locally so `selfmind eval capture` can
# promote a bad turn into an offline regression test. See "Diagnostics And
# Regression Capture".
flight_recorder:
  enabled: true
  keep: 30
```

### Model Config Fields

`models.primary` is the single default selection:

| Field | Purpose |
|---|---|
| `models.primary.provider` | Provider ID: built-in, `custom:<name>`, or an ID from `provider_profiles`. |
| `models.primary.model` | Primary model name. |
| `models.primary.reasoning` | Optional model reasoning level. Omit/use `auto` for the provider default; supported values are discovered per model when available. |
| `models.primary.service_tier` | Optional provider service tier. Omit/use `auto` for the provider default. |
| `models.primary.context_length` | Advanced fallback only. Normally omit it and let SelfMind discover model metadata. |

`providers` is the core provider config area for first-class built-in providers:

| Field | Purpose |
|---|---|
| `providers.openai` | Official OpenAI API settings. The default protocol is usually `openai_chat`. |
| `providers.anthropic` | Anthropic Messages API settings. The default protocol is usually `anthropic_messages`. |
| `providers.google` | Google Gemini through its OpenAI-compatible endpoint. |
| `providers.custom` | One-off or local custom endpoints, such as Ollama, a local gateway, or a temporary enterprise proxy. Select them with `models.primary.provider: "custom:<name>"`. |

`provider_profiles` is the extensible provider registry for MiniMax, Kimi, DeepSeek, Z.AI, OpenRouter, internal model gateways, or future compatible vendors:

| Field | Purpose |
|---|---|
| `provider_profiles.<id>` | `<id>` becomes the provider ID, such as `kimi-coding` or `minimax`. |
| `api_key` | API key. `${ENV_NAME}` values are expanded from the environment. You can leave it empty and use env vars or `auth.credentials_file` instead. |
| `base_url` | Provider endpoint. OpenAI-compatible endpoints usually end in `/v1`; Anthropic-compatible endpoints usually use the vendor's Anthropic gateway root. |
| `protocol` | Protocol family: `openai_chat`, `openai_compatible`, `anthropic_messages`, or `codex_responses`. Compatible new vendors do not need code changes. |
| `model` | Legacy endpoint fallback. New configurations select the model in `models.primary` or a role override. |

`auth.credentials_file` is an optional SelfMind-owned JSON credential file that keeps secrets out of reusable YAML:

```yaml
auth:
  credentials_file: "~/.selfmind/auth.json"
```

Credential precedence is: explicit key from a command/model role, `api_key` in `config.yaml`, `auth.credentials_file`, environment variables, then external CLI login reuse. External CLI reuse is limited to Codex CLI, Claude Code, Gemini CLI, and Qwen CLI. Codex CLI tokens are auto-refreshed when expired; Claude Code, Gemini CLI, and Qwen CLI tokens are reused as-is and need a re-login in the source CLI when they expire.

Recommended usage:

- OpenAI, Anthropic, Google: use `providers`.
- Ollama, local models, temporary OpenAI-compatible endpoints: use `providers.custom`.
- MiniMax, Kimi, DeepSeek, Z.AI, OpenRouter, internal model gateways: use `provider_profiles`.
- Secrets that should not live in YAML: use `auth.credentials_file` or environment variables.

Old flat provider keys and legacy `agent.provider` / `agent.model` /
`model.provider` / `model.default` are still read for compatibility.
`selfmind config upgrade` backs up the file and converges model selection on
`models.primary` without replacing existing provider credentials or unknown
settings.

### Provider Profiles And Auth Reuse

SelfMind resolves models through `internal/modelruntime`:

- Built-ins: `openai`, `anthropic`, `google`, `codex-cli`, `claude-code`, `gemini-cli`, `qwen-cli`, `openrouter`, `minimax` (with `minimax-oauth` and regional variants), `kimi-coding`, `deepseek`, `zai`, and `alibaba-coding-plan`.
- Custom OpenAI-compatible endpoints: use `selfmind model`, then choose `Custom endpoint (enter URL manually)`.
- Configurable provider profiles: add entries under `provider_profiles` when a provider has a stable base URL/protocol but you do not want a code change.
- Auth reuse is intentionally limited to Codex CLI, Claude Code, Gemini CLI, Qwen CLI, and SelfMind-owned OAuth providers such as the `minimax-oauth` profile. Other providers should use API keys in YAML, environment-expanded YAML values, or `auth.credentials_file`.

`auth.credentials_file` is a SelfMind-owned JSON credential store. The shape is:

```json
{
  "providers": {
    "kimi-coding": { "api_key": "..." },
    "codex-cli": { "access_token": "..." }
  }
}
```

For external CLI reuse, SelfMind also scans common local auth files and env vars, such as `~/.codex/auth.json`, Claude Code credentials, Gemini CLI OAuth files, and Qwen CLI OAuth files. SelfMind auto-refreshes expired Codex CLI OAuth tokens (and SelfMind-owned MiniMax OAuth tokens) before a request. For Claude Code, Gemini CLI, and Qwen CLI, tokens are reused as-is; if one expires, re-login with the source CLI.

MiniMax and Kimi Coding Plan:

- MiniMax Coding Plan should use the Anthropic-compatible profile: `provider: "minimax"`, `base_url: "https://api.minimax.io/anthropic"`, `protocol: "anthropic_messages"`, and model `MiniMax-M2.7` or `MiniMax-M2.7-highspeed`.
- Kimi Coding Plan should use `provider: "kimi-coding"`, `base_url: "https://api.kimi.com/coding/v1"`, `protocol: "openai_compatible"`, and model `kimi-for-coding`.
- If you want the Kimi Code Anthropic-compatible endpoint instead, set `provider_profiles.kimi-coding.base_url` to `https://api.kimi.com/coding`, `protocol` to `anthropic_messages`, and keep the model as `kimi-for-coding`.
- If you want the normal Kimi Open Platform API instead of the Coding Plan quota, add a custom OpenAI-compatible endpoint with `base_url: "https://api.moonshot.ai/v1"`.

### Model Routing

The foreground model is configured under `models.primary`. One optional
`models.auxiliary` selection covers bounded background work. Per-role entries
under `models.roles` are advanced exceptions and override auxiliary. There is
no `default` role.

Current role names:

- `coding_agent`: main agent loop.
- `memory_extract`: fact and turn extraction.
- `background_review`: after-turn learning review.
- `fast_classifier`: direct-answer routing, cheap classification, and low-latency smart approval triage.
- `skill_curator`: skill review and curation.
- `semantic_recall`: semantic session recall.
- `summarizer`: bounded context compaction summaries.

In personal mode these are read from local YAML. In the future SaaS mode, the same role names can be resolved from a database-backed tenant/person/workspace model policy.

Smart approval uses `fast_classifier` from an explicit role or the auxiliary
selection. For legacy configs it may fall back to an explicitly configured `background_review`, but
it never silently borrows `models.primary`; without either route it safely asks
the person.

## Local TUI

Start the TUI:

```sh
selfmind
```

With a custom config:

```sh
selfmind -f ./config/config.yaml
```

Common slash commands are listed below. See the
[complete command reference](docs/command-reference.md) for every CLI,
gateway, IM, and TUI command.

| Command | Purpose |
|---|---|
| `/help` | Show available commands. |
| `/status` | Show provider, model, runtime, token usage, current task, and any pending approval/question. |
| `/tasks` / `/tasks done\|archived\|all` | List open work as compact cards (status, last input, primary file, pending approvals/questions, run count, short id); finished work collapses to a count. |
| `/task <n\|id>` / `/task <n\|id> runs\|rename <name>\|pin\|unpin\|archive\|merge <dst>` | Inspect one task (detail, recent runs), rename, pin, archive, or merge it — `<n>` is the card number from `/tasks`, `<id>` a full or short id. |
| `/queue` / `/queue drop <n>` / `/queue clear` | List queued tasks / drop one by position / drop all. |
| `/stop` | Cancel the active run — or, if nothing is running, cancel the current (stuck) task. |
| `/cancel` | Cancel the current task even when no run is active. |
| `/new [title]` | Start a fresh task instead of continuing the current one. |
| `/resume <n\|task_id>` | Switch back to an earlier task by its `/tasks` card number, short id, or full id. |
| `/workspace [n\|id]` (alias `/ws`, also `/workspaces`) | Bare lists workspaces; with a number or id, switches to it. |
| `/approvals` / `/approve <n\|id\|all> [run]` / `/reject <n\|id\|all>` | List and answer pending tool approvals. `run` is accepted only when that request offers run-local reuse. |
| `/mode [mode]` | Show or set approval mode: `on-request`, `read-only`, `auto-edit`, `full-auto`, `smart` (default; safely asks when triage is unavailable). |
| `/diag [memory\|context\|tasks\|models\|delivery]` | Compact runtime diagnostics, optionally focused on one subsystem. |
| `/skills` | Skill list/view/search/catalog/install/audit/archive/pin/unpin/delete/stats/reload. |
| `/skills history <name>` | View learning audit history for a skill. |
| `/skills undo <change_id>` | Undo a supported skill learning change. |
| `/bundles` | List, view, create, or delete skill bundles. |
| `/reload-skills` | Reload skill tools from disk without restarting. |
| `/memory` | List, inspect history, remove, or undo long-term memory. |
| `/curator` | Check, dry-run, report, run, or restore skill curator actions. |
| `/checkpoint` | Save, load, list, or delete conversation checkpoints. |
| `/migrate` | Migrate Hermes Agent skills. |
| `/model` | Show the daemon's configured model and the CLI command used to change it. |
| `/clear` | Clear the screen. |
| `/exit` | Exit. |

Useful keys:

| Key | Behavior |
|---|---|
| `Enter` | Submit input. |
| `Shift+Enter` | Insert newline. |
| `Ctrl+C` | Cancel the current run or exit. |
| `Ctrl+V` | Paste. Large paste is converted into a readable attachment-style block. |

## Managing tasks

SelfMind is spine-first: your person-level work history provides continuity,
while **tasks are work labels** that group runs for status, resume, and
handoff across every endpoint (CLI, WeChat, etc.). Casual and diagnostic runs
may be filed into a hidden Inbox instead of creating visible task clutter. A
visible task moves through these states:

| Status | Meaning |
|---|---|
| `running` | A run is executing right now. |
| `in_progress` | The turn finished but more work is planned — resumable, nothing executing. |
| `blocked` | Waiting on you (a pending approval or question). |
| `queued` | Accepted while another task was running; starts automatically when the runner frees up. |
| `interrupted` | The run was lost (daemon restart / crash); resumable. |
| `done` / `completed` / `cancelled` / `failed` | Terminal. |

Everyday management:

- **See everything:** `/tasks` (open work; `/tasks done` for finished),
  `/task <id>` (one task's detail and runs), `/status` (the current one + any
  pending approval/question), `/queue` (what's waiting). `/tasks` renders one
  card per open task:

  ```text
  Open tasks:

  1. [running] KOF '97 style fighting game
     last: add a few more characters · 3m ago
     file: arcade-fury-97.html
     runs: 6
     id: task_65de41f2a...

  2. [waiting] WeChat channel setup
     last: waiting for QR-code login confirmation · 1h ago
     approvals: 1
     runs: 2
     id: task_9f2a77b01...

  … and 43 done — /tasks done

  Reply to continue the current task, /resume <id> to switch, /task <id> for detail.
  ```

  The bracket is the simplified state: `running` (a run is executing),
  `waiting` (a pending approval/question needs you), `paused` (open, nothing
  executing — reply or `/resume` to continue), or a terminal status verbatim.
- **Continue a task:** just reply (`继续` / `ok` / a follow-up) — an ordinary
  follow-up runs under your current open task by default (your working context
  follows you regardless, and the bounded post-run analyzer re-files a run that
  actually belonged to another task); `/resume <task_id>` switches to an older
  one, including an archived one.
- **Keep important work visible:** `/task <id> pin` protects a task from
  retention; `/task <id> unpin` removes that protection. Old terminal tasks
  are archived, never deleted, according to the `tasks.auto_archive_*`
  settings; `/resume <id>` reopens an archive.
- **You'll be nudged:** an unanswered approval or question is re-pushed to your
  preferred IM if you leave the CLI, so it never sits pending invisibly.
- **Start something separate:** `/new` — otherwise a new request while a task
  runs is **queued** behind it (not rejected), and a follow-up continues the
  current task.
- **Stop a task:** `/stop` cancels the active run. If nothing is running but a
  task is stuck non-terminal (e.g. it was created but never executed), `/stop`
  (or `/cancel`) cancels that current task so it leaves `in_progress`.
- **Runs are daemon-owned:** closing the CLI does **not** kill a running task —
  it keeps running on the gateway and the result is pushed to your bound IM.
  Reopen the CLI to see a "while you were away" digest and re-attach to a
  still-running task. Press `Ctrl+C` while watching to detach (the task keeps
  running); press it during your own run to choose *background / cancel / keep
  watching*.
- **Diagnose a stuck task:** `selfmind doctor` (or `--out file.txt`) exports a
  redacted snapshot — recent runs, pending approvals, the queue, and the event
  timeline — so you can see exactly what a task is waiting on.

## Skills

Skills are procedural memory: reusable workflows, checklists, and project-specific ways of working. They live under:

```text
~/.selfmind/<tenant>/skills/
```

New skills use a directory layout:

```text
<skill-name>/
  SKILL.md
  references/
  templates/
  scripts/
  assets/
```

Legacy flat `.md` skills are still loaded. Installed or learned skills are hot-reloadable; newly created skills become callable in the current session.

Catalog installs follow durable provenance rules. `skill_catalog` writes install metadata to `~/.selfmind/<tenant>/skills/.catalog/lock.json`, marks installed skills as `catalog-installed`, and refuses to overwrite an existing directory or legacy `.md` skill unless you pass `--force`. Forced reinstalls first move the previous copy into `~/.selfmind/<tenant>/skills/.catalog/backups/`. Curator never auto-archives catalog-installed, manual, bundled, or pinned skills.

Common commands:

```sh
/skills list
/skills view codebase-inspection
/skills search docker
/skills catalog
/skills install official/codebase-inspection
/skills install ./my-skill --name my-skill
/skills install https://raw.githubusercontent.com/org/repo/main/path/SKILL.md
/skills install ./my-skill --name my-skill --force
/skills audit
/skills history codebase-inspection
/skills undo <change_id>
/skills reload
/reload-skills
```

Invoke a skill directly with a slash command:

```text
/codebase-inspection inspect this repository
```

Skill bundles load multiple skills together. Bundle files are YAML under `~/.selfmind/<tenant>/skill-bundles/`.

```sh
/bundles create backend-dev codebase-inspection,test-driven-change
/bundles list
/backend-dev implement this change
```

Curator commands:

```sh
/curator status
/curator run --dry-run --report
/curator run --report
/curator restore old-skill
```

Curator only manages `agent-created` skills by default. Pinned skills are protected from archive/delete. Manual and catalog-installed skills are left alone unless you explicitly manage them.

## Memory

Memory stores durable user preferences and project/environment facts. It should not be used for temporary task status.

The default view is a human-readable profile: related evidence is grouped, storage IDs are hidden, and no raw fact is deleted. Search exposes short references for inspection and correction; use `raw` only when auditing the underlying evidence.

```sh
/memory
/memory search selfmind
/memory show 5f33aefd
/memory correct 5f33aefd "Prefers concise, structured answers."
/memory forget 5f33aefd
/memory pin "Always discuss technical work in Chinese."
/memory raw
/memory history
/memory history user
/memory remove user "prefers concise answers"
/memory undo <change_id>
```

Memory and skill changes are written to `~/.selfmind/<tenant>/learning/`. Use history first, then undo the specific `change_id` when a learned item is wrong or stale.

## Diagnostics And Regression Capture

SelfMind records what happens as you use it, so you can find problems and turn real failures into permanent regression tests. Three things, in order of how often you touch them.

### 1. See what recently went wrong

Errors (failed tools, model/API failures like 429 or connection drops) are recorded automatically. To review them:

```sh
selfmind doctor
```

The `== Recent errors ==` section aggregates both kinds, newest first:

```
== Recent errors ==
- 07-13 10:33 [tool:web_search] backend unavailable (HTTP 202 anti-bot challenge)
- 07-13 09:15 [run:failed] llm chat: responses API error 429: usage limit reached
```

`[tool:<name>]` is a single tool call failing; `[run:failed]` is a whole turn / model interface failing. Secrets are redacted. For subsystem detail use the model-free control commands in a chat: `/diag`, `/diag context`, `/diag tasks`, `/diag memory`, `/diag models`, `/diag delivery`, and `/diag execution` (sandbox, network posture, process environment policy, and workspace roots; never credential values).

### 2. Record real turns (flight recorder)

Enable it once in config so every real turn's model interaction is saved locally:

```yaml
flight_recorder:
  enabled: true
  keep: 30        # keep the newest 30 turns, auto-prune older
```

Restart the gateway (`selfmind gateway restart`) after changing config. Recordings live in `~/.selfmind/flight/` (local only, never uploaded); `keep` bounds disk use. Internal/background turns (delegation, reviews) are excluded — only real user-facing turns are kept.

### 3. Turn a bad turn into a regression test

When a turn shows a bug worth preventing forever, promote it into an offline eval case:

```sh
# Capture the most recent recorded turn as a private draft
selfmind eval capture latest --title "search backend outage must report failure, not a false negative" --suite quality

# Add deterministic assertions, replay it without provider quota, then move the
# reviewed YAML from evaldrafts/ to evalcases/ and its cassette from
# .vcr-drafts/ to .vcr/. The full release gate then includes it:
selfmind selfcheck
```

**Before committing a captured case, open its YAML and scrub it** — flight recordings are plaintext conversations, so remove any real names, private queries, or anything that should not enter the repo.

The loop: use SelfMind normally → `selfmind doctor` to spot problems → `eval capture` to freeze a private draft → add assertions and replay → explicitly promote it into the release corpus.

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
selfmind ws                       # list workspaces (alias of `workspace`; `workspaces` also works)
selfmind approvals
selfmind approve <approval_id>
selfmind reject <approval_id>
selfmind ws add .                 # register the current dir as a workspace
selfmind ws use <workspace_id>    # or: selfmind ws <n>  to switch by list number
selfmind ws trust [workspace_id]  # local CLI only; omit id for the current workspace
selfmind ws untrust [workspace_id]# revoke trust and active execution capabilities
selfmind ws grants [workspace_id] # list active temporary execution capabilities
selfmind ws revoke <capability> [workspace_id]
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
| `GET` | `/v1/approvals` | List pending or filtered approval requests. |
| `POST` | `/v1/approvals/respond` | Approve or reject an approval request. |
| `GET` | `/v1/tasks` | List tasks for an account. |
| `GET` | `/v1/tasks/current` | Get current task and active run. |
| `GET` | `/v1/tasks/events` | List recent run/tool/learning events for the current or specified task. |
| `POST` | `/v1/workspaces/register` | Register a local workspace. |
| `POST` | `/v1/workspaces/trust` | Change workspace trust. Loopback CLI requests only. |
| `GET` | `/v1/workspaces` | List workspaces. |
| `GET` | `/v1/gateway/status` | Inspect process state and active runs. |
| `POST` | `/v1/gateway/shutdown` | Request graceful shutdown. |

Channel chats are not mirrored automatically. CLI messages are not pushed to IM, and IM messages are not pushed to CLI. Shared state is task/run/workspace/memory/skill state.

When an async IM task finishes, the gateway sends the result back to the source channel if an outbound sender is configured. Otherwise the message remains pending in `control.db` until a sender is configured. Built-in senders:

- `gateway.telegram_token`: send Telegram `sendMessage` replies to the inbound `channel/chat_id`.
- `gateway.outbound_webhook_url`: POST the normalized JSON payload to a custom relay.

Long messages are split by `gateway.delivery_max_message_chars`; failed deliveries retry up to `gateway.delivery_retry_attempts`. Use gateway `/status` for a task summary and `/events` for recent runtime events.

## Linux Release

The GitHub Actions release workflow lives at:

```text
.github/workflows/release.yml
```

Trigger modes:

- **Push a `v*` tag** — builds and publishes a GitHub Release automatically.
- **Manual run (Actions → Release → Run workflow)** — builds and packages on
  demand. Leave `tag` empty for a `dev-<date>-<sha>` build; keep `publish` off to
  just produce downloadable `tar.gz` artifacts on the run page (no Release is
  created); turn `publish` on to also cut a GitHub Release.

Current release artifacts:

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

Related documentation:

- [Complete command reference](docs/command-reference.md)
- [Configuration reference](docs/config-reference.md)
- [Coding agent foundations](docs/coding-agent-foundations.md)
- [Current development status](docs/STATUS.md)

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

Architecture notes:

- [Identity & Cross-Endpoint Continuity (north star)](docs/identity-continuity.md)
- [Development Guide](docs/development-guide.md)
- [Architecture Constraints](docs/architecture-constraints.md)
- [Coding Agent Foundations](docs/coding-agent-foundations.md)
- [Implementation Status & Priorities](docs/STATUS.md)

Important directories:

```text
cmd/selfmind/              user-facing binary entrypoint
internal/cliapp/           selfmind command router, model command, gateway/client commands
internal/app/              application wiring for storage, agent, tools, gateway
internal/platform/config/  YAML config loading, defaults, save, compatibility
internal/kernel/           agent loop, memory/review, native tool calls
internal/kernel/llm/       provider adapters and model gateway
internal/modelruntime/     provider profiles, credentials, model catalog/cache
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

Most new providers do not need code changes.

For one-off OpenAI-compatible endpoints, run:

```sh
selfmind model
```

Then choose `Custom endpoint (enter URL manually)`.

For reusable local config, add `provider_profiles.<id>` with `base_url`, `protocol`, `api_key`, and optional `model`.

Only add a Go provider adapter when the provider has a different protocol family. The current core families are:

- OpenAI Chat Completions and OpenAI-compatible endpoints.
- Anthropic Messages API.
- Google Gemini via OpenAI-compatible endpoint.
- OpenAI/Codex Responses-compatible endpoints.
- Generic provider profiles and custom endpoints.

For a new protocol family:

1. Implement `llm.Provider` in `internal/kernel/llm`.
2. Add protocol/runtime metadata in `internal/modelruntime`.
3. Keep credential discovery and model-list fetching in `internal/modelruntime`, not in `internal/app`.
4. Wire only the protocol-to-adapter conversion in `internal/app/agent.go`.
5. Add tests for streaming, native tool calls if supported, auth resolution, model catalog, and `models.roles` routing.

### Add An IM Platform

Keep the adapter boundary thin:

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
