# SelfMind Provider Runtime 规范

[中文开发指南](development-guide.zh-CN.md)

这份文档记录 SelfMind 接入多模型服务商的统一方式。目标不是为每个厂商堆一套独立逻辑，而是把成熟 coding agent 的多模型接入经验收敛成稳定边界：

- `ProviderProfile` 描述一个服务商是什么。
- `Resolver` 把配置、环境变量、OAuth、本地凭据解析成一次可执行的 `Runtime`。
- `llm.TransportConfig` 是 app/runtime 进入 LLM 层的唯一交接结构。
- `llm` transport 只实现协议族传输，例如 OpenAI Chat、Anthropic Messages、Codex Responses。
- `ProviderQuirks` 只描述会改变协议编码方式的厂商差异，例如认证方式、tool
  schema 修复、thinking 形状和 User-Agent 兼容。任意厂商请求参数应使用
  `extra_headers`、`extra_body`、`extra_query`，不再继续扩张 quirks。

CLI、IM、HTTP webhook、未来 SaaS 多端入口都必须复用同一套 runtime。渠道层只负责身份、任务、进度和消息呈现，不应该直接判断 Kimi、MiniMax、OpenAI 这类 provider 细节。

这条边界的原则是：provider 差异尽量沉淀为 profile/quirks 数据，协议 adapter 负责把请求、流式输出、tool call、usage 统一成 `llm.Provider`。渠道、任务策略、IM 和 UI 都不应该知道某个厂商的特殊 HTTP 参数。

## 设计原则

1. Provider 是声明式配置，adapter 是协议实现。
2. 新厂商优先选择已有协议族，不新增 Go adapter。
3. 只有协议完全不同，才新增 `llm.Provider` 实现。
4. 认证、base URL、模型 fallback、模型列表来源放在 `internal/modelruntime/profile.go`。
5. 用户 YAML 和命令行覆盖放在 `internal/modelruntime/resolver.go` 合并。
6. app 层只负责把 `Runtime` 转成 `llm.TransportConfig`，不按 provider/protocol 选择具体 adapter。
7. 传输层只消费 `TransportConfig`，不再自行读取全局 config。
8. 协议行为差异落到 `ProviderQuirks`；普通厂商参数落到 `extra_*`，避免在
   adapter 中不断新增厂商 if。
9. `extra_body` 与 `extra_query` 按 provider → role 合并，角色层优先；请求体
   对象递归合并。它们只在最终 HTTP transport 边界生效，CLI、IM、cron 与未来
   远程入口不会各自实现一套。
10. 请求前缀诊断是 provider wrapper 的合约，而不是仅由 adapter 负责。role
    router、飞行记录器/VCR 等生产 wrapper 必须转发 `RequestFingerprinter`；每次
    provider 调用要么记录不含正文的 prefix/block 哈希，要么记录明确的不支持原因。
    `/diag context` 展示最近一次 run 的 token 与缓存状态，`selfmind usage` 展示当前
    person 最近 24 小时的执行与 token 报告；两者都不估算货币成本。

## 核心文件

| 文件 | 职责 |
|---|---|
| `internal/modelruntime/profile.go` | 内置 provider catalog、协议族、认证类型、fallback models、quirks |
| `internal/modelruntime/resolver.go` | 解析 config/env/auth store/selection，输出 `Runtime` |
| `internal/modelruntime/catalog.go` | 拉取和缓存模型列表 |
| `internal/platform/config/loader.go` | YAML schema、环境变量展开、旧配置兼容 |
| `internal/app/agent.go` | 把 `Runtime` 转成 `llm.TransportConfig` |
| `internal/kernel/llm/transport.go` | 协议族 transport registry，把 `TransportConfig` 构造成 `llm.Provider` |
| `internal/kernel/llm/adapters.go` | OpenAI-compatible transport |
| `internal/kernel/llm/anthropic_adapter.go` | Anthropic-compatible transport |
| `internal/kernel/llm/responses_adapter.go` | Codex Responses transport |

## Runtime 数据流

