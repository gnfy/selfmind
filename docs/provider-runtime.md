# SelfMind Provider Runtime

[中文规范](provider-runtime.zh-CN.md) · [Development Guide](development-guide.md)

SelfMind uses one provider runtime for CLI, IM channels, HTTP webhooks, and future SaaS policy. Channel code must not contain vendor-specific logic for Kimi, MiniMax, OpenAI, or other providers.

The runtime follows four boundaries:

- `ProviderProfile` describes a provider declaratively.
- `Resolver` combines config, environment variables, auth store entries, and per-command selections into a `Runtime`.
- `llm.TransportConfig` is the only handoff from app/runtime into the LLM layer.
- `llm` transports implement one protocol family, such as OpenAI Chat, Anthropic Messages, or Codex Responses.
- `ProviderQuirks` carries provider-specific wire behavior that changes how a
  protocol is encoded, such as auth style, tool-schema fixes, thinking shape,
  User-Agent compatibility, and Responses flags. Arbitrary vendor parameters
  belong in `extra_headers`, `extra_body`, or `extra_query`, not in quirks.

Provider
differences are declarative profile/quirk data, while protocol adapters
normalize requests, streaming, tool calls, and usage into SelfMind's shared
`llm.Provider` interface. Channel code, gateway routing, task strategy, and IM
adapters must not contain vendor-specific logic.

User YAML `quirks` exposes `auth_header`, `tool_schema`, `thinking_mode`,
`user_identity_field`, `user_agent`, `http_version`, `prompt_cache`,
`responses_store_false`, and `responses_require_stream`.
`system_message_mode` remains readable for compatibility but is deprecated and
ignored: protocol adapters own the system-message shape. Boolean quirks are
three-state in YAML: omission inherits the built-in profile, while explicit
`true` or `false` overrides it. This matters when a private endpoint needs to
disable a built-in cache or Responses behavior. Capability flags such as
tools, streaming, and vision are built-in profile metadata maintained in Go.

Quirk values are validated during resolution; invalid values and protocol
mismatches surface as actionable configuration errors. Adapters never infer a
vendor contract from the endpoint hostname. Built-in providers declare their
headers, HTTP version, schema repair, identity field, and thinking shape in the
profile; custom proxies receive the same behavior when they resolve the same
profile.

HTTP headers merge low-to-high as legacy `model.headers`,
`model.extra_headers`, built-in profile headers, legacy provider `headers`,
`provider_profiles.<id>.extra_headers`, and role/selection extra headers;
adapters set protocol defaults (`content-type`, auth, `anthropic-version`)
first and then apply the merged map, so yaml can
override any of them as an emergency compatibility escape hatch until a
release ships the fix. Compatibility defaults stay in Go (profile/adapters),
never materialized into generated yaml — a config file is a snapshot and
would pin stale values across upgrades. The resolver retains each merged
header's origin in `Resolver.HeaderOrigins` for diagnostics. `extra_body`
and `extra_query` merge provider-to-role, with the higher layer winning;
request-body objects merge recursively. The transport applies those options
only at the final HTTP boundary, so CLI, IM, cron, and future remote clients
share one wire contract.

`ProviderQuirks.PromptCache` opts an Anthropic-protocol provider into explicit prompt-cache breakpoints: the adapter attaches `cache_control: {"type":"ephemeral"}` to the last system content block and a rolling breakpoint on the last content block of the most recent message before the final user message (never more than 4 breakpoints). Built-in native Anthropic and MiniMax profiles enable it because those endpoints document the contract. Custom endpoints default off, and direct Kimi Coding remains off because its native coding endpoint has not established the same contract. With the quirk off, request bytes are unchanged. Usage accounting normalizes Anthropic cache reads/creation and OpenAI-compatible `prompt_cache_hit_tokens` / `prompt_cache_miss_tokens`, plus reported reasoning tokens, into `llm.UsageStats`. The kernel `token.updated` event records hit, miss, creation, output, and reasoning totals. `/diag context` renders the latest run totals and hit rate; `selfmind usage` renders the person-scoped 24-hour execution and token report. Neither command embeds provider prices.

Request-prefix diagnostics are a wrapper contract, not an adapter-only detail.
Every production provider wrapper, including the flight recorder/VCR and role
router, forwards `RequestFingerprinter` when the wrapped transport supports it.
Each provider-call context event records either the content-free prefix/block
hashes or an explicit unsupported reason. Daily reports expose coverage and
unique-prefix counts, so a missing wrapper or adapter probe cannot masquerade
as a stable cache baseline.

## Tool Schema Governance

Tool schemas cross two separate compatibility boundaries:

