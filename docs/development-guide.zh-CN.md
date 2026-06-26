# SelfMind 中文开发指南

[English Development Guide](development-guide.md) · [中文使用说明](../README.zh-CN.md)

这份文档记录 SelfMind 当前的工程结构，方便后续开发者或下一次 AI 按同一个架构继续迭代。当前版本已经完成个人版到 SaaS 方向的基础演进：单 binary gateway、原生工具调用、学习闭环、动态模型配置和自定义模型 endpoint。

## 产品边界

SelfMind 当前优先做“日常可用的个人 Agent”：

- 用户入口只有 `selfmind`。
- 本地 TUI 用于交互式工作。
- 本地 gateway 用于 24 小时后台执行任务。
- CLI、IM、Webhook 渠道通过 gateway 身份绑定识别到同一个人。
- 渠道聊天记录互相隔离；共享的是 task/run/workspace/memory/skill 状态。
- 个人版从本地 `config.yaml` 读取模型和 provider 策略。
- 未来 SaaS 版应从数据库中的租户、用户、workspace 策略里解析同样的模型配置。

`selfmind` 是唯一 binary，守护进程通过 `selfmind gateway run` 启动。

## 目录结构

```text
cmd/selfmind/              用户可见 binary 入口
internal/cliapp/           顶层 CLI 应用路由
  root.go                  全局 -f/--config 和模式分发
  model_commands.go        selfmind model picker/list/set/current
  gateway_commands.go      selfmind gateway run/start/status/stop/restart
  client_commands.go       send/status/tasks/workspace gateway client 命令
internal/app/              应用组装层
  agent.go                 provider 构造、模型 gateway、复盘引擎
  storage.go               storage 初始化
  tools.go                 工具注册和 middleware
internal/platform/config/  YAML 配置 schema、默认值、兼容、保存
internal/kernel/           agent loop、memory、review、native tool calls
internal/kernel/llm/       provider adapters 和 role-based model gateway
internal/modelruntime/     provider profiles、credential resolution、model catalog/cache
internal/tools/            内置工具和工具 middleware
internal/gateway/          TUI、HTTP API、router、delivery、channel adapters
internal/control/          control.db 身份/workspace/task/run 状态
internal/runtime/gateway/  gateway 进程 runner、pid、lock、state、start/stop
packaging/                 Linux 打包脚本和 systemd 模板
docs/                      架构和开发文档
```

依赖方向要保持清晰：

- `kernel` 不应该依赖 `tools`、`gateway` 或 `server`。
- `kernel.Agent` 只通过 `AgentBackend` 访问工具。
- `app` 负责组装 storage、LLM provider、tools、memory、gateway。
- `cliapp` 负责命令路由和用户可见 CLI 行为。
- `gateway/httpapi` 负责 HTTP handler 和 model-free 控制命令。

## 架构约束

后续开发和 AI 辅助修改必须先阅读：

- [SelfMind 架构约束](architecture-constraints.zh-CN.md)
- [SelfMind Architecture Constraints](architecture-constraints.md)
- [SelfMind 上下文生命周期与 P0-P2 落地](context-lifecycle.zh-CN.md)

重点约束：

- `internal/gateway/cli/controller.go` 只做 TUI 状态编排，不继续承载新的大块 UI 逻辑。
- `/help`、详情页、列表页、搜索页等临时页面使用 `internal/ui/components/Pager` 或同类可复用 surface。
- slash command 的执行、帮助文案和输入提示要逐步收敛到统一 registry。
- 避免新增跨租户、跨测试共享的全局 mutable 状态。
- 新 provider、工具、HTTP handler、TUI 组件应按职责拆分，不把逻辑堆进已有大文件。
- durable context 选择必须走 `gateway/httpapi/context_selector.go` 和 `kernel.TaskRuntimeContext`；不要在无关 handler 里直接拼接 raw task event、artifact 或 memory snippet。

## 配置系统

默认配置路径：

```text
~/.selfmind/config.yaml
```

全局指定配置路径：

```sh
selfmind -f ./config/config.yaml
selfmind --config ./config/config.yaml gateway start
```

环境变量指定配置路径：

```sh
SELF_CONFIG=/etc/selfmind/config.yaml selfmind gateway run
```

SelfMind 不引入 `.env`。YAML 字段支持 `${OPENAI_API_KEY}` 这类环境变量展开。

### 当前 YAML Schema

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