```text
config.yaml / env / auth.json / CLI selection
  -> modelruntime.Resolver
  -> modelruntime.Runtime
  -> app.buildProviderFromRuntime
  -> llm.TransportConfig
  -> llm.BuildTransportProvider
  -> llm transport
  -> kernel.Agent / model gateway
```

`Runtime` 是 app 层和 modelruntime 层之间的唯一 provider 合约；`TransportConfig` 是 app 层和 LLM 传输层之间的唯一 provider 合约。新增渠道时不要绕过它们。

## 工具 Schema 治理

工具 schema 需要经过两个相互独立的兼容边界：

1. `tools.Registry` 在注册时把每个 schema 编译成 provider-neutral、与
   来源对象分离的 JSON Schema 快照。
2. 协议 adapter 只对另一个副本应用最终 wire 规则。adapter 保留防御性
   归一化，但不再负责判断工具目录本身是否合法。

编译器只自动修复语义确定的结构问题：`required` 为 null、空集合或有重复项，
对象缺少 `properties`，数组缺少 `items`，以及 default 与声明类型明显冲突。
required 指向不存在属性、非法 type、损坏的组合 schema、enum/type 冲突等歧义
问题不会被猜测修复。内置工具只要发生修复或错误，就让启动失败，从而在 CI
和发布前暴露缺陷；MCP/plugin 等外部工具则只隔离单个坏工具，daemon 和其余
工具继续工作，被隔离工具既不会发给模型，也不能执行。

MCP 工具保留完整原始 `inputSchema` 供编译，旧 `ToolSchema` 投影只服务于本地
参数转换。因此嵌套对象、数组、组合关键字、definitions 和
additional-properties 规则都能完整进入 provider 请求。过长 MCP 工具名会稳定
收敛到 64 字符，并追加防碰撞哈希。

`/diag tools` 显示 active/repaired/quarantined 数量以及脱敏后的问题路径；gateway
状态接口携带同一聚合，`selfmind doctor` 会把它放进 gateway 健康行。系统只展示
schema 哈希和问题类别，不展示外部原始 schema。

本层明确不做 provider 拒绝 schema 后的自动重试。静默改变工具契约后重放同一
轮可能改变行为或重复副作用；兼容性应在分发前通过编译器、adapter 归一化、测试
和 live model probe 建立。

## 上下文窗口与输出上限

SelfMind 区分两个容易混淆的字段：

- `context_length`：模型总上下文窗口，包含输入和输出 token。CLI 底部 usage、未来上下文压缩预算、会话健康检查都应该使用它。
- `max_tokens`：单次响应输出上限，只影响模型本次最多生成多少 token。

解析优先级：

1. role/primary 显式设置的 `context_length`
2. `provider_profiles.<id>.context_length`
3. 内置 provider profile 元数据
4. provider/本地模型能力元数据（例如 Codex 的 `models_cache.json`）
5. custom provider 的 `models.<model>.context_length`
6. `internal/modelruntime/context_length.go` 的保守 fallback

不要把 `max_tokens` 当作上下文窗口展示，也不要在 CLI、IM 或 web 状态栏硬编码 `1M` 这类看起来真实但不可追溯的数字。无法解析时应显示未知或提示用户配置 `context_length`。

普通用户应省略 `context_length`。它只作为私有/本地模型无法提供能力元数据时
的高级覆盖项。

## 推理等级与 service tier

主模型只在 `models.primary` 选择。`reasoning` 和 `service_tier` 都是可选项；
省略或写 `auto` 表示使用 provider/模型默认值，resolver 不会强制发送该字段。
支持哪些取值由具体模型的能力元数据决定，不维护一份全局硬编码枚举。
模型管理器在能发现元数据时动态校验；私有 endpoint 没有元数据时，仍会保留用户
显式配置的兼容值。

本地初始化时，未填写 provider/model 的 auxiliary 默认使用 primary 的
provider/model。缺少“模型就绪”时，直接交互启动会打开与 `selfmind model` 相同的
Model Manager；显式运行 `selfmind setup` 只会引导到该入口，不会实现另一套选择器。
Model Manager 会同时展示并校验两条路由，因此后台模型不是隐藏副作用；首次事务会把
两个槽位都明确落盘。
`model-state.json` 中已应用的事务是“模型就绪”的唯一权威；onboarding 不再复制
路由或验证事实，因此之后成功应用的路由变更不会重新打开无关的工作区或后台服务
步骤。有效的旧设置回执只会作为一次性迁移证据，并在移除其中的模型字段前备份。
auxiliary 一旦显式配置，
修改 primary 就不会覆盖它。逻辑后台角色继续作为高级 `models.roles.<role>` 覆盖
存在；未配置角色覆盖时继承 auxiliary。

