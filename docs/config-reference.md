# SelfMind Configuration Reference

Every setting lives in one YAML file: `~/.selfmind/config.yaml`. This page
documents each section, its defaults, and when you would change it. A Chinese
mirror is `docs/config-reference.zh-CN.md`.

## Quick facts

- **Location**: `~/.selfmind/config.yaml` (override with `--config <path>`).
- **First run** creates the file from a default template; missing sections fall
  back to built-in defaults, so you only write what you change.
- **Reload requires a restart.** The daemon reads config at startup only.
  After editing, run `selfmind gateway restart`. (A common trap: editing
  a key but forgetting to restart — the running daemon keeps the old value.)
- **Secrets**: API keys can go directly in the file, or be referenced from the
  environment as `${ENV_VAR}` (expanded at load). Keep the file `chmod 600` on
  shared hosts.
- **Durations** are Go duration strings: `"300ms"`, `"30s"`, `"5m"`, `"24h"`.

---

## 1. Model & providers (required)

The one thing you must configure: which model answers you.

```yaml
model:
  provider: "codex-cli"      # a provider id below, or custom:<name>, or a profile id
  default: "gpt-5.5"         # model name

providers:                   # first-class vendors
  openai:
    api_key: "${OPENAI_API_KEY}"
    base_url: "https://api.openai.com/v1"
    protocol: "openai_chat"
  anthropic:
    api_key: ""
    base_url: "https://api.anthropic.com"
    protocol: "anthropic_messages"
  google:
    api_key: ""
    base_url: "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"
    protocol: "openai_compatible"
  custom: []                 # one-off/local endpoints (Ollama, gateways)

provider_profiles:           # extensible registry (Kimi, MiniMax, DeepSeek, OpenRouter, …)
  kimi-coding:
    api_key: "${KIMI_API_KEY}"
    base_url: "https://api.kimi.com/coding"
    protocol: "anthropic_messages"
    model: "kimi-for-coding"
  codex-cli:
    base_url: "https://chatgpt.com/backend-api/codex"
    protocol: "codex_responses"
    model: "gpt-5.5"
```

- `protocol` is one of `openai_chat`, `openai_compatible`, `anthropic_messages`,
  `codex_responses`. Pick the one your endpoint speaks.
- **External CLI auth reuse**: Codex CLI, Claude Code, Gemini CLI, Qwen CLI
  credentials can be reused instead of an API key — no `api_key` needed for
  those profiles.
- Use `provider_profiles` for any vendor beyond the three first-class ones;
  reference it by setting `model.provider` to the profile id.

## 2. Model routing (roles)

Background jobs run on cheaper/faster models than your main chat, so heavy
memory/skill work never spends your primary quota.

```yaml
models:
  source: "local"
  roles:
    memory_extract:          # memory intake + consolidation + compaction summaries
      provider: "kimi-coding"
      model: "kimi-for-coding"
    background_review:       # skill/memory self-review, smart-mode approval triage
      provider: "kimi-coding"
      model: "kimi-for-coding"
    semantic_recall:         # optional query expansion for recall
      provider: "kimi-coding"
      model: "kimi-for-coding"
    skill_curator: { provider: "kimi-coding", model: "kimi-for-coding" }
    fast_classifier: { provider: "kimi-coding", model: "kimi-for-coding" }
```

Role names are stable; a role with no override falls back to the main model.
Point them at a cheap model to keep background work off your primary provider.

## 3. Storage & auth

```yaml
storage:
  type: "sqlite"
  data_dir: "~/.selfmind/data"   # sessions, memory, tasks, run state (SQLite)
auth:
  credentials_file: "~/.selfmind/auth.json"   # optional; keeps secrets out of YAML
```

## 4. Web search

Local DuckDuckGo scraping is unreliable (anti-bot 202 pages, GFW), so a hosted
search API is strongly recommended. **Pick one backend and give it its key.**

```yaml
web:
  search_backend: "tavily"   # tavily | brave | serper | firecrawl | searxng | duckduckgo
  api_key: "tvly-xxxx"       # the chosen backend's key; for searxng, the instance URL
```

