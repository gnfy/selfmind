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
models:
  primary:
    provider: "codex-cli"    # provider id, custom:<name>, or profile id
    model: "gpt-5.6-sol"
    reasoning: "xhigh"       # optional; omit or use auto for the model default

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
  codex-cli:
    base_url: "https://chatgpt.com/backend-api/codex"
    protocol: "codex_responses"
```

- `protocol` is one of `openai_chat`, `openai_compatible`, `anthropic_messages`,
  `codex_responses`. Pick the one your endpoint speaks.
- **External CLI auth reuse**: Codex CLI, Claude Code, Gemini CLI, Qwen CLI
  credentials can be reused instead of an API key — no `api_key` needed for
  those profiles.
- Use `provider_profiles` for any vendor beyond the three first-class ones;
  reference it with `models.primary.provider`.
- Provider blocks own connection/authentication. Model choice belongs in
  `models.primary`; endpoint-level `model` remains readable only for backward
  compatibility.
- **`providers` vs `provider_profiles`**: `providers` holds the three
  first-class vendor slots plus `custom` (which *creates* a new provider with
  its own protocol/URL); `provider_profiles` *overrides* any built-in provider
  by id (Kimi, MiniMax, OpenRouter, codex-cli, …). For `openai`, `anthropic`,
  and `google` the `providers` slot is authoritative — a `provider_profiles`
  entry under those three ids is ignored.

### Generic provider request options

SelfMind supports the same escape-hatch names used by the OpenAI Python SDK:
`extra_headers`, `extra_body`, and `extra_query`. Put vendor-wide values under
`provider_profiles.<id>`; use `models.roles.<role>` only when one bounded role
needs a different value. These options work across OpenAI Chat/Compatible,
Anthropic Messages, and Responses transports. See the
[OpenAI Python request-options documentation](https://github.com/openai/openai-python#undocumented-request-params)
and [DeepSeek user isolation documentation](https://api-docs.deepseek.com/quick_start/rate_limit).

Headers merge in layers; each higher layer overrides lower ones key by key:

| Layer (low → high) | Where | Typical use |
|---|---|---|
| protocol defaults | code (adapters) | `content-type`, auth header, `anthropic-version` |
| `model.extra_headers` | yaml, global | org-wide custom headers on every request |
| built-in profile | code | vendor compatibility (e.g. kimi-coding `User-Agent`), OpenRouter app attribution |
| `provider_profiles.<id>.extra_headers` | yaml, per provider | vendor-specific overrides |
| `models.roles.<role>.extra_headers` | yaml, per role | one role diverges |

```yaml
model:
  extra_headers:                  # global: lowest yaml layer
    X-Org-Proxy-Token: "..."

provider_profiles:
  deepseek:
    extra_headers:
      X-Org-Proxy-Token: "${ORG_PROXY_TOKEN}"
    extra_body:
      user_id: "selfmind-workstation-01"
    extra_query:
      api-version: "2026-08-11"
```

- `extra_body` is recursively merged over the normal typed request. An
  explicit value therefore overrides an automatically derived optional value;
  for example, `extra_body.user_id` overrides SelfMind's opaque DeepSeek
  `user_id`. When omitted, the derived non-personal value remains the default.
- `extra_query` preserves existing URL query values and replaces only keys it
  declares. Lists are encoded as repeated query parameters.
- String values inside all three maps support `${ENV_VAR}` expansion.
- DeepSeek documents `user_id` as an account-scoped scheduling/isolation key,
  not a credential. Still use an opaque stable identifier and do not put a
  name, email, phone number, prompt, or other personal data in it.
- **Emergency compatibility**: when a vendor starts requiring a new parameter
  and no SelfMind release has shipped the fix yet, add the matching `extra_*`
  option under its provider profile, then remove it after the built-in profile
  catches up.
- **Verify it took effect**: `selfmind model check` prints the merged headers
  with their origins and lists extra body/query keys without printing values.
- Legacy `headers` remains readable for compatibility, but new and generated
  configuration should use `extra_headers`; the latter wins on duplicate keys.
- Defaults deliberately live in code, not in generated yaml: a config file is
  a snapshot, and materialized defaults would pin stale compatibility values
  across upgrades.

Typed wire compatibility belongs under `quirks`; arbitrary vendor request
parameters belong under `extra_*`:

```yaml
provider_profiles:
  example-anthropic:
    quirks:
      auth_header: bearer
      tool_schema: anthropic
      thinking_mode: anthropic
      user_identity_field: auto
      http_version: auto
      prompt_cache: false       # explicit false overrides a built-in true