## 模型路由事务

所有面向用户的模型修改都汇入 daemon 拥有的同一个事务服务。`selfmind model` 与
TUI 中不带参数的 `/model` 打开同一个模型管理器；IM 只展示只读摘要，不再维护第二套
修改语法。在线修改遵循以下状态机：

```text
等待确认 -> 校验中 -> 等待安全边界 -> 排空 -> 重启中 -> 启动中 -> 已应用
                                      |-> 可归因于模型的探测失败 -> 已回滚
                                      \-> 基础设施/未知失败 -> 需要恢复
```

当前 run 会冻结已经解析的路由。模型变更重启会等待该 run，包括等待中的审批或
澄清，而不会使用普通重启的短超时强制中断。在进入“排空”前，用户可以取消或显式
替换候选项；之后事务会冻结。drain 期间收到的新工作写入持久队列，只会由新 daemon
启动。本地 TUI 在 HTTP 短暂不可用时会保留草稿，并直接读取持久事务。30 秒后会提示
进度较慢；替换进程启动后 120 秒仍未健康则进入显式恢复。五分钟内连续三次启动失败
会打开恢复熔断，而不是让服务管理器无限重启。系统只允许一个 pending 变更，并使用
单调 generation；过期确认和并发旧写入会失败，不会覆盖更新的意图。预览十分钟后
过期。历史最多保留十条不含密钥的终态快照；回滚会从上一条成功快照创建一笔新的、
经过校验的事务。

状态文件是所选 `config.yaml` 同目录下的 `model-state.json`。其中只有路由选择、
运行快照验证时间、阶段转换、探测摘要、generation、重启次数和历史，不包含凭据、
provider header 或原始认证数据。经过校验的候选项只有在当前 run 空闲、到达安全
边界后才写入 YAML。
启动时还会再次探测，并且只有运行时构建和真实 `/health` 端点成功后才记录为 running。
只有确定且可归因于模型的启动探测失败才会自动恢复上一条运行快照。网络、额度、
监听端口、服务管理器和未知失败会保留证据并进入 `recovery_required`；模型管理器的
“变更状态”页面可以重试候选项，或显式恢复上一条健康路由。schema v1 和 v2 状态
迁移到带就绪证据的新状态机前都会先创建备份。直接编辑 YAML 后只会显示为
configured、尚未验证；在下一次 daemon 启动完成验证前，系统拒绝再创建另一笔模型事务。
Gateway 状态只公开有效路由快照的哈希。onboarding 使用这个不含秘密的指纹，在允许
Runtime Degraded 前排除模型漂移；仅有 live health 永远不会建立“模型就绪”。provider
凭据和 endpoint 配置从不参与该指纹。
“模型就绪”不完整时，无模型控制命令仍然可用，但所有新的 agent 消息只会持久化入队，
不会在未验证路由上执行；每次模型驱动的维护或记忆治理 pass 也受同一边界约束。取消
预览并恢复“模型就绪”后，停放队列会立即按原顺序唤醒。显式恢复会先等待持有失败候选项
的进程释放 Gateway 所有权，再清除旧的运行证明、恢复上一条健康选择并启动替代进程；
受管恢复只等待现有服务管理器启动的进程，绝不会再用 detached fallback 与其竞争。
只有启动探测和 `/health` 重新建立 verified-running 边界后，队列才会继续运行。

模型发现优先使用 provider 实时目录，其次是带时间戳的新鲜缓存，再降级到明确标注
为 stale 的缓存或内置 fallback。用户始终可以手工输入模型 ID，但仍必须通过同样的
契约探测。只有已知兼容时才会保留原 reasoning 和 service tier；兼容性未知时只把
受影响选项恢复为 provider `auto` 并提示。显式输入始终优先。