provider_profiles:
  minimax:
    api_key: "${MINIMAX_API_KEY}"
    base_url: "https://api.minimax.io/anthropic"
    protocol: "anthropic_messages"
    model: "MiniMax-M3"
  kimi-coding:
    api_key: "${KIMI_CODING_API_KEY}"
    base_url: "https://api.kimi.com/coding"
    protocol: "anthropic_messages"
    model: "kimi-for-coding"

auth:
  credentials_file: "~/.selfmind/auth.json"

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
```

`internal/platform/config/loader.go` 的兼容规则：

- 旧的 `agent.provider` / `agent.model` 会被读取并规范化到 `model.provider` / `model.default`。
- 旧的 `providers.openai_api_key`、`providers.anthropic_api_key`、`providers.gemini_api_key` 会被读取到嵌套 provider。
- 新保存格式不会再写旧的 flat key。
- `LoadConfig(config.Options{Path: ...})` 支持显式路径。
- `LoadConfig(config.Options{Path: ..., CreateIfMissing: true})` 用于可初始化配置的命令，例如 `selfmind model`。

## 模型 Provider 系统

新增或调整模型服务商前，先阅读 [Provider Runtime 规范](provider-runtime.zh-CN.md)。当前模型接入采用 `ProviderProfile + Resolver + Adapter + ProviderQuirks` 四层边界，Kimi、MiniMax、OpenAI、Anthropic、OpenRouter、Google、DeepSeek、Z.AI、Alibaba Coding Plan 都按这套规则接入。

用户命令：

```sh
selfmind model
selfmind model current
selfmind model list
selfmind model set openai gpt-4o
```

交互流程：

1. 展示供应商列表：OpenAI、Anthropic、Google、已保存 custom endpoint、`Custom endpoint (enter URL manually)`。
2. 输入或保留 API key。
3. 供应商支持时，实时拉取模型列表。
4. 用户选择模型；如果列表不可用，手动输入。
5. 写入 `config.yaml`。

当前协议族：

| Provider | 协议 | Adapter |
|---|---|---|
| OpenAI | Chat Completions / native tools | `llm.OpenAIAdapter` |
| Anthropic | Messages API | `llm.AnthropicAdapter` |
| Google | OpenAI-compatible Gemini endpoint | `llm.GeminiAdapter` |
| Codex CLI | Responses-compatible Codex endpoint | `llm.ResponsesAdapter` |
| Claude Code | Anthropic Messages + external OAuth | `llm.AnthropicAdapter` |
| Gemini CLI | OpenAI-compatible Gemini endpoint + external OAuth | `llm.GeminiAdapter` |
| Qwen CLI | OpenAI-compatible endpoint + external OAuth | `llm.GenericOpenAIAdapter` |
| Provider profile | OpenAI-compatible / Anthropic Messages / Responses | 由 `modelruntime.Runtime.Protocol` 选择 |
| Custom | OpenAI-compatible endpoint | `llm.GenericOpenAIAdapter` |

大多数新厂商如果提供 OpenAI-compatible 或 Anthropic-compatible API，都应该优先通过 `provider_profiles` 或新增 `ProviderProfile` 接入，不要直接在 app、CLI、IM adapter 中硬编码厂商逻辑。只有遇到真正不同的协议族时，才新增 Go adapter。

模型 runtime 边界：

- `internal/modelruntime/profile.go` 管内置 provider 元数据、别名、协议族、环境变量、模型列表方式和 fallback models。
- `internal/modelruntime/resolver.go` 把 YAML、环境变量、SelfMind auth JSON、外部 CLI 登录解析成 `Runtime`。
- `internal/modelruntime/catalog.go` 拉取并缓存模型列表，不负责创建 LLM adapter。
- `internal/app/agent.go` 只把 `Runtime` 转成 adapter 并组装 role routing，不再写厂商级认证探测逻辑。
- `ProviderQuirks` 负责认证头、tool schema、thinking、system message、User-Agent 等厂商差异。
- `internal/cliapp/model_commands.go` 管用户交互式 provider/model picker；`Custom endpoint (enter URL manually)` 保持第 4 项，兼容脚本输入。
- P2 外部认证复用只覆盖 Codex CLI、Claude Code、Gemini CLI、Qwen CLI，以及 SelfMind 自己管理的 OAuth provider（例如 MiniMax OAuth）。`Runtime.TokenGetter` 是每次请求前的 token 来源；`Runtime.TokenRefresher` 是 provider 返回认证失败后，协议 adapter 可以调用一次的强制刷新 hook。其它厂商走 API key、自定义 endpoint 或 `provider_profiles`，不要随意探测随机客户端的本地登录态。

### Role-Based Model Routing

`internal/kernel/llm/model_gateway.go` 是轻量模型路由器。当前 role 名称应保持稳定，后续 SaaS 的模型策略也复用这些名称：

- `coding_agent`
- `memory_extract`
- `background_review`
- `skill_curator`
- `semantic_recall`

`internal/app/agent.go` 构造默认 provider 和 role provider。如果某个 role 构造失败，就回退到默认 provider。

SaaS 演进建议：

1. 定义 model policy store interface。
2. 个人版实现从 YAML 读取。
3. SaaS 实现从数据库读取租户/用户/workspace/provider policy。
4. adapter 接收已经解析好的 provider config，不直接读取全局 YAML。

## Gateway Runtime

用户命令：

```sh
selfmind gateway run
selfmind gateway start
selfmind gateway status
selfmind gateway stop
selfmind gateway restart
```

实现分工：

- `internal/cliapp/gateway_commands.go` 解析用户命令。
- `internal/runtime/gateway/runner.go` 负责初始化、HTTP server、signal、shutdown、runtime status。
- `internal/runtime/gateway/client.go` 负责后台 child process 启动。
- `internal/runtime/gateway/state.go` 管理 pid/state/lock 文件。

运行时文件：

```text
<storage.data_dir>/gateway/
  gateway.pid
  gateway_state.json
  gateway.lock
  gateway.log
