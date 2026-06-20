# SelfMind Provider Runtime 规范

[中文开发指南](development-guide.zh-CN.md)

这份文档记录 SelfMind 接入多模型服务商的统一方式。目标不是为每个厂商堆一套独立逻辑，而是把 Hermes 这类 coding agent 的经验收敛成稳定边界：

- `ProviderProfile` 描述一个服务商是什么。
- `Resolver` 把配置、环境变量、OAuth、本地凭据解析成一次可执行的 `Runtime`。
- `llm.Adapter` 只实现协议族传输，例如 OpenAI Chat、Anthropic Messages、Codex Responses。
- `ProviderQuirks` 描述厂商差异，例如认证头、tool schema、thinking 参数、User-Agent。

CLI、IM、HTTP webhook、未来 SaaS 多端入口都必须复用同一套 runtime。渠道层只负责身份、任务、进度和消息呈现，不应该直接判断 Kimi、MiniMax、OpenAI 这类 provider 细节。

## 设计原则

1. Provider 是声明式配置，adapter 是协议实现。
2. 新厂商优先选择已有协议族，不新增 Go adapter。
3. 只有协议完全不同，才新增 `llm.Provider` 实现。
4. 认证、base URL、模型 fallback、模型列表来源放在 `internal/modelruntime/profile.go`。
5. 用户 YAML 和命令行覆盖放在 `internal/modelruntime/resolver.go` 合并。
6. 传输层只消费 `Runtime`，不再自行读取全局 config。
7. provider-specific 行为先落到 `ProviderQuirks`，避免在 adapter 中不断新增厂商 if。

## 核心文件

| 文件 | 职责 |
|---|---|
| `internal/modelruntime/profile.go` | 内置 provider catalog、协议族、认证类型、fallback models、quirks |
| `internal/modelruntime/resolver.go` | 解析 config/env/auth store/selection，输出 `Runtime` |
| `internal/modelruntime/catalog.go` | 拉取和缓存模型列表 |
| `internal/platform/config/loader.go` | YAML schema、环境变量展开、旧配置兼容 |
| `internal/app/agent.go` | 把 `Runtime` 转成具体 `llm.Provider` |
| `internal/kernel/llm/adapters.go` | OpenAI-compatible transport |
| `internal/kernel/llm/anthropic_adapter.go` | Anthropic-compatible transport |
| `internal/kernel/llm/responses_adapter.go` | Codex Responses transport |

## Runtime 数据流

```text
config.yaml / env / auth.json / CLI selection
  -> modelruntime.Resolver
  -> modelruntime.Runtime
  -> app.buildProviderFromRuntime
  -> llm.Adapter
  -> kernel.Agent / model gateway
```

`Runtime` 是 app 层和 adapter 层之间的唯一 provider 合约。新增渠道时不要绕过它。

## 上下文窗口与输出上限

SelfMind 和 Hermes 一样区分两个容易混淆的字段：

- `context_length`：模型总上下文窗口，包含输入和输出 token。CLI 底部 usage、未来上下文压缩预算、会话健康检查都应该使用它。
- `max_tokens`：单次响应输出上限，只影响模型本次最多生成多少 token。

解析优先级：

1. role/selection 的 `context_length`
2. `model.context_length`
3. `provider_profiles.<id>.context_length`
4. custom provider 的 `models.<model>.context_length`
5. 内置 provider/profile 或 `internal/modelruntime/context_length.go` 的模型族 fallback

不要把 `max_tokens` 当作上下文窗口展示，也不要在 CLI、IM 或 web 状态栏硬编码 `1M` 这类看起来真实但不可追溯的数字。无法解析时应显示未知或提示用户配置 `context_length`。

## ProviderProfile

内置 provider 在 `BuiltinProfiles()` 中注册。一个 profile 至少应该声明：