校验分为两个边界。模型管理器在每一项选择完成后自动发送相应的有界契约探测：主模型
使用前台探测，后台模型及六个受管理角色使用后台或维护 JSON 探测。最终 daemon 事务
会在修改服务状态前，在 daemon 自己的环境中再次解析并探测整份草稿。这样可以避免
只在 shell 中存在的凭据被误判为后台运行也可用。新输入的 API key 只有探测成功后
才会复制到权限为 `0600` 的 SelfMind 私有凭据文件；服务定义只包含路径和非凭据环境。
探测失败会保留可编辑草稿，绝不会伪装成已验证。

Anthropic Messages 在 `thinking_mode: anthropic` 下，会把显式推理等级映射为 thinking
预算：`low=4096`、`medium/default=8192`、`high=16384`、
`xhigh/max=32768`，必要时同步提高响应上限。OpenAI-compatible transport 使用协议的
`reasoning_effort` 或 provider 专属 thinking 契约。优先使用这些有类型字段；
`extra_body` 只作为最终 wire 边界的应急覆盖。

后台维护不会继承主模型或辅助模型用于交互任务的高推理等级。Post-run 分析、记忆
合并、模型契约探针和审批判定都会显式请求关闭推理并限制输出；维护 provider 链在
实际派发时再次执行同一约束。因此，即使用户把辅助模型设为 `high` 或 `xhigh`，
日常治理也不会按该档位放大成本，前台任务的推理设置则保持不变。

`user_identity_field: auto` 在 OpenAI-compatible 请求中映射为 `user_id`，在
Anthropic Messages 中映射为 `metadata.user_id`。值是从认证身份派生的稳定匿名 ID，
不会发送原始 tenant、person、channel、邮箱或平台 ID；`off` 可以禁用。显式
`extra_body.user_id` 或 `extra_body.metadata.user_id` 的优先级更高。

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
| `SystemMessageMode` | 已废弃的兼容字段；system 形态由协议 adapter 决定 | 不建议配置 |
| `ThinkingMode` | thinking 参数策略 | `openai`、`anthropic`、`kimi`、`minimax`、`deepseek`、`omit` |
| `UserIdentityField` | 可选的 provider 侧稳定匿名用户字段 | `auto`、`user_id`、`metadata.user_id`、`off` |
| `UserAgent` | provider 需要的客户端标识 | 例如 `claude-code/0.1.0` |
| `HTTPVersion` | transport 的 HTTP 版本约束 | `auto`、`http1`、`http2` |
| `PromptCache` | Anthropic 显式 `cache_control` | bool |
| `ResponsesStoreFalse` | Responses 请求强制 `"store": false`（无状态服务端） | bool |
| `ResponsesRequireStream` | Responses 请求强制流式（服务端拒绝非流式） | bool |
| `SupportsTools` | 是否支持 native tools | bool |
| `SupportsStreaming` | 是否支持 stream | bool |
| `SupportsVision` | 是否支持视觉输入 | bool |

用户 YAML 里的 `quirks` 开放 `auth_header`、`tool_schema`、`thinking_mode`、
`user_identity_field`、`user_agent`、`http_version`、`prompt_cache`，以及
Responses-compatible endpoint 使用的 `responses_store_false` 和
`responses_require_stream`。布尔 quirks 是三态：省略表示继承内置 profile，显式
`true` 或 `false` 表示覆盖；因此私有 endpoint 可以关闭内置缓存或 Responses 行为。
`system_message_mode` 只为兼容旧配置继续读取，现已忽略。`SupportsTools`、
`SupportsStreaming`、`SupportsVision` 属于内置 profile 元数据。

resolver 会校验 quirks 取值；非法值和协议不匹配会形成可操作的配置错误。协议
adapter 不再根据 endpoint hostname 猜测厂商；内置 profile 声明 header、HTTP
版本、schema 修复、匿名身份和 thinking 形态，自定义代理解析到同一 profile 时得到
相同契约。

现有默认 helper：