```

Omit a boolean quirk to inherit the built-in profile. Set it explicitly to
`true` or `false` only when the endpoint contract differs. Valid identity
values are `auto`, `user_id`, `metadata.user_id`, and `off`; valid HTTP values
are `auto`, `http1`, and `http2`. `system_message_mode` is deprecated and
ignored. `selfmind model check` shows the resolved contract and warnings.

## 2. Model routing

Background jobs run on cheaper/faster models than your main chat, so heavy
memory/skill work never spends your primary quota.

```yaml
models:
  source: "local"
  primary:
    provider: "codex-cli"
    model: "gpt-5.6-sol"
    reasoning: "xhigh"       # optional
  auxiliary:                  # default for bounded background work
    provider: "kimi-coding"
    model: "kimi-for-coding"
  roles:
    # Optional advanced exceptions. Unlisted background roles use auxiliary.
    fast_classifier: { model: "kimi-for-coding-fast" }
```

`models.primary` owns the foreground conversation. `models.auxiliary` is the
single default for `fast_classifier`, `memory_extract`, `background_review`,
`skill_curator`, `semantic_recall`, and `summarizer`. `models.roles.<role>` is
optional and has the highest priority; a partial override inherits missing
provider/model behavior from `models.auxiliary`.

`reasoning` and
`service_tier` are optional; `auto` or omission means the provider/model
default and sends no forced value. When capability metadata is available,
`selfmind model set` validates requested values and `selfmind model current`
shows the discovered defaults.

For `kimi-coding`, every role uses the provider default Anthropic Messages
transport (`https://api.kimi.com/coding/v1/messages`). This matches Hermes and
the wire contract of Kimi Coding Plan's `/coding` route. A role-level `protocol`
value is available only for custom gateways or installations with a different
wire contract; it should normally be omitted for Kimi Coding Plan.

There is no `default` role. Capability-specific roles such as `vision` do not
inherit the auxiliary model and must be configured explicitly when needed.
Old configurations that list every background role remain valid.

Smart approval is intentionally stricter than ordinary role inheritance. It
uses `fast_classifier` resolved from an explicit role or `models.auxiliary`;
legacy configurations may
fall back to an explicitly configured `background_review`, but approval triage
never silently uses `models.primary`. If neither route exists or responds in
time, the operation is escalated to the person.

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

## exec_sandbox (Loop Engineering P0-D)

Bubblewrap isolation for `terminal`, `verify`, and `execute_code` on Linux.
**Default ON, best effort.** `sandbox: auto` prefers an isolated read-only-root,
workspace-writable process that shares the daemon's network namespace by
default. If bubblewrap is unavailable, auto records a degraded host fallback;
`required: true` fails closed instead.

```yaml
exec_sandbox:
  enabled: true         # prefer bwrap for auto-mode exec calls
  required: false       # if true, refuse to exec when the sandbox is unavailable (fail-closed)
  allow_network: true   # share the daemon network namespace and inherited proxy/DNS settings
```

