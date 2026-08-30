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
- **Secrets**: enter provider API keys through the Model Manager. SelfMind keeps
  them in its private auth store, not in generated YAML. Environment references
  and literal keys remain readable only for compatibility; run
  `selfmind config upgrade` to migrate an old file.
- **Durations** are Go duration strings: `"300ms"`, `"30s"`, `"5m"`, `"24h"`.

---

## 1. Model & providers (required)

The one thing you must configure: which model answers you.

```yaml
models:
  primary:
    provider: "codex-cli"    # built-in or providers.custom map key
    model: "gpt-5.6-sol"
    reasoning: "xhigh"       # optional; omit or use auto for the model default
  auxiliary:
    enabled: true
    provider: "deepseek"
    model: "deepseek-v4-flash"

providers:
  deepseek:                  # optional override of a built-in provider
    base_url: "https://gateway.example.com/v1"
  custom:
    company-gateway:         # this map key is the provider id
      base_url: "https://ai.example.com/v1"
      protocol: "openai-compatible"
      auth: "bearer"
```

- A custom `protocol` is `openai-compatible`, `anthropic-compatible`, or
  `responses-compatible`; `auth` is `bearer`, `x-api-key`, or `none`.
- **External CLI auth reuse**: Codex CLI, Claude Code, Gemini CLI, Qwen CLI
  credentials can be reused instead of an API key.
- Built-in providers need no YAML block unless an endpoint or non-secret wire
  option is being overridden. User-defined connections live only under
  `providers.custom.<id>` and routes refer directly to `<id>`.
- Provider blocks own connection/authentication. Model choice belongs in
  `models.primary`; endpoint-level `model` remains readable only for backward
  compatibility.
- Credentials entered in Model Manager are stored at
  `auth.credentials_file`. Credential-bearing `extra_headers`, `extra_body`,
  and `extra_query` values are rejected.
- `provider_profiles` and provider IDs prefixed with `custom:` are compatibility
  reads only. `selfmind config upgrade` backs up and rewrites them.

### Generic provider request options