| Helper | 适用 |
|---|---|
| `openAIQuirks()` | OpenAI-compatible，大多数 `Authorization: Bearer` provider |
| `anthropicQuirks()` | Anthropic Messages，大多数 `x-api-key` provider |
| `minimaxQuirks()` | MiniMax Anthropic-compatible，Bearer auth，MiniMax thinking |
| `kimiQuirks()` | Kimi Coding Plan，Moonshot schema，Kimi thinking 省略，Claude Code UA，HTTP/1.1 transport |

## 外部 CLI 登录态复用

外部 CLI 登录态复用只是兼容桥，不是新的登录系统。凭据读取和刷新必须放在
`internal/modelruntime`；adapter 每次请求前只调用 `Runtime.TokenGetter` 获取当前
token。若服务端返回 `401 token_expired`、`invalid_token` 等认证失败，adapter 可以调用
`Runtime.TokenRefresher` 强制刷新 token，并把同一个请求重放一次。不要在 LLM adapter
里读取 token 文件、拼 OAuth refresh payload，或维护 provider 登录状态。

- `codex-cli` 读取 `~/.codex/auth.json` 或 `CODEX_HOME/auth.json`，按 JWT 过期时间
  刷新 ChatGPT OAuth access token，并保留原有的 `tokens.account_id`。
- Codex 请求走 `llm.ResponsesAdapter`，该 adapter 必须挂上 `TokenGetter` 和
  `TokenRefresher`，避免使用初始化时的旧 token，并能处理服务端提前判定 token 失效的情况。
- Codex 后端要求无状态、仅流式的 Responses 调用：内置 `codex-cli` profile 设置
  `ProviderQuirks.ResponsesStoreFalse=true` 和 `ProviderQuirks.ResponsesRequireStream=true`；
  adapter 必须序列化 `"store": false`，即使调用方要求非流式也必须走流式。不要依赖
  服务端默认值。
- 所有原生工具 adapter 在发送前都必须规范化 tool schema。Chat Completions 与
  Anthropic Messages 对空值或非法的 `required` 直接省略，确保纯可选参数工具不会
  序列化为 `required: null`；Responses-compatible adapter 再应用其更严格的协议
  规则，把空 required 集合序列化为 `[]`。
- 模型管理器会在一项路由选择完成、且其 transport 声明支持原生工具时，自动发起一次
  携带纯可选参数探测工具的有界请求。这样验证的是实际协议 adapter 和端点，而不只是
  配置解析。`doctor --probe-models` 复用同一 schema 探测，并继续合并共享同一 provider
  路由的角色，避免重复消耗额度。
- Responses-compatible adapter 必须在线上规范化工具名：provider 侧名称必须匹配
  `^[a-zA-Z0-9_-]+$`；adapter 内部维护别名表，把返回的 tool call 映射回 SelfMind
  原始内部名称后再分发。
- 对无状态 Responses provider，每次重放 tool result 时，必须在同一请求 input 中
  先包含 assistant 的 `function_call` item，再放对应的 `function_call_output`。
  `store=false` 时服务端无法查找此前的 response item。
- 如果刷新失败或 refresh token 已失效，界面应提示用户运行 `codex login`，不要把
  provider 返回的原始 JSON 直接展示出来。
- `CODEX_ACCESS_TOKEN` 是静态覆盖项，SelfMind 不会刷新它。
- MiniMax OAuth 也按同一契约处理：`TokenGetter` 负责临近过期刷新，
  `TokenRefresher` 负责请求时发现的服务端失效。

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
| `codex-cli` | `codex_responses` | 外部 OAuth（Codex CLI 登录态） | `gpt-5.5` |
| `claude-code` | `anthropic_messages` | 外部 OAuth（Claude Code 登录态） | `claude-3-5-sonnet-20241022` |
| `gemini-cli` | `openai_compatible` | 外部 OAuth（Gemini CLI 登录态） | `gemini-1.5-pro` |
| `qwen-cli` | `openai_compatible` | 外部 OAuth（Qwen CLI 登录态） | `qwen3-coder-plus` |

## OpenRouter 应用归因