1. `tools.Registry` compiles every schema once into a provider-neutral,
   detached JSON Schema snapshot at registration time.
2. Each protocol adapter applies only its final wire rule to another detached
   copy. Adapters remain defensive, but they do not own catalogue validity.

The compiler auto-repairs only deterministic shape defects: null/empty or
duplicate `required`, missing object `properties`, missing array `items`, and
type-invalid defaults. Ambiguous defects such as an unknown required property,
invalid type, malformed composition, or enum/type conflict are never guessed.
Built-in tools make startup fail on any repair or error, so defects are caught
in CI. External MCP/plugin tools are quarantined individually: the daemon and
the rest of the catalogue continue, while the bad tool is neither advertised
nor executable.

MCP tools retain their complete raw `inputSchema` for compilation; the legacy
`ToolSchema` projection is used only for local argument coercion. Nested
objects, arrays, composition keywords, definitions, and additional-properties
rules therefore survive provider serialization. Long MCP names are normalized
to provider-safe 64-character names with a stable collision-resistant suffix.

`/diag tools` shows active/repaired/quarantined counts and redacted issue
paths. The gateway status endpoint carries the same aggregate, and
`selfmind doctor` includes it in the gateway health line. Schema hashes and
issue classes are observable; raw external schemas are not.

Runtime retry after a provider rejects a schema is deliberately not part of
this layer. Retrying the same turn with a silently changed tool contract can
alter behavior and duplicate work. Compatibility must be established before
dispatch by the compiler, adapter normalization, tests, and live model probes.

## Context Window vs Output Cap

SelfMind distinguishes two commonly confused fields:

- `context_length`: the model's total input+output context window. CLI usage display, future compression budgets, and session health checks should use it.
- `max_tokens`: the per-response output cap sent to the provider.

Resolution priority:

1. explicit role/primary `context_length` override
2. `provider_profiles.<id>.context_length`
3. built-in provider profile metadata
4. provider/local model metadata (for example Codex `models_cache.json`)
5. custom provider `models.<model>.context_length`
6. the conservative fallback table in `internal/modelruntime/context_length.go`

Do not display `max_tokens` as the model context window, and do not hardcode fake values such as `1M` in CLI, IM, or web status surfaces. If the context window cannot be resolved, show it as unknown or ask the user to configure `context_length`.

Normal users should omit `context_length`. It is an advanced override for
private/local models whose provider publishes no capability metadata.

## Reasoning and service tier

The primary selection lives only under `models.primary`. `reasoning` and
`service_tier` are optional. Omission or `auto` means provider/model default;
the resolver deliberately does not send a forced value. Supported values are
model capabilities, not a global hardcoded enum. The Model Manager validates
them when metadata is discoverable and otherwise preserves the explicit value
for compatible private endpoints.

For local onboarding, an auxiliary selection with no provider/model defaults
to the primary provider/model. The setup screen always shows and confirms both
routes; the background route is not a hidden side effect. Model commands that
create or change the initial selection materialize both slots. When setup is
reconciling an existing legacy file without rewriting it, the accepted pair is
also pinned in the setup receipt; a later route change invalidates that receipt
and opens repair instead of silently accepting a different background model.
After auxiliary is explicit, changing primary never overwrites it. Logical
background roles remain available as advanced `models.roles.<role>` overrides
and inherit auxiliary when omitted.

## Transactional route changes

All user-facing model changes converge on one daemon-owned transaction service.
`selfmind model` and bare `/model` in the TUI open the same Model Manager; the
IM surface exposes a read-only summary rather than a second mutation grammar.
An online change follows this state machine:

```text
awaiting_confirmation -> validating -> awaiting_safe_boundary -> draining
  -> restarting -> starting -> applied
                         |-> model-attributable probe failure -> rolled_back
                         \-> infrastructure/unknown failure -> recovery_required
```

The current run freezes its resolved route. A model-change restart waits for
that run, including a pending approval or clarification, instead of applying
the ordinary short restart timeout. Before `draining`, the person may cancel or
explicitly replace the candidate; afterward the transaction is frozen. New
work received while draining is stored in the durable queue and starts only in
the new daemon. The local TUI keeps its draft and reads the durable transaction
while HTTP is briefly unavailable. A slow notice appears after 30 seconds;
after the replacement process is started, failure to become healthy within 120
seconds enters explicit recovery. Three startup failures within five minutes
open the recovery circuit instead of allowing a service-manager restart loop.
There is one pending change and a monotonic generation; stale confirmations and
concurrent writers fail instead of overwriting newer intent. A pending preview
expires after ten minutes. History is bounded to ten terminal non-secret
snapshots, and rollback creates a fresh validated transaction from a previous
applied snapshot.