```

gateway 配置：

```yaml
gateway:
  addr: "127.0.0.1:8765"
  token: ""
  drain_timeout: "30s"
```

`SELF_GATEWAY_*` 环境变量仍然兼容；旧的 `SELF_DAEMON_*` 也兼容，但 `SELF_GATEWAY_*` 优先。

控制接口：

| 方法 | 路径 | 作用 |
|---|---|---|
| `GET` | `/v1/gateway/status` | 查看进程状态和 active runs |
| `POST` | `/v1/gateway/shutdown` | 请求优雅 draining shutdown |
| `GET` | `/v1/approvals` | 查看 pending 或指定状态的审批请求 |
| `POST` | `/v1/approvals/respond` | 批准或拒绝审批请求 |
| `GET` | `/v1/tasks/events` | 查看当前任务或指定 task 的最近事件 |

停机时 gateway 会进入 draining 状态，拒绝新 run，等待 active run 完成；CLI 可在需要时 force stop。

## 身份、任务、Workspace

`internal/control/store.go` 管理本地控制面数据库。

核心概念：

```text
tenant_id       个人默认租户或 SaaS 租户
person_id       跨渠道同一个人
account_id      平台账号绑定
workspace_id    项目/工作目录
task_id         持久化任务状态
run_id          一次 Agent 执行
event_id        可审计任务事件
```

运行时状态规则：

- `task_runs.heartbeat_at` 由 gateway 每 10 秒刷新，异常重启后 `MarkInterruptedRuns` 会把遗留 running run 标记为 `interrupted`。
- `task_events` 记录 `run.started`、`tool.started`、`tool.completed`、`learning.review`、`learning.memory.saved`、`learning.skill.updated`、`approval.approved`、`approval.rejected`、`run.finished`、`run.cancelled` 等事件。
- `approval_requests` 记录持久化审批请求；gateway 控制命令和 HTTP handler 应读写这张表，不要把审批状态放进 IM adapter。
- `outbound_messages` 是 IM 回发队列。没有 sender 时保持 `pending`；配置 sender 后由 delivery worker 重试发送。
- `/status` 面向摘要，`/events` 面向最近运行事件。

触碰文件、搜索、补丁、终端或进程的工具必须遵守 `allowed_roots`，不要绕过 `WorkspaceScopeMiddleware`。

## 工具调用

SelfMind 当前采用 Hermes 风格的 native tool-call contract：

- `ChatRequest.Tools` 会发送给 OpenAI-compatible provider。
- OpenAI-compatible streaming response 会累积 `delta.tool_calls`。
- assistant tool calls 存在 `Message.ToolCalls`。
- tool result message 使用 `Message.ToolCallID` 和 `Message.Name`。
- 如果 provider 拒绝 native tools，OpenAI-compatible adapter 会重试无工具请求，Agent 再回退到旧的 `[TOOL:name:{...}]` 格式。
- 只有明确只读的工具批次会并行；修改型、终端、记忆、Skill、委托、进程控制、补丁工具顺序执行。

关键文件：

- `internal/kernel/native_tool_call.go`
- `internal/kernel/agent.go`
- `internal/kernel/llm/adapters.go`
- `internal/tools/dispatcher.go`
- `internal/tools/guardrails.go`
- `internal/tools/security.go`

工具稳定性规则：

- `ToolGuardrails` 按 run 维度阻止同一工具/同一参数的连续重复失败。
- 对只读工具会检测“相同参数得到相同结果”的无进展循环。
- 工具日志、gateway delivery 错误和事件 payload 应通过 `tools.RedactSensitive` 脱敏。
- mutating、terminal、memory、skill、delegation、process-control 和 patch 工具必须保持顺序执行，不要加入并行白名单。

### 工具运行时隔离

新的应用路径应创建独立 registry：

```go
registry := tools.NewRegistry()
dispatcher := tools.NewDispatcherWithRegistry(registry)
```

规则：

- `tools.NewDispatcher()` 和 `tools.GlobalRegistry()` 只作为旧兼容入口保留。
- 租户、workspace、MCP、动态加载的 skill 工具都应通过当前 dispatcher/registry 注册。
- `MCPToolManager` 连接和断开工具时必须操作自己的 dispatcher，不要操作全局 registry。
- `Registry.Dispatch` 和 `Dispatcher.Dispatch` 都会先做参数类型转换和校验，再执行工具。
- 参数转换是严格的：非法 integer、number、boolean 会直接报错。
- `ClarifyFn` 仍作为 TUI 兼容 fallback 保留，新审批/clarify 接入应优先通过 dispatcher registry 注入 handler。

### Agent 并发

一个 `Agent` 实例目前用 `runMu` 串行执行，因为它只拥有一个 `EventChannel`。如果 gateway 后续需要真正并行的 active runs，应为每个 run 创建独立 agent，或先引入 per-run event sink，再移除这个锁。

`syncTurn` 使用有界后台队列，不再每轮 assistant 响应都创建无上限 goroutine。高频对话下宁愿丢弃旧的 sync 快照，也不要堆积大量 memory-sync worker。

## “越用越智能”学习闭环

目标闭环：

```text
conversation/task
  -> memory 和 session 持久化
  -> skill 使用和 metrics
  -> final response
  -> background review
  -> memory save / skill create / skill patch / skip
  -> curator 生命周期治理