内置 `openrouter` profile 声明 `HTTP-Referer`（应用链接）、`X-Title`（应用
名称）和由 `buildinfo.Version` 派生的 `User-Agent`。它们刻意放在 profile 层
而不是 adapter 默认头：profile 若配成其它协议、或走流式调用，都不会经过
OpenRouter adapter 自己的请求构造，adapter 里设置的归因头因此几乎覆盖不到
真实请求。`provider_profiles.openrouter.extra_headers` 仍可逐项覆盖，供 fork
与自建代理使用。

## DeepSeek V4

内置 `deepseek` profile 使用 OpenAI-compatible transport，并启用 DeepSeek 的
思考/工具调用契约。`models.primary.reasoning: high` 会发送
`thinking.type=enabled` 和 `level=high`；`xhigh` 会映射为 provider 的 `max`。
当思考响应调用工具时，adapter 会把 `reasoning_content` 与 assistant tool call
一起保存，并在 tool result 之前原样回放；缺少这段内容会使下一次 provider 请求无效。

DeepSeek 请求还可携带 `user_id`。SelfMind 不会发送原始 person、tenant、channel、
邮箱或平台 ID；`StableProviderUserID` 根据已认证的 tenant/person 派生带版本的
匿名 `sm_...` 值。它跨渠道和 run 稳定、不同用户不同，并且只在 provider profile
声明 `user_identity_field: user_id` 时发送。

模型管理器的自动路由校验与 `doctor --probe-models` 除了验证普通原生工具 schema，
还会验证完整思考工具循环：思考与工具调用、工具结果回放、最终 assistant 答复。

## Kimi Coding Plan

推荐最小配置：

```yaml
models:
  primary:
    provider: "kimi-coding"
    model: "kimi-for-coding"

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
- 禁用 HTTP/2，并把 TLS ALPN 限制为 `http/1.1`，该 endpoint 只有在 HTTP/1.1-only 客户端行为下才能正常访问 `/coding`
- tool schema 使用 `moonshot` 修复规则
- Anthropic-compatible 路径不发送 `thinking`

如果用户显式选择 OpenAI-compatible：

```yaml
provider_profiles:
  kimi-coding:
    api_key: "${KIMI_CODING_API_KEY}"
    base_url: "https://api.kimi.com/coding/v1"
    protocol: "openai_compatible"
```

resolver 会避免重复拼出 `/coding/v1/v1`，并对 OpenAI-compatible 请求发送 `reasoning_effort` 与 `thinking`。

## MiniMax Coding Plan

API key 方式：

```yaml
models:
  primary:
    provider: "minimax"
    model: "MiniMax-M3"

provider_profiles:
  minimax:
    api_key: "${MINIMAX_API_KEY}"
```

中国区 API key：

```yaml
models:
  primary:
    provider: "minimax-cn"
    model: "MiniMax-M3"

provider_profiles:
  minimax-cn:
    api_key: "${MINIMAX_CN_API_KEY}"
```

OAuth 方式：

```sh
selfmind auth login minimax-oauth
selfmind model
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
models:
  primary:
    provider: "example"
    model: "example-coder"
    reasoning: "medium"

provider_profiles:
  example:
    api_key: "${EXAMPLE_API_KEY}"
    base_url: "https://api.example.com/v1"
    protocol: "openai_compatible"
    max_tokens: 32768
    extra_headers:
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

## 传输韧性（Transport Resilience）

对无状态 Responses 后端（codex `store=false`）做流式请求时会遇到瞬时的
`Post .../responses: EOF` 断连。重试/退避层在不改动协议契约的前提下吸收这些错误
（英文规范为准，见 `provider-runtime.md`）：

- Agent 重试循环（`internal/kernel/agent.go`
  `streamChatWithRetry`/`chatResponseWithRetry`）只对**可重试**错误重发，采用指数退避
  `base*2^(attempt-1)` + `[0.9,1.1)` 抖动、封顶、可被 ctx 取消的 sleep。分类逻辑在
  `internal/kernel/llm/retryable.go`（`IsRetryableError`）：EOF /
  `io.ErrUnexpectedEOF` / 连接 reset/refused / `net.Error` 超时 / 5xx / 429 /
  流空闲 属于可重试；上下文超限、配额/用量上限、401/鉴权失败、400 无效请求属于
  **致命**错误，直接失败。新增 provider 错误要可分类——暴露状态码或可识别短语。