Each exec tool accepts `sandbox: auto|isolated|host`. `isolated` refuses when
the host cannot isolate. With `allow_network: true` (the default), isolated
commands keep filesystem isolation while sharing the daemon process's network
namespace and inherited proxy/DNS settings. Set it to `false` for a network-less
sandbox. `host` is an explicit escape hatch for host credentials or writable
host paths; it always goes through the approval funnel and is disabled entirely
when `required: true`. `selfmind gateway restart` preserves the running
daemon's executable search path plus `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY`,
and `NO_PROXY` (including lowercase variants). Current `PATH` entries lead and
old-only tool directories are appended, so a restart from an IDE or updater
does not hide user-installed commands. Explicit proxy values from the caller
always win. Other environment variables, including credentials, are not
copied. Install with `apt install bubblewrap`.

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
  outbound_retention: "336h"   # retain terminal delivery history for 14 days; 0 disables pruning
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
> `allow_from`** before exposing the account. The built-in
> `selfmind weixin login` command does this automatically for the scanned
> WeChat user and binds it to the current CLI person; manual YAML setup must
> preserve the same invariant.

## 7. Tasks

```yaml
tasks:
  inbox_enabled: true
  default_list_limit: 10
  auto_archive_done_after: "720h"      # 30d; "0" disables that archive class
  auto_archive_cancelled_after: "168h" # 7d
  maintenance_model_role: "memory_extract"  # role for post-run label/fact maintenance
  maintenance_fallback_roles: ["background_review", "fast_classifier"] # explicit cheap-role failover; never the primary model implicitly
  maintenance_debounce: "5m"       # wait for a quiet window before semantic maintenance
  maintenance_max_wait: "15m"      # force a batch even if runs keep arriving
  maintenance_batch_max_runs: 10   # never put more than this many runs in one call
  maintenance_quota_probe_initial: "15m" # first provider probe after a quota 403
  maintenance_quota_probe_max: "4h"      # maximum exponential probe interval
  maintenance_soft_probe_initial: "15m"  # first probe after a 200/empty output-exhaustion response
  maintenance_soft_probe_max: "1h"       # maximum backoff for soft provider failures
  maintenance_llm_timeout: "2m"    # bound for one analyzer provider call; too tight = deadline-exceeded retries and skipped learning
```

Auto-archive only touches stale, terminal, unpinned tasks with no live run.
`maintenance_fallback_roles` is ordered. After a non-retryable provider
failure, SelfMind tries each explicitly configured role, skips missing roles,
and never falls back to the primary coding model. Reusing the same provider in
every role does not provide provider-level failover. For example, keep Kimi as
the normal low-cost role and add a MiniMax backup:

```yaml
models:
  roles:
    memory_extract:
      provider: "kimi-coding"
      model: "kimi-for-coding"
    maintenance_backup:
      provider: "minimax"
      model: "MiniMax-M3"

tasks:
  maintenance_model_role: "memory_extract"
  maintenance_fallback_roles: ["maintenance_backup", "background_review"]
```

Run finalization always persists replayable evidence immediately. The three
batch settings only delay reversible task-label and long-term-memory governance;
they do not delay the final answer or recent conversation continuity. Batches
never cross tenant, person, or workspace boundaries. Empty or zero duration
values use the product defaults.

Maintenance roles that resolve to the same physical endpoint and credential
share one persistent quota circuit even when their role or model names differ.
The first quota 403 pauses queued jobs for that route without consuming their
retry budgets. SelfMind then permits one half-open probe after
`maintenance_quota_probe_initial`; repeated quota failures back off up to
`maintenance_quota_probe_max`. A successful probe closes the circuit and
replays the paused jobs. `/diag` shows blocked routes and the next probe time.
Configure a fallback role on a different provider or credential when
maintenance must continue while the primary Coding Plan quota is exhausted.

An HTTP 200 response with no usable text and an output-exhaustion reason such
as `max_tokens` opens a shorter soft circuit controlled by
`maintenance_soft_probe_initial` and `maintenance_soft_probe_max`. This avoids
re-sending the same maintenance prompt through every queued batch while still
recovering sooner than a confirmed quota outage. Provider/network failures do
not recursively split a batch; only a malformed multi-run response may be
bisected. `/diag models` shows calls, failures, circuit skips, and token usage
by maintenance route and model role.

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
      extra_headers: {}