```

关键文件：

- `internal/kernel/fact_extractor.go`
- `internal/kernel/turn_extractor.go`
- `internal/kernel/reflection.go`
- `internal/kernel/background_review.go`
- `internal/tools/learning_audit.go`
- `internal/tools/skill_manage.go`
- `internal/tools/skill_runtime.go`
- `internal/tools/skill_bundles.go`
- `internal/tools/skill_catalog.go`
- `internal/tools/skill_curator.go`
- `internal/tools/session_search.go`
- `internal/kernel/memory/sqlite_provider.go`

需要保持的规则：

- 稳定的用户偏好和项目事实进入 memory。
- 可复用流程、步骤、检查清单进入 skills。
- 临时任务进度不要写入长期 memory。
- 使用 skill 后发现过时或被用户纠正，优先 patch 原 skill，避免重复创建。
- 临时 provider outage、一次性 tool failure、猜测性的失败原因不要沉淀为 memory/skill。
- 创建 skill 时默认使用 `source=agent-created`，session 细节放到 `references/`，不要写进主 `SKILL.md`。
- Curator 默认只治理 agent-created skills，除非用户明确要求。
- Skill 发现必须保持渐进加载：`skills_list` 只返回紧凑元数据，`skill_view` 才加载完整 `SKILL.md` 或 linked file，`skill_manage` 只负责修改。
- Skill 修改应尽量热加载当前 registry；`create`、`install`、`archive`、`restore` 后不要要求用户重启。
- Skill slash command 先解析 bundle，再解析单个 skill。Bundle 存放在 `~/.selfmind/<tenant>/skill-bundles/`。
- Catalog 安装的 skill 标记为 `catalog-installed`，并记录到 `~/.selfmind/<tenant>/skills/.catalog/lock.json`；curator 不应自动治理，除非显式标记为 `agent-created`。
- Catalog 安装默认拒绝同名目录和旧版 `.md` 冲突；只有用户显式传入 `--force` 才能替换，且替换前必须先把旧副本备份到 `skills/.catalog/backups/`。
- Patch 操作应支持 fuzzy matching，并在失败时返回可行动的上下文，而不是只有 `not found`。
- Memory 和 Skill 修改必须写入 `~/.selfmind/<tenant>/learning/` 下的租户级学习审计记录。
- `skill_manage(action=history|undo, ...)` 和 `memory(action=history|undo, ...)` 是用户可见的审计和撤销入口；不要在 TUI 或 IM 中绕过工具层实现私有 history/undo。

Skill 相关入口：

```text
skill_catalog   -> official/local/url install and audit
skills_list     -> compact metadata only
skill_view      -> SKILL.md or linked file content
skill_bundle    -> bundle CRUD
skill_manage    -> skill mutation and hot reload
/skill-name     -> user-facing direct invocation
/bundle-name    -> user-facing multi-skill invocation
```

## 新增工具

1. 实现 `tools.Tool`。
2. 在 `internal/app/tools.go` 注册。
3. 需要时增加 schema 校验和危险内容扫描。
4. 触碰文件或 shell 时，确保 workspace scope 生效。
5. 添加 dispatcher/tool 测试。

工具参数应尽量使用结构化 JSON schema，避免能用结构化 API 时还做字符串解析。

## 新增模型 Provider

先判断是否真的需要改代码：

- 一次性 OpenAI-compatible 厂商：使用 `selfmind model` 自定义 endpoint。
- 可复用且只有 base URL、协议、模型名变化的厂商：优先新增或文档化 `provider_profiles.<id>` YAML 配置。
- 内置厂商元数据、认证环境变量、实时模型列表：更新 `internal/modelruntime/profile.go` 和 `catalog.go`。
- 新协议族：新增 Go adapter。

新增 adapter 流程：

1. 实现 `llm.Provider`。
2. 支持 `Chat`、`StreamChat`、`ChatCompletion`。
3. provider 支持工具调用时，保留 native tool-call 行为。
4. 在 `internal/modelruntime` 增加 runtime/profile 元数据和认证解析测试。
5. 在 `internal/app/agent.go` 只接入协议到 adapter 的构造逻辑。
6. 只有 `ProviderEndpoint` / `provider_profiles` 不够时，才扩展 config schema。
7. 添加 streaming、错误处理、native tools、认证解析、model catalog、role routing 测试。

## 新增 IM 平台

保持 Hermes 风格边界：

```text
platform adapter
  验证签名
  解析平台 payload
  归一化 inbound message
  实现平台 sender（可选）
        |
        v