- Recommended: **Tavily** (https://tavily.com, AI-native, free tier, good CN
  coverage). Alternatives: Brave (https://brave.com/search/api/), Serper
  (https://serper.dev, Google results).
- Empty backend + empty key = best-effort DuckDuckGo (often blocked — use only
  if you cannot get a key).
- One backend, no fallback chain: a configured backend that fails returns an
  error (so the model reports the outage) rather than silently switching.

## 5. Memory & governance

```yaml
memory:
  auto_extract_interval: 5       # extract durable facts at most every N turns
  auto_extract_min_chars: 80     # skip trivially short turns
  semantic_recall: true          # attach related prior-work slices per turn
  use_memory_fence: true         # wrap recalled memory as untrusted background data
  governance:
    enabled: true
    mode: "shadow"               # shadow (report only) → merge-only → full (adds caps)
    model_role: "memory_extract" # which role judges consolidation
    consolidation_interval: "24h"
    consolidation_batch_size: 8
    auto_merge_confidence: 0.95      # MERGE gate
    auto_reinforce_confidence: 0.90  # REINFORCE gate (verbatim member text)
    auto_archive_confidence: 0.90    # ARCHIVE gate (reversible)
    max_active_global: 120       # active-memory cap (FULL mode only)
    max_active_per_workspace: 200
    archive_after: "4320h"       # 180d; age-out (FULL mode only)
    pause_while_run_active: true # foreground runs always win
```

- `mode`: `shadow` writes nothing (only reports what it *would* do);
  `merge-only` applies gated MERGE/REINFORCE/ARCHIVE; `full` adds the
  active-memory caps + age-based archival. **Do not switch to `full` until
  `max_active_global` is recalibrated to your real steady state** — a low cap
  against a large store mass-archives in one pass.
- Pinned and user-confirmed memories are immune to all automatic changes.
- Keep `shadow` until the Markdown report in
  `~/.selfmind/reports/memory-consolidation/` shows acceptable decisions over
  real history. Promote to `merge-only` deliberately; `full` remains a later
  cap/aging step.
- See `docs/memory-governance.zh-CN.md` for the full logic.

## 6. Gateway & IM channels

The always-on daemon. CLI, IM, cron, and HTTP all converge on it.

```yaml
gateway:
  addr: "127.0.0.1:8765"       # bind address; a public bind REQUIRES a token
  token: ""                    # shared secret; mandatory for non-loopback binds
  presence_idle_timeout: "5m"  # idle CLI stops counting as "attached" (pushes go to IM)
  pending_notify_after: "2m"   # re-push an unanswered approval/question to IM after this
  delivery_max_message_chars: 3500
  delivery_retry_attempts: 3
```

Each IM platform is a subsection under `gateway`, all disabled by default:

```yaml
  weixin:                      # personal/enterprise WeChat (iLink); the primary WeChat path
    enabled: true
    owner_person_id: "person_xxx"   # senders bound to you (see security note)
    account_id: "xxx@im.bot"
    token: "xxx@im.bot:xxxx"
    dm_policy: "allowlist"     # open | allowlist | disabled  (see security note)
    allow_from: ["openid@im.wechat"]
    group_policy: "disabled"
  telegram_token: ""           # Telegram bot token (webhook/long-poll)
  feishu: { enabled: false, app_id: "", app_secret: "", verification_token: "", encrypt_key: "" }
  qq:     { enabled: false, app_id: "", secret: "", token: "" }
  wechat: { enabled: false, app_id: "", app_secret: "", token: "" }   # official account
```

> **Security — WeChat DM policy.** With `owner_person_id` set, every sender the
> DM policy admits is bound to *you* (your tasks, memory, workspaces). The
> default `dm_policy: open` therefore lets any stranger who DMs the account
> become you. **Set `dm_policy: allowlist` and list your own openid in
> `allow_from`** before exposing the account.

## 7. Tasks

```yaml
tasks:
  inbox_enabled: true
  default_list_limit: 10
  auto_archive_done_after: "720h"      # 30d; "0" disables that archive class
  auto_archive_cancelled_after: "168h" # 7d
  maintenance_model_role: "memory_extract"  # role for post-run label/fact maintenance
```

Auto-archive only touches stale, terminal, unpinned tasks with no live run.

## 8. Diagnostics: flight recorder

Records real turns locally so a bad turn can become a regression test (see the
"Diagnostics And Regression Capture" README section).

```yaml
flight_recorder:
  enabled: true
  keep: 30            # keep newest N turns, auto-prune older; local only, never uploaded
```

## 9. Cron & delegation

```yaml
cron:
  enabled: true       # scheduled jobs (daily summaries, liveness canary)
delegation:           # multi-agent sub-tasks; empty values use the default model
  provider: ""
  model: ""
  api_key: ""
  max_retries: 3
  max_iterations: 50
```

## 10. Intent routing (advanced — rarely changed)

```yaml
intent:
  mode: "hybrid"       # how explicit commands / continuation cues are matched
  thresholds:
    direct: 0.8
    ask: 0.55
```

Ordinary language always reaches the agent; these knobs only tune
explicit-command and continuation detection. Defaults are fine for most users.

## 11. MCP servers

```yaml
mcp:
  servers:
    - name: "my-tools"
      transport: "stdio"        # stdio | http
      command: "my-mcp-server"  # for stdio
      args: []
      url: ""                   # for http
      headers: {}
```

Empty by default. Each server's tools are registered on demand.

## 12. Agent tuning (advanced)

```yaml
agent:
  max_iterations: 90            # max tool-loop steps per turn
  max_retries: 3
  log_level: "INFO"             # DEBUG | INFO | WARN | ERROR
  llm_max_retries: 5            # transport retry attempts
  llm_retry_base: "300ms"       # backoff base
  llm_retry_cap: "30s"          # backoff cap
  llm_stream_idle_timeout: "180s"  # abort a stalled SSE stream after this
editor:
  large_paste_chars: 1000       # TUI large-paste detection
  large_paste_lines: 10
evolution:
  enabled: true                 # skill/self-improvement review
  min_complexity_threshold: 5
  nudge_interval: 10
```

Defaults are tuned for normal use; only touch these when diagnosing transport
flakiness or tuning the tool loop.

---

## Minimal config to get started

```yaml
model:
  provider: "codex-cli"
  default: "gpt-5.5"
provider_profiles:
  codex-cli:
    base_url: "https://chatgpt.com/backend-api/codex"
    protocol: "codex_responses"
    model: "gpt-5.5"
web:
  search_backend: "tavily"
  api_key: "tvly-xxxx"
```

Everything else falls back to sensible defaults. Add IM channels, role models,
and memory governance as your needs grow. **Restart the daemon after any
change.**