- 429 的 `Retry-After` 会被遵守（`RetryAfterFromError`）：响应头经 `foldRetryAfter`
  折叠进错误信息，并解析 codex/OpenAI 的 "try again in N" 正文措辞；上限 600s。
- SSE 空闲看门狗（`responses_adapter.go`）在流长时间无新数据时中止并抛出可重试的
  空闲错误，让循环重连；由配置驱动
  （`SELFMIND_STREAM_IDLE_TIMEOUT` 环境变量 > 配置默认 > 180s），且从不改动
  `store`/`stream` 标志。
- Provider HTTP 调用统一走 `llm.ProviderHTTPClient()`，开启 TCP keepalive
  （`httpclient.go`），让死连接尽快暴露；Kimi 的 HTTP/1.1-only 路径复用同一带
  keepalive 的 transport。
- Provider 请求统一走具备路由感知能力的共享 transport。macOS 会读取当前手动系统
  代理，缓存 30 秒，并在连接类错误后立即刷新。由 launchd 管理的 Gateway 以实时
  系统路由为准，因此从公司直连切换到家里的 Clash，或反向切换，都不需要重启 CLI。
  本地 provider endpoint 和系统排除项继续直连。PAC/WPAD 在正式支持前会返回带解决
  办法的错误；当系统仍要求代理时，SelfMind 绝不会静默绕过代理改为直连。
- Linux 与非 managed 进程使用标准代理环境变量或宿主 TUN/路由。Managed Gateway
  只保存不含凭据且名称精确匹配的 `HTTP_PROXY`、`HTTPS_PROXY`、`ALL_PROXY`、
  `NO_PROXY`（也接受小写形式）；名称相似的非标准变量和带内联凭据的 URL 会被拒绝。
  Go 不读取 `ALL_PROXY`，因此安全的值会补足缺失的 `HTTP_PROXY` 与
  `HTTPS_PROXY`。`selfmind env refresh --restart` 会用重新采样的 login shell 环境
  改写 launchd/systemd 服务定义、验证新 daemon，并提交已验证的 service
  generation，避免所有权状态仍指向旧进程；已开始的 run 保留其冻结环境。
- `/diag` 会在不暴露凭据的前提下显示 provider 当前选择的路由与本地代理可达性，
  并给出具体恢复操作。因网络失败的后台学习任务会保留网络路由指纹；直连/代理或
  本地监听状态改变后，下一个 maintenance sweep 会自动释放任务，显式 managed
  restart 也会给予一次新的尝试。
- **不做游标续传。** `store=false` 时服务端从未持久化响应，无法用
  `previous_response_id` 续传——重试始终是整体重发，不要尝试部分续传。

配置项（`agent:` 段）：`llm_max_retries`(默认 5)、`llm_retry_base`(`300ms`)、
`llm_retry_cap`(`30s`)、`llm_stream_idle_timeout`(`180s`)。留空或 0 = 默认值。

## 新增内置 Provider Checklist

1. 在 `internal/modelruntime/profile.go` 添加 `ProviderProfile`。
2. 选择现有协议族，不要先写新 adapter。
3. 设置 `APIKeyEnvVars`、`BaseURLEnvVar`、`ModelList`、`FallbackModels`。
4. 选择或新增最小必要的 `ProviderQuirks`。
5. 如果是 OAuth，先把 token 读写封装在 `internal/modelruntime`，不要放到 adapter。
6. 不要在 app/gateway/CLI/IM/task strategy 里新增 provider-name 分支；已有协议族应能直接工作。
7. 在 `internal/modelruntime/resolver_test.go` 覆盖默认协议、base URL、fallback 模型、quirks。
8. 如果新增协议族，在 `internal/kernel/llm/transport.go` 注册 transport，并增加 transport 测试。
9. 如果新增 quirks 行为，在 `internal/kernel/llm` 增加 adapter 测试。
10. 如果 CLI picker 需要展示更友好的名称，更新 `internal/cliapp/model_commands.go` 测试。
11. 更新本文档的“已内置的常用模型服务商”表格。

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