The state file is `model-state.json` beside the selected `config.yaml`. It
contains route selections, phase transitions, probe summaries, generations,
restart attempts, and history, but no credentials, provider headers, or raw
authentication data. The validated candidate is written to YAML only at the
safe boundary, after the current run is idle. Startup probes it again, then
records it as running only after runtime construction and the real `/health`
endpoint succeed. Only a deterministic, model-attributable startup probe
failure automatically restores the last running snapshot. Network, quota,
listener, service-manager, and unknown failures preserve the evidence in
`recovery_required`; the Model Manager's Change status screen offers retry or
restore against the last healthy routes.
Schema-v1 state is backed up before migrating to the current state machine.
Direct YAML edits are shown as configured but unverified until the next daemon
startup; another model transaction is refused while such drift exists.

Model discovery prefers a live provider catalogue, then a timestamped fresh
cache, then a visibly stale cache or built-in fallback. A model ID may always
be entered manually; that does not bypass the same contract probes. Existing
reasoning and service-tier settings survive a model selection only when known
compatible. Unknown compatibility resets the affected setting to provider
`auto` with a notice. Explicit values are authoritative.

Validation has two boundaries. The Model Manager automatically sends the
appropriate bounded contract probe after each completed selection: a foreground
probe for Main and a background or maintenance-JSON probe for Background and
its six managed roles. The final daemon transaction resolves and probes the
whole draft again inside the daemon environment before changing service state.
This prevents a shell credential from appearing healthy while the background
runtime cannot use it. A newly entered API key is copied to the `0600` SelfMind
auth store only after its probe succeeds; service definitions contain paths and
non-credential environment only. A failed probe keeps the draft editable and
never masquerades as verified.

For Anthropic Messages, `thinking_mode: anthropic` maps an explicit reasoning
effort to an enabled thinking budget (`low=4096`, `medium/default=8192`,
`high=16384`, `xhigh/max=32768`) and raises the response cap when necessary.
For OpenAI-compatible transports the adapter uses the protocol's
`reasoning_effort` or provider-specific thinking contract. An explicit typed
field is preferred; `extra_body` remains the emergency override at the final
wire boundary.

Background maintenance does not inherit the primary or auxiliary profile's
interactive reasoning level. Post-run analysis, memory consolidation, model
contract probes, and approval triage request disabled reasoning and bounded
output explicitly; the maintenance provider chain enforces that contract again
at dispatch. A user-selected `high` or `xhigh` auxiliary profile therefore
does not multiply routine governance cost, while foreground reasoning remains
unchanged.

`user_identity_field: auto` maps to `user_id` for OpenAI-compatible requests
and `metadata.user_id` for Anthropic Messages. The value is a stable opaque
SelfMind identifier derived from authenticated identity, never a raw tenant,
person, channel, email, or platform id. `off` disables it. An explicit
`extra_body.user_id` or `extra_body.metadata.user_id` wins over the automatic
value.

## Core Files

| File | Responsibility |
|---|---|
| `internal/modelruntime/profile.go` | Built-in provider profiles, aliases, protocols, auth type, fallbacks, quirks |
| `internal/modelruntime/resolver.go` | Config/env/auth/selection resolution into `Runtime` |
| `internal/modelruntime/catalog.go` | Model list fetching and cache |
| `internal/platform/config/loader.go` | YAML schema and compatibility |
| `internal/app/agent.go` | `Runtime` to `llm.TransportConfig` handoff |
| `internal/kernel/llm/transport.go` | Protocol transport registry and provider factory boundary |
| `internal/kernel/llm/adapters.go` | OpenAI-compatible transport |
| `internal/kernel/llm/anthropic_adapter.go` | Anthropic-compatible transport |
| `internal/kernel/llm/responses_adapter.go` | Codex Responses transport |

## Built-In Providers