gateway
  身份绑定
  workspace/task/run 状态
  agent dispatch
  delivery enqueue/retry/split
```

gateway 拥有 person identity、task state、workspace binding、active run policy、memory、skills 和 outbound 队列。平台 adapter 不应拥有这些概念。

通用 webhook 入口：

```text
POST /v1/im/{platform}
```

生产平台 adapter 需要按这个顺序补：

1. 入站签名/解密/去重。
2. payload 归一化为 `api.MessageRequest`。
3. sender 实现 `delivery.Sender`，注册到 `delivery.Router`。
4. 复用 delivery 的长消息拆分、重试和 pending 队列。
5. 平台支持时再接入原生审批按钮和富媒体附件。

## 新增 Gateway 命令

1. 优先在 `internal/gateway/httpapi/server.go` 增加 model-free pre-agent command。
2. 用户常用命令在 `internal/cliapp` 增加 CLI wrapper。
3. request/response DTO 放到 `internal/gateway/api`。
4. 添加 HTTP 和 CLI 测试。
5. 更新 README 和本开发指南。

## 测试

全量测试：

```sh
GOWORK=off go test ./...
```

常用检查：

```sh
go test ./internal/platform/config ./internal/cliapp ./internal/runtime/gateway
go build ./cmd/selfmind
git diff --check
```

手工 smoke test：

```sh
selfmind -f ./tmp/config.yaml model set openai gpt-test
selfmind -f ./tmp/config.yaml model current
selfmind -f ./tmp/config.yaml gateway run
selfmind -f ./tmp/config.yaml gateway status
selfmind -f ./tmp/config.yaml gateway stop
```

## Release

发布 workflow：

```text
.github/workflows/release.yml
```

当前只构建 Linux：

- `linux-amd64`
- `linux-arm64`

支持 tag 自动发布和手动 workflow dispatch。

Linux 安装脚本会创建 `/etc/selfmind/config.yaml`，systemd 服务使用下面的命令启动：

```sh
/usr/local/bin/selfmind -f /etc/selfmind/config.yaml gateway run
```

## SaaS 演进方向

不要把个人版和 SaaS 版拆成两套架构。保持单 binary 多模式：

```text
selfmind
selfmind gateway ...
selfmind server ...
selfmind worker ...
selfmind migrate ...
```

推荐演进：

1. 增加 DB-backed model policy 和 secret store interface。
2. 保留 YAML store 作为个人版实现。
3. 增加 durable worker queue 执行长任务。
4. 增加 IM outbound sender 和 approval callback。
5. 增加租户隔离、data dir 隔离、rate limit、model budget。
6. 增加 Web admin 和 observability，但保持 gateway/task/run contract 稳定。
