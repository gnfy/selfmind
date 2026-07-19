# SelfMind Provider Runtime

[中文规范](provider-runtime.zh-CN.md) · [Development Guide](development-guide.md)

SelfMind uses one provider runtime for CLI, IM channels, HTTP webhooks, and future SaaS policy. Channel code must not contain vendor-specific logic for Kimi, MiniMax, OpenAI, or other providers.

The runtime follows four boundaries:

- `ProviderProfile` describes a provider declaratively.
- `Resolver` combines config, environment variables, auth store entries, and per-command selections into a `Runtime`.
- `llm.TransportConfig` is the only handoff from app/runtime into the LLM layer.
- `llm` transports implement one protocol family, such as OpenAI Chat, Anthropic Messages, or Codex Responses.
- `ProviderQuirks` carries provider-specific wire behavior, such as auth headers, tool schema fixes, thinking parameters, User-Agent, and Responses request flags.

Provider
differences are declarative profile/quirk data, while protocol adapters
normalize requests, streaming, tool calls, and usage into SelfMind's shared
`llm.Provider` interface. Channel code, gateway routing, task strategy, and IM
adapters must not contain vendor-specific logic.

User YAML `quirks` currently exposes `auth_header`, `tool_schema`, `system_message_mode`, `thinking_mode`, `user_agent`, `responses_store_false`, and `responses_require_stream`. Capability flags such as tools, streaming, and vision are built-in profile metadata maintained in Go.

`ProviderQuirks.PromptCache` opts an Anthropic-protocol provider into explicit prompt-cache breakpoints: the adapter attaches `cache_control: {"type":"ephemeral"}` to the last system content block and a rolling breakpoint on the last content block of the most recent message before the final user message (never more than 4 breakpoints). Built-in native Anthropic and MiniMax profiles enable it because those endpoints document the contract. Custom endpoints default off, and direct Kimi Coding remains off because its native coding endpoint has not established the same contract. With the quirk off, request bytes are unchanged. Usage accounting always parses `cache_read_input_tokens` / `cache_creation_input_tokens` into `llm.UsageStats`, and the kernel `token.updated` event adds `cache_read_input_tokens`, `cache_creation_input_tokens`, and `billed_input_tokens` (= `input_tokens` - `cache_read_input_tokens`). `/diag context` renders the latest run totals and hit rate.

## Context Window vs Output Cap

SelfMind distinguishes two commonly confused fields:

- `context_length`: the model's total input+output context window. CLI usage display, future compression budgets, and session health checks should use it.
- `max_tokens`: the per-response output cap sent to the provider.

Resolution priority:

1. role/selection `context_length`
2. `model.context_length`
3. `provider_profiles.<id>.context_length`
4. custom provider `models.<model>.context_length`
5. built-in provider/profile metadata or the fallback table in `internal/modelruntime/context_length.go`

Do not display `max_tokens` as the model context window, and do not hardcode fake values such as `1M` in CLI, IM, or web status surfaces. If the context window cannot be resolved, show it as unknown or ask the user to configure `context_length`.

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

## Kimi Coding Plan

Recommended minimal config:

```yaml
model:
  provider: "kimi-coding"
  default: "kimi-for-coding"

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
model:
  provider: "minimax"
  default: "MiniMax-M3"

provider_profiles:
  minimax:
    api_key: "${MINIMAX_API_KEY}"
```

OAuth config:

```sh
selfmind auth login minimax-oauth
selfmind model set minimax-oauth MiniMax-M3
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
- Responses-compatible adapters must normalize tool schemas before sending.
  In particular, `required` must be a JSON array, never `null`; nil Go slices
  from tool definitions must be converted to `[]`.
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