| Provider ID | Protocol | Auth | Default model |
|---|---|---|---|
| `openai` | `openai_chat` | API key | `gpt-4o` |
| `anthropic` | `anthropic_messages` | API key or Claude Code token | `claude-3-5-sonnet-20241022` |
| `google` | `openai_compatible` | API key | `gemini-1.5-pro` |
| `openrouter` | `openai_compatible` | API key | `anthropic/claude-3.5-sonnet` |
| `kimi-coding` | `anthropic_messages` | API key | `kimi-for-coding` |
| `minimax` | `anthropic_messages` | API key | `MiniMax-M3` |
| `minimax-cn` | `anthropic_messages` | API key | `MiniMax-M3` |
| `minimax-oauth` | `anthropic_messages` | SelfMind auth store | `MiniMax-M3` |
| `deepseek` | `openai_compatible` | API key | `deepseek-chat` |
| `zai` | `openai_compatible` | API key | `glm-4.5` |
| `alibaba-coding-plan` | `openai_compatible` | API key | `qwen3-coder-plus` |
| `codex-cli` | `codex_responses` | External OAuth (Codex CLI login) | `gpt-5.5` |
| `claude-code` | `anthropic_messages` | External OAuth (Claude Code login) | `claude-3-5-sonnet-20241022` |
| `gemini-cli` | `openai_compatible` | External OAuth (Gemini CLI login) | `gemini-1.5-pro` |
| `qwen-cli` | `openai_compatible` | External OAuth (Qwen CLI login) | `qwen3-coder-plus` |

## OpenRouter app attribution

The built-in `openrouter` profile declares `HTTP-Referer` (app link), `X-Title`
(app name), and a `User-Agent` derived from `buildinfo.Version`. They are
profile headers rather than adapter defaults on purpose: a profile configured
with any other protocol, and every streaming call, bypasses the OpenRouter
adapter's own request builder, so adapter-set attribution reached almost no
real request. `provider_profiles.openrouter.extra_headers` still overrides
each of them for forks and proxies.

## DeepSeek V4

The built-in `deepseek` profile uses the OpenAI-compatible transport and
enables DeepSeek's thinking/tool contract. `models.primary.reasoning: high`
is sent as `thinking.type=enabled` with level `high`; `xhigh` maps to the
provider's `max` level. When a thinking response calls a tool, the adapter
preserves and replays `reasoning_content` with the assistant tool call before
the matching tool result. Dropping it makes the next provider request invalid.

DeepSeek requests also carry an optional `user_id`. SelfMind never sends a raw
person, tenant, channel, email, or platform ID. `StableProviderUserID` derives
an opaque, versioned `sm_...` value from the authenticated tenant/person pair;
it remains stable across channels and runs but changes across people. The
field is sent only when a provider profile declares
`user_identity_field: user_id`.

The Model Manager's automatic route validation and `doctor --probe-models`
validate both the ordinary native tool schema and the complete thinking tool
loop: reasoning + tool call, tool result replay, then a final assistant answer.

## Kimi Coding Plan

Recommended minimal config:

```yaml
models:
  primary:
    provider: "kimi-coding"
    model: "kimi-for-coding"

provider_profiles:
  kimi-coding:
    api_key: "${KIMI_CODING_API_KEY}"
```

Default behavior:

- `protocol=anthropic_messages`
- `base_url=https://api.kimi.com/coding`
- `max_tokens=32000`
- `User-Agent=claude-code/0.1.0`
- HTTP/2 disabled for `/coding`, including TLS ALPN restricted to `http/1.1`, because the endpoint only behaves correctly with an HTTP/1.1-only client
- Moonshot tool schema repair
- no Anthropic `thinking` field on the Kimi Anthropic-compatible path

## MiniMax Coding Plan

API key config:

```yaml
models:
  primary:
    provider: "minimax"
    model: "MiniMax-M3"

provider_profiles:
  minimax:
    api_key: "${MINIMAX_API_KEY}"
```

OAuth config:

```sh
selfmind auth login minimax-oauth
selfmind model
```

Default behavior:

- global base URL: `https://api.minimax.io/anthropic`
- CN base URL: `https://api.minimaxi.com/anthropic`
- `Authorization: Bearer <token>`
- fallback models: `MiniMax-M3`, `MiniMax-M2.7`, `MiniMax-M2.7-highspeed`, `MiniMax-M2.5`
- `MiniMax-M3` uses adaptive thinking for the `coding_agent` role

## External CLI Auth Reuse

External CLI auth reuse is a compatibility bridge, not a new login system.
Keep credential parsing and refresh in `internal/modelruntime`; adapters should
call `Runtime.TokenGetter` before sending each request. When a provider returns
an auth failure such as `401 token_expired`, an adapter may call
`Runtime.TokenRefresher` and replay the same request once. Do not put token file
parsing, OAuth refresh payloads, or provider login state inside an LLM adapter.

- `codex-cli` reads `~/.codex/auth.json` or `CODEX_HOME/auth.json`, refreshes
  expired ChatGPT OAuth access tokens through the Codex OAuth client, preserves
  the existing `tokens.account_id`, and sends requests through
  `llm.ResponsesAdapter`.