```go
ProviderProfile{
    ID: "example",
    DisplayName: "Example AI",
    Protocol: ProtocolOpenAICompatible,
    AuthType: AuthAPIKey,
    BaseURL: "https://api.example.com/v1",
    APIKeyEnvVars: []string{"EXAMPLE_API_KEY"},
    BaseURLEnvVar: "EXAMPLE_BASE_URL",
    ModelList: ModelListOpenAICompatible,
    FallbackModels: []string{"example-large", "example-fast"},
    Quirks: openAIQuirks(),
}
```

常用协议族：

| 常量 | 用途 |
|---|---|
| `ProtocolOpenAIChat` | OpenAI Chat Completions 原生形态 |
| `ProtocolOpenAICompatible` | OpenAI-compatible endpoint |
| `ProtocolAnthropic` | Anthropic Messages 兼容 endpoint |
| `ProtocolResponses` | Codex Responses 兼容 endpoint |

常用认证类型：

| 常量 | 用途 |
|---|---|
| `AuthAPIKey` | API key |
| `AuthExternalOAuth` | 复用外部 CLI OAuth 凭据 |
| `AuthMiniMaxOAuth` | MiniMax user-code OAuth |
| `AuthNone` | 本地或无需认证 |

## ProviderQuirks

`ProviderQuirks` 是厂商差异的声明式层。新增厂商时，先看是否能通过 quirks 表达。

| 字段 | 作用 | 常见值 |
|---|---|---|
| `AuthHeader` | 请求认证头策略 | `bearer`、`x_api_key`、`auto` |
| `ToolSchema` | tool JSON schema 修复策略 | `openai`、`anthropic`、`moonshot` |
| `SystemMessageMode` | system message 放置方式 | `top_level`、`inline` |
| `ThinkingMode` | thinking 参数策略 | `openai`、`anthropic`、`kimi`、`minimax`、`omit` |
| `UserAgent` | provider 需要的客户端标识 | 例如 `claude-code/0.1.0` |
| `SupportsTools` | 是否支持 native tools | bool |
| `SupportsStreaming` | 是否支持 stream | bool |
| `SupportsVision` | 是否支持视觉输入 | bool |

用户 YAML 里的 `quirks` 目前只开放 `auth_header`、`tool_schema`、`system_message_mode`、`thinking_mode`、`user_agent`。`SupportsTools`、`SupportsStreaming`、`SupportsVision` 属于内置 profile 元数据，新增内置 provider 时在 Go 代码中维护。

现有默认 helper：

| Helper | 适用 |
|---|---|
| `openAIQuirks()` | OpenAI-compatible，大多数 `Authorization: Bearer` provider |
| `anthropicQuirks()` | Anthropic Messages，大多数 `x-api-key` provider |
| `minimaxQuirks()` | MiniMax Anthropic-compatible，Bearer auth，MiniMax thinking |
| `kimiQuirks()` | Kimi Coding Plan，Moonshot schema，Kimi thinking 省略，Claude Code UA，HTTP/1.1 transport |

## 已内置的常用模型服务商

| Provider ID | 协议 | 认证 | 默认模型 |
|---|---|---|---|
| `openai` | `openai_chat` | API key | `gpt-4o` |
| `anthropic` | `anthropic_messages` | API key 或 Claude Code token | `claude-3-5-sonnet-20241022` |
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

推荐最小配置：

```yaml
model:
  provider: "kimi-coding"
  default: "kimi-for-coding"

provider_profiles:
  kimi-coding:
    api_key: "${KIMI_CODING_API_KEY}"
```

默认行为：

- `protocol=anthropic_messages`
- `base_url=https://api.kimi.com/coding`
- `model=kimi-for-coding`
- `max_tokens=32000`
- `User-Agent=claude-code/0.1.0`
- 禁用 HTTP/2，并把 TLS ALPN 限制为 `http/1.1`，按 Hermes/httpx 的 HTTP/1.1 行为访问 `/coding`
- tool schema 使用 `moonshot` 修复规则
- Anthropic-compatible 路径不发送 `thinking`

