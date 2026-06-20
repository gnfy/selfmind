# SelfMind Provider Runtime

[中文规范](provider-runtime.zh-CN.md) · [Development Guide](development-guide.md)

SelfMind uses one provider runtime for CLI, IM channels, HTTP webhooks, and future SaaS policy. Channel code must not contain vendor-specific logic for Kimi, MiniMax, OpenAI, or other providers.

The runtime follows four boundaries:

- `ProviderProfile` describes a provider declaratively.
- `Resolver` combines config, environment variables, auth store entries, and per-command selections into a `Runtime`.
- `llm.Adapter` implements one protocol family, such as OpenAI Chat, Anthropic Messages, or Codex Responses.
- `ProviderQuirks` carries provider-specific wire behavior, such as auth headers, tool schema fixes, thinking parameters, and User-Agent.

User YAML `quirks` currently exposes `auth_header`, `tool_schema`, `system_message_mode`, `thinking_mode`, and `user_agent`. Capability flags such as tools, streaming, and vision are built-in profile metadata maintained in Go.

## Context Window vs Output Cap

SelfMind follows Hermes' distinction between two commonly confused fields:

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
| `internal/app/agent.go` | `Runtime` to concrete `llm.Provider` wiring |
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
- HTTP/2 disabled for `/coding`, including TLS ALPN restricted to `http/1.1`, matching Hermes/httpx HTTP/1.1 behavior
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

## Adding a Provider

1. Add a `ProviderProfile` in `internal/modelruntime/profile.go`.
2. Prefer an existing protocol family before writing a new adapter.
3. Set env vars, base URL env var, model-list mode, fallback models, and quirks.
4. Add resolver tests for protocol, base URL, fallback model, auth, and quirks.
5. Add adapter tests only when new wire behavior or quirks are introduced.
6. Update this document.

Useful verification:

```sh
GOWORK=off go test ./internal/modelruntime ./internal/kernel/llm ./internal/app
GOWORK=off go test ./...
```