- Codex's backend requires stateless, stream-only Responses calls, so the
  built-in `codex-cli` profile sets
  `ProviderQuirks.ResponsesStoreFalse=true` and
  `ProviderQuirks.ResponsesRequireStream=true`; the adapter must serialize
  `"store": false` and use streaming even when a caller asks for non-streaming
  fallback. Do not rely on provider defaults for these flags.
- Every native-tool adapter normalizes tool schemas before sending. Empty or
  invalid `required` values are omitted for Chat Completions and Anthropic
  Messages, so optional-only tools never serialize `required: null`.
  Responses-compatible adapters apply their stricter protocol rule afterwards
  and serialize an empty required set as `[]`.
- The Model Manager automatically sends one bounded request with an
  optional-only probe tool when a completed route's transport advertises native
  tools. This verifies the real protocol adapter and endpoint rather than only
  resolving configuration. `doctor --probe-models` uses the same schema probe
  while grouping roles that share one provider route.
- Responses-compatible adapters must also normalize tool names on the wire.
  The provider-facing name must match `^[a-zA-Z0-9_-]+$`; keep an alias table
  inside the adapter so returned tool calls are mapped back to SelfMind's
  original internal names before dispatch.
- For stateless Responses providers, every tool result replay must include the
  assistant `function_call` item before the matching `function_call_output`
  item in the same request input. The server cannot resolve prior response
  items when `store=false`.
- If Codex refresh fails or the refresh token is no longer valid, surface an
  actionable "run `codex login`" error instead of dumping raw provider JSON.
- `CODEX_ACCESS_TOKEN` remains a static override and is not refreshed by
  SelfMind.
- MiniMax OAuth uses the same contract: `TokenGetter` refreshes near expiry,
  while `TokenRefresher` handles server-side invalidation discovered on a
  request.

## Transport Resilience

Streaming against stateless Responses backends (codex `store=false`) hits
transient `Post .../responses: EOF` connection drops. The retry/backoff layer
absorbs these without touching the wire contract:

- The agent retry loop (`internal/kernel/agent.go`
  `streamChatWithRetry`/`chatResponseWithRetry`) re-sends only on **retryable**
  errors with exponential backoff `base*2^(attempt-1)` and `[0.9,1.1)` jitter,
  capped, with a context-cancellable sleep. Classification lives in
  `internal/kernel/llm/retryable.go` (`IsRetryableError`): EOF /
  `io.ErrUnexpectedEOF` / connection reset/refused / `net.Error` timeout / 5xx
  / 429 / stream-idle are retryable; context-window, quota/usage limit,
  401/invalid-auth, and 400 invalid-request are **fatal** and fail fast. Keep
  new provider errors classifiable — surface a status code or a recognizable
  phrase, not opaque text.
- 429 `Retry-After` is honored (`RetryAfterFromError`): the header is folded
  into the error via `foldRetryAfter` at the adapter 4xx/5xx return sites, and
  the codex/OpenAI "try again in N" body phrasing is parsed. Capped at 600s.
- The SSE idle watchdog (`responses_adapter.go` `streamIdleTimeout` +
  `streamResponse`) aborts a stream that stalls without new data, emitting a
  retryable stream-idle error so the loop reconnects. It is config-driven
  (`SELFMIND_STREAM_IDLE_TIMEOUT` env > config default > 180s) and never
  changes `store`/`stream` flags.
- Provider HTTP calls use the shared `llm.ProviderHTTPClient()` with TCP
  keepalive (`httpclient.go`) so dead sockets surface fast. The Kimi
  HTTP/1.1-only path clones the same keepalive transport.
- **No cursor resume.** With `store=false` the server never persisted the
  response, so `previous_response_id` resume is impossible — a retry is always
  a full re-send. Do not attempt partial-resume.

Config knobs (`agent:` section): `llm_max_retries` (default 5),
`llm_retry_base` (`300ms`), `llm_retry_cap` (`30s`), `llm_stream_idle_timeout`
(`180s`). Absent/0 = defaults.

## Adding a Provider

1. Add a `ProviderProfile` in `internal/modelruntime/profile.go`.
2. Prefer an existing protocol family before writing a new adapter.
3. Set env vars, base URL env var, model-list mode, fallback models, and quirks.
4. Do not add provider-name branches in `internal/app`, gateway, CLI, IM adapters,
   or task strategy. If an existing protocol is sufficient, the transport
   registry should keep working without code outside `internal/modelruntime`.
5. Add resolver tests for protocol, base URL, fallback model, auth, and quirks.
6. Add transport/adapter tests only when new wire behavior or quirks are introduced.
7. Update this document.

Useful verification:

```sh
GOWORK=off go test ./internal/modelruntime ./internal/kernel/llm ./internal/app
GOWORK=off go test ./...
```