如果用户显式选择 OpenAI-compatible：

```yaml
provider_profiles:
  kimi-coding:
    api_key: "${KIMI_CODING_API_KEY}"
    base_url: "https://api.kimi.com/coding/v1"
    protocol: "openai_compatible"
    model: "kimi-for-coding"
```

resolver 会避免重复拼出 `/coding/v1/v1`，并对 OpenAI-compatible 请求发送 `reasoning_effort` 与 `thinking`。

## MiniMax Coding Plan

API key 方式：

```yaml
model:
  provider: "minimax"
  default: "MiniMax-M3"

provider_profiles:
  minimax:
    api_key: "${MINIMAX_API_KEY}"
```

中国区 API key：

```yaml
model:
  provider: "minimax-cn"
  default: "MiniMax-M3"

provider_profiles:
  minimax-cn:
    api_key: "${MINIMAX_CN_API_KEY}"
```

OAuth 方式：

```sh
selfmind auth login minimax-oauth
selfmind model set minimax-oauth MiniMax-M3
```

默认行为：

- global base URL: `https://api.minimax.io/anthropic`
- cn base URL: `https://api.minimaxi.com/anthropic`
- `Authorization: Bearer <token>`
- fallback models: `MiniMax-M3`、`MiniMax-M2.7`、`MiniMax-M2.7-highspeed`、`MiniMax-M2.5`
- `MiniMax-M3` 在 `coding_agent` role 默认使用 adaptive thinking
- `MiniMax-M2.x` 可按 `reasoning_effort` 映射 manual thinking budget

## 用户自定义 profile

如果某个 provider 不值得内置，但需要稳定保存，可以用 `provider_profiles`：

```yaml
model:
  provider: "example"
  default: "example-coder"

provider_profiles:
  example:
    api_key: "${EXAMPLE_API_KEY}"
    base_url: "https://api.example.com/v1"
    protocol: "openai_compatible"
    model: "example-coder"
    max_tokens: 32768
    reasoning_effort: "medium"
    headers:
      X-Client-Name: "SelfMind"
    quirks:
      auth_header: "bearer"
      tool_schema: "openai"
      system_message_mode: "inline"
      thinking_mode: "openai"
```

如果 provider 使用 Anthropic-compatible，但认证头是 Bearer：

```yaml
provider_profiles:
  example-anthropic:
    api_key: "${EXAMPLE_API_KEY}"
    base_url: "https://api.example.com/anthropic"
    protocol: "anthropic_messages"
    model: "example-claude"
    quirks:
      auth_header: "bearer"
      tool_schema: "anthropic"
      system_message_mode: "top_level"
      thinking_mode: "omit"
```

## 新增内置 Provider Checklist

1. 在 `internal/modelruntime/profile.go` 添加 `ProviderProfile`。
2. 选择现有协议族，不要先写新 adapter。
3. 设置 `APIKeyEnvVars`、`BaseURLEnvVar`、`ModelList`、`FallbackModels`。
4. 选择或新增最小必要的 `ProviderQuirks`。
5. 如果是 OAuth，先把 token 读写封装在 `internal/modelruntime`，不要放到 adapter。
6. 在 `internal/modelruntime/resolver_test.go` 覆盖默认协议、base URL、fallback 模型、quirks。
7. 如果新增 quirks 行为，在 `internal/kernel/llm` 增加 adapter 测试。
8. 如果 CLI picker 需要展示更友好的名称，更新 `internal/cliapp/model_commands.go` 测试。
9. 更新本文档的“已内置的常用模型服务商”表格。

## 测试建议

局部测试：

```sh
GOWORK=off go test ./internal/modelruntime ./internal/kernel/llm ./internal/app
```

全量测试：

```sh
GOWORK=off go test ./...
```

WSL 裸二进制构建：

```sh
GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/selfmind-linux-amd64/selfmind ./cmd/selfmind
```