SelfMind supports the same escape-hatch names used by the OpenAI Python SDK:
`extra_headers`, `extra_body`, and `extra_query`. Put vendor-wide values under
`providers.<id>` for built-ins or `providers.custom.<id>` for custom
connections; use `models.roles.<role>` only when one bounded role needs a
different value. These options work across OpenAI Chat/Compatible,
Anthropic Messages, and Responses transports. See the
[OpenAI Python request-options documentation](https://github.com/openai/openai-python#undocumented-request-params)
and [DeepSeek user isolation documentation](https://api-docs.deepseek.com/quick_start/rate_limit).

Headers merge in layers; each higher layer overrides lower ones key by key:

| Layer (low → high) | Where | Typical use |
|---|---|---|
| protocol defaults | code (adapters) | `content-type`, auth header, `anthropic-version` |
| legacy `model.extra_headers` | yaml, global | compatibility read only |
| built-in profile | code | vendor compatibility (e.g. kimi-coding `User-Agent`), OpenRouter app attribution |
| `providers.<id>.extra_headers` or `providers.custom.<id>.extra_headers` | yaml, per provider | vendor-specific overrides |
| `models.roles.<role>.extra_headers` | yaml, per role | one role diverges |

```yaml
providers:
  deepseek:
    extra_headers:
      X-Client-Name: "SelfMind"
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
- String values inside all three maps support `${ENV_VAR}` expansion, but
  secrets and credential-shaped keys must use the private auth store.
- DeepSeek documents `user_id` as an account-scoped scheduling/isolation key,
  not a credential. Still use an opaque stable identifier and do not put a
  name, email, phone number, prompt, or other personal data in it.
- **Emergency compatibility**: when a vendor starts requiring a new parameter
  and no SelfMind release has shipped the fix yet, add the matching `extra_*`
  option under its provider profile, then remove it after the built-in profile
  catches up.
- **Inspect and validate it**: `selfmind model` shows the effective routes, and
  every completed selection runs an automatic bounded probe without printing
  header/body/query values.
- Legacy `headers` remains readable for compatibility, but new and generated
  configuration should use `extra_headers`; the latter wins on duplicate keys.
- Defaults deliberately live in code, not in generated yaml: a config file is
  a snapshot, and materialized defaults would pin stale compatibility values
  across upgrades.

Typed wire compatibility belongs under `quirks`; arbitrary vendor request
parameters belong under `extra_*`:

```yaml
providers:
  custom:
    example-anthropic:
      base_url: "https://anthropic.example.com"
      protocol: "anthropic-compatible"
      auth: "x-api-key"
      quirks:
        auth_header: x-api-key
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
ignored. Model Manager validates the resolved contract automatically and keeps
warnings with the draft.

## 2. Model routing

Background jobs use one explicit shared route, which may be the same physical
model as Main or a cheaper/faster model selected for bounded maintenance.

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

On a local installation, omitting the auxiliary provider/model intentionally
defaults them to the primary selection. Initial setup and the first accepted
Model Manager change materialize that choice in YAML. Once auxiliary has been
materialized or customized, changing primary does not overwrite it. This keeps
onboarding to two visible slots without exposing the internal role catalogue.

`reasoning` and
`service_tier` are optional; `auto` or omission means the provider/model
default and sends no forced value. When capability metadata is available,
Model Manager validates each completed selection and shows the discovered
defaults.

Model switching has no additional YAML keys. SelfMind stores the non-secret
transaction generation, pending change, last running snapshot, probe summaries,
verified-running timestamp, and bounded history in `model-state.json` beside
this file. That file is the sole authority for Model Readiness; onboarding does
not duplicate its routes. Do not edit it. A direct edit to `models.primary` or
`models.auxiliary` is treated as configured but unverified until daemon startup
probes it; use `selfmind model` for the normal validated path.

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
never adds an undeclared primary fallback. The effective auxiliary may
intentionally point at the same provider/model as primary by default. If no
usable route responds in time, the operation is escalated to the person.

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

Memory consolidation always uses the stable `memory_extract` semantic role.
To give it a dedicated model, configure `models.roles.memory_extract`; without
that override it uses `models.auxiliary`. Memory behavior settings do not
select model roles.

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
  presence_idle_timeout: "0"   # deprecated compatibility key; presence follows client process liveness
  pending_notify_after: "15m"  # attached CLI: escalate an unanswered approval/question after T1; detached pushes immediately
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

The combined post-run task-label and memory-intake pass always uses the stable
`memory_extract` role. Advanced model selection belongs only under
`models.roles.memory_extract`; task settings control scheduling and behavior.

Background maintenance runs through a two-position chain: the role's own
configuration first, then `models.auxiliary` as the shared floor. There is
nothing to configure for failover beyond pointing `models.auxiliary` at a
provider you trust. The chain never falls back to the primary coding model.
Auxiliary may intentionally point at the same physical model as primary; the
chain still does not add a separate implicit primary position.

Both positions are de-duplicated by physical route, so a role without its own
override resolves to `models.auxiliary` and honestly ends up with a
single-entry chain. To get real failover, give the role an override on a
different provider than `models.auxiliary`:

```yaml
models:
  auxiliary:
    provider: "minimax"      # the floor every background role degrades to
    model: "MiniMax-M3"
  roles:
    memory_extract:
      provider: "kimi-coding"
      model: "kimi-for-coding"
```

The `memory_extract` row in `selfmind model` shows the resolved fallback, or
says none is available when both positions share one endpoint and credential.

`memory.governance.model_role`, `tasks.maintenance_model_role`, and
`tasks.maintenance_fallback_roles` are deprecated. Model selection now belongs
only under `models`, and the old fallback list inserted extra role slots before
the auxiliary floor existed. `selfmind config doctor` identifies these keys;
`selfmind config upgrade` backs up the file and removes them.

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
      transport: "stdio"        # stdio | http | streamable_http
      command: "my-mcp-server"  # for stdio
      args: []
      url: ""                   # Streamable HTTP endpoint
      headers: {}                # optional HTTP headers
      auth: {}                   # optional bearer or user/pass static auth
      env_filter: []             # optional stdio environment allow-list
```

Empty by default. The daemon connects with the official MCP Go SDK at startup,
negotiates a compatible protocol version, follows paginated tool catalogues,
and applies live tool-list changes to the Dispatcher. A server that cannot
initialize or list its tools remains visible as a gateway health failure and in
`selfmind doctor`. Give every server a unique, stable `name`; an omitted name
gets a deterministic endpoint-derived compatibility name and a startup warning,
while a duplicate name is rejected instead of silently replacing a server.

MCP tools are unclassified external tools by default. Each invocation requires
a once-only human approval even in `full-auto`; it cannot reuse a grant or smart
triage until a reviewed per-tool trust policy exists. SelfMind sends only the
tool's public input fields to the remote endpoint and strips daemon-only
top-level underscore fields, including tenant/person/run scope and callbacks.
Remote servers currently
support static headers, bearer tokens, and basic authentication; SelfMind does
not yet expose an interactive MCP OAuth flow.

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
  approval_wait: "30m"             # resource wait while a live/healthy endpoint can answer
  approval_wait_unattended: "30s"  # resource wait when no endpoint can currently answer
editor:
  large_paste_chars: 1000       # TUI large-paste detection
  large_paste_lines: 10
tui:
  theme: "auto"                 # auto | dark | light | mono
history:
  persistence: "save-all"      # save-all | none
  max_bytes: 524288
  load_entries: 200
evolution:
  enabled: true                 # skill review plus deterministic workflow profiling
  mode: "observe"               # observe | shadow | auto-readonly (legacy modes observe only)
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

The TUI does not paint a full-screen background. `auto` follows terminal
capabilities; `dark` and `light` choose a contrast palette; `mono` removes
color while retaining structure and emphasis. `Enter` submits, `Ctrl+J`
inserts a portable newline, and `Ctrl+V` attaches a GUI-clipboard image.
Deleting its complete `[Image #N · name]` token detaches it. Input history is
person-local; `history.persistence: none` disables disk writes but keeps
in-session recall.

`approval_triage_timeout` is independent from the primary model transport
timeout. If the auxiliary/explicit `fast_classifier` does not return within
this budget, smart mode fails safe to a human approval prompt. The default is
30 seconds; lower values can turn a healthy reasoning-capable cheap model into
an apparent outage.

Approval wait values are resource budgets, not answer-expiry timers. A live
process uses `approval_wait`. Without one, no routable IM account or a latest
preferred-IM state of `pending_session`, `failed`, or `sent_unconfirmed` uses
`approval_wait_unattended`. When that shorter budget ends, the run parks and
releases its slot; the approval remains answerable for seven days. In
particular, a Weixin outage may make tasks park after roughly 30 seconds by
default. This is expected recovery behavior, not a rejected approval. The
caller's own remaining deadline may shorten either configured value.

Evolution profiles are deterministic projections of completed run events; they
do not add a foreground model call. The default `observe` mode records profiles
without enabling batching advice. `shadow`, `auto-readonly`, and the `auto`
alias remain accepted for configuration compatibility, but ordinary completed
runs now advance observation counts only. They never increment shadow matches,
revive degraded candidates, or authorize runtime advice. `batch_read` advice
requires a separately verified candidate-versus-baseline comparison contract;
the current profiler does not create one. Automatic evolution never batches
writes, shell commands, credentials, or network actions.

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