```

Empty by default. Each server's tools are registered on demand.

## 12. Agent tuning (advanced)

```yaml
agent:
  max_iterations: 90            # max tool-loop steps per turn
  action_tool_budget: 12        # initial action-tool budget for tool-using turns
  action_tool_budget_step: 6    # evidence-gated extension size
  action_tool_budget_limit: 64  # hard action-tool ceiling
  max_budget_extensions: 9      # maximum evidence-gated extensions
  max_retries: 3
  log_level: "INFO"             # DEBUG | INFO | WARN | ERROR
  llm_max_retries: 5            # transport retry attempts
  llm_retry_base: "300ms"       # backoff base
  llm_retry_cap: "30s"          # backoff cap
  llm_stream_idle_timeout: "180s"  # abort a stalled SSE stream after this
  approval_triage_timeout: "30s"   # smart-mode cheap judge foreground budget
editor:
  large_paste_chars: 1000       # TUI large-paste detection
  large_paste_lines: 10
evolution:
  enabled: true                 # skill review plus deterministic workflow profiling
  mode: "auto-readonly"         # observe | shadow | auto-readonly
  min_complexity_threshold: 5
  nudge_interval: 10
  shadow_after_observations: 3
  promote_after_observations: 5
  min_shadow_runs: 3
  max_shadow_failure_rate: 0.05
```

Tool budgets apply uniformly across languages and task types. Extensions still
require new evidence; these knobs do not classify prompts or force simple
answers to use tools. Defaults are tuned for normal use; only touch these when
diagnosing transport flakiness or tuning the tool loop.

`approval_triage_timeout` is independent from the primary model transport
timeout. If the auxiliary/explicit `fast_classifier` does not return within
this budget, smart mode fails safe to a human approval prompt. The default is
30 seconds; lower values can turn a healthy reasoning-capable cheap model into
an apparent outage.

Evolution profiles are deterministic projections of completed run events; they
do not add a foreground model call. `observe` only records profiles,
`shadow` also evaluates bounded read-only batching candidates without using
them, and `auto-readonly` enables a candidate only after the configured
observation and zero/low-failure gates. Automatic evolution never batches
writes, shell commands, credentials, or network actions. `mode: auto` remains
accepted as a compatibility alias for `auto-readonly`.

---

## Minimal config to get started

```yaml
models:
  primary:
    provider: "codex-cli"
    model: "gpt-5.6-sol"
    reasoning: "xhigh"
  auxiliary:
    provider: "deepseek"
    model: "deepseek-v4-flash"
provider_profiles:
  codex-cli:
    base_url: "https://chatgpt.com/backend-api/codex"
    protocol: "codex_responses"
web:
  search_backend: "tavily"
  api_key: "tvly-xxxx"
```

Everything else falls back to sensible defaults. Add IM channels, per-role
model exceptions, and memory governance as your needs grow. **Restart the daemon after any
change.**

## 13. Update checks and feedback

```yaml
updates:
  enabled: true
  channel: "auto"         # auto (follow the installed version line) | latest | next
  check_interval: "15m"   # cached; never blocks TUI startup

feedback:
  repository: "gnfy/selfmind"  # default GitHub Issue destination
  labels: []                   # optional existing repository labels
  endpoint: ""                 # optional self-hosted collector override
```

`selfmind update check` reads npm registry dist-tags and only reports the
available version. It never replaces the running binary.
`selfmind update` is the single supported installation command shown by update
notices. It installs or refreshes the selected npm release, verifies the
launcher, and drains/restarts a running daemon. An equal version is refreshed;
a newer local build is preserved unless `--force` is explicit.

`selfmind feedback` writes a private, redacted local report by default.
`selfmind feedback --send "description"` uses the authenticated GitHub CLI to
create an Issue in `gnfy/selfmind` unless `feedback.repository` or `--repo`
overrides it. SelfMind never stores a GitHub token. Install and authenticate
the CLI with `gh auth login --hostname github.com`. Missing or expired
authentication leaves the local report intact and prints an actionable error
plus a pre-filled manual Issue URL.

An explicitly configured `feedback.endpoint` keeps the legacy self-hosted JSON
submission path and takes precedence over GitHub submission.
