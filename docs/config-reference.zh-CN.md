# SelfMind 配置说明

所有配置都在一个 YAML 文件里：`~/.selfmind/config.yaml`。本文逐段说明字段、
默认值和"什么时候需要改"。英文版为准：`docs/config-reference.md`。

## 快速须知

- **位置**：`~/.selfmind/config.yaml`（可用 `--config <路径>` 覆盖）。
- **首次运行**会用默认模板创建；缺失的段落自动回退到内置默认值，所以你只需
  写你要改的部分。
- **改完必须重启才生效。** daemon 只在启动时读一次配置。改完执行
  `selfmind gateway restart`。（常见坑：改了字段忘了重启，运行中的
  daemon 还在用旧值。）
- **密钥**：通过 Model Manager 输入 provider API key。SelfMind 会把它保存到
  私有 auth store，而不是生成的 YAML。环境变量引用和明文 key 只为兼容旧配置
  继续读取；运行 `selfmind config upgrade` 可迁移旧文件。
- **时间**用 Go 时长字符串：`"300ms"`、`"30s"`、`"5m"`、`"24h"`。

---

## 1. 模型与 provider（必填）

唯一必须配的东西：谁来回答你。

```yaml
models:
  primary:
    provider: "codex-cli"    # 内置 provider 或 providers.custom 的 map key
    model: "gpt-5.6-sol"
    reasoning: "xhigh"       # 可选；省略或 auto 表示模型默认值
  auxiliary:
    enabled: true
    provider: "deepseek"
    model: "deepseek-v4-flash"

providers:
  deepseek:                  # 可选：覆盖内置 provider 的 endpoint
    base_url: "https://gateway.example.com/v1"
  custom:
    company-gateway:         # 此 map key 就是 provider id
      base_url: "https://ai.example.com/v1"
      protocol: "openai-compatible"
      auth: "bearer"
```

- 自定义连接的 `protocol` 可用 `openai-compatible`、
  `anthropic-compatible`、`responses-compatible`；`auth` 可用 `bearer`、
  `x-api-key`、`none`。
- **复用外部 CLI 登录**：Codex CLI、Claude Code、Gemini CLI、Qwen CLI 的凭据
  可直接复用，无需 API key。
- 内置 provider 只有覆盖 endpoint 或非密钥 wire 参数时才需要 YAML 块。用户
  自定义连接只放在 `providers.custom.<id>`，模型路由直接引用 `<id>`。
- provider 配置只管理连接和认证；模型选择统一写在 `models.primary`。
  endpoint 中旧的 `model` 字段仅为兼容历史配置继续读取。
- Model Manager 输入的凭据存放在 `auth.credentials_file`。包含凭据的
  `extra_headers`、`extra_body`、`extra_query` 会被拒绝。
- `provider_profiles` 和带 `custom:` 前缀的 provider id 只作为兼容读入。
  `selfmind config upgrade` 会先备份再重写。

### 通用 Provider 请求扩展

SelfMind 采用与 OpenAI Python SDK 一致的三个名称：`extra_headers`、
`extra_body`、`extra_query`。内置 provider 的通用参数放在
`providers.<id>`，自定义连接放在 `providers.custom.<id>`；只有某个后台角色
需要不同参数时，才放在
`models.roles.<role>`。它们统一适用于 OpenAI Chat/Compatible、Anthropic
Messages 和 Responses 协议。可对照
[OpenAI Python 请求扩展说明](https://github.com/openai/openai-python#undocumented-request-params)
与 [DeepSeek 用户隔离说明](https://api-docs.deepseek.com/zh-cn/quick_start/rate_limit)。

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

- `extra_body` 会递归合并到标准请求体之后，因此显式字段可以覆盖自动派生的
  可选字段。例如配置 `extra_body.user_id` 后，它会覆盖 SelfMind 自动生成的
  DeepSeek 匿名 `user_id`；不配置则继续使用匿名派生值。
- `extra_query` 保留 URL 原有查询参数，只覆盖自己声明的键；列表会编码为同名
  多值参数。
- 三个映射中的字符串都支持 `${ENV_VAR}` 展开，但密钥和凭据形态的 key 必须走
  私有 auth store。
- DeepSeek 将 `user_id` 定义为同一账号下用于调度与隔离的标识，并非凭证；但仍
  建议使用稳定、无业务含义的值，不要写姓名、邮箱、手机号、提示词等隐私信息。
- `selfmind model` 会显示有效路由；每完成一个选择都会自动执行有界探测，且不会
  回显 body/query 的值。
- 旧 `headers` 继续兼容；新配置和文档统一使用 `extra_headers`，同名键由新字段
  覆盖。

有类型的传输兼容规则放在 `quirks`，任意厂商请求参数放在 `extra_*`：

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
        prompt_cache: false       # 显式 false 可以覆盖内置 true
```

布尔 quirk 省略时继承内置 profile；只有 endpoint 契约不同时才显式写 `true` 或
`false`。匿名身份可用 `auto`、`user_id`、`metadata.user_id`、`off`；HTTP 版本可用
`auto`、`http1`、`http2`。`system_message_mode` 已废弃并被忽略。
Model Manager 会自动校验最终契约，并把警告保留在草稿中。

## 2. 模型路由

后台任务使用一个明确的共享路由；它可以与 Main 指向同一物理模型，也可以选择
更便宜/更快的模型处理有界维护工作。

```yaml
models:
  source: "local"
  primary:
    provider: "codex-cli"
    model: "gpt-5.6-sol"
    reasoning: "xhigh"       # 可选
  auxiliary:                  # 后台工作的统一默认模型
    provider: "kimi-coding"
    model: "kimi-for-coding"
  roles:
    # 可选的高级例外；没有列出的后台角色使用 auxiliary。
    fast_classifier: { model: "kimi-for-coding-fast" }
```

`models.primary` 只负责主对话和前台执行。`models.auxiliary` 是
`fast_classifier`、`memory_extract`、`background_review`、`skill_curator`、
`semantic_recall` 和 `summarizer` 的统一默认模型。`models.roles.<role>`
是可选的高级覆盖，优先级最高；只覆盖 `model` 等局部字段时，其余字段继承
`models.auxiliary`。

本地安装省略 auxiliary 的 provider/model 时，会有意默认使用 primary 的选择。
初始化和第一次接受 Model Manager 变更会把这两个槽位明确写入 YAML。一旦
auxiliary 已经落盘或被用户自定义，之后修改 primary 不会覆盖它。这样初始化只需
呈现两个模型槽位，无需把内部角色目录暴露给新用户。

`reasoning` 和 `service_tier`
都可省略；省略或写 `auto` 时使用 provider/模型默认值，不强制向接口发送。
存在能力元数据时，Model Manager 会自动校验完成的选择并显示探测到的默认值。

模型切换不需要额外 YAML 字段。SelfMind 会在本文件同目录的
`model-state.json` 中保存不含密钥的事务 generation、pending 变更、上一次运行
快照、探测摘要、运行快照验证时间和有界历史。该文件是“模型就绪”的唯一权威，
onboarding 不会复制其中的路由；不要直接编辑该状态文件。直接修改 `models.primary`
或 `models.auxiliary` 后，只会被视为 configured、尚未验证，直到 daemon 启动并
完成探测。正常情况下请使用经过校验的
`selfmind model` 路径。

`kimi-coding` 的全部角色都使用供应商默认的 Anthropic Messages 传输
（`https://api.kimi.com/coding/v1/messages`），与 Hermes 和 Kimi Coding
Plan `/coding` 路径的实际协议一致。角色级 `protocol` 只用于自定义网关
或协议不同的特殊部署；正常使用 Kimi Coding Plan 时应省略。

不需要也不存在 `default` 角色。`vision` 等能力特定角色不会自动继承辅助模型，
需要时应显式配置。旧配置继续逐项列出后台角色也完全兼容。

smart 审批比普通角色继承更严格：它使用显式配置或由 `models.auxiliary` 继承的
`fast_classifier`；旧配置可以
兼容回退到显式配置的 `background_review`，但审批裁决不会额外添加一个未声明的
primary 回落位置。有效 auxiliary 默认可以有意与 primary 指向同一个
provider/model。没有可用路由及时响应时，操作会升级为人工确认。

## 3. 存储与认证

```yaml
storage:
  type: "sqlite"
  data_dir: "~/.selfmind/data"   # 会话、记忆、任务、运行状态（SQLite）
auth:
  credentials_file: "~/.selfmind/auth.json"   # 可选；把密钥挪出 YAML
```

## 4. 联网搜索

本地爬 DuckDuckGo 不可靠（反爬 202、GFW），强烈建议配一个托管搜索 API。
**只选一个后端，填它的 key。**

```yaml
web:
  search_backend: "tavily"   # tavily | brave | serper | firecrawl | searxng | duckduckgo
  api_key: "tvly-xxxx"       # 所选后端的 key；searxng 时填实例 URL
```

- 推荐 **Tavily**（https://tavily.com，为 AI 设计、有免费额度、中文覆盖好）。
  备选：Brave（https://brave.com/search/api/）、Serper（https://serper.dev，
  Google 结果）。
- 后端和 key 都留空 = 尽力而为的 DuckDuckGo（常被拦，只在拿不到 key 时用）。
- 单后端、无 fallback 链：配的后端失败会返回错误（让模型如实报告故障），而不是
  偷偷换引擎。

### 执行沙箱与网络

Linux 上的 `terminal`、`verify` 和 `execute_code` 默认优先使用 Bubblewrap：
宿主根目录只读，工作区可写。默认共享 daemon 的网络命名空间，因此可继续使用
daemon 继承到的代理和 DNS 配置。

```yaml
exec_sandbox:
  enabled: true
  required: false
  allow_network: true   # false 表示创建无网络命名空间
```

每个执行工具也可指定 `sandbox: auto|isolated|host`。`isolated` 在隔离能力不可用
时直接失败；`host` 会经过审批，并在 `required: true` 时禁用。
`selfmind gateway restart` 会合并当前终端和旧 daemon 的 `PATH`：当前目录优先，
旧 daemon 独有的工具目录追加在后，因此 IDE 或更新器触发重启时不会隐藏
`~/.local/bin`、Cloud SDK 等用户安装命令。同时会保留旧 daemon 的
`HTTP_PROXY`、`HTTPS_PROXY`、`ALL_PROXY`、`NO_PROXY` 及其小写形式；当前终端
显式设置的代理值始终优先。其他环境变量（包括凭据）不会复制。Linux 可通过
`apt install bubblewrap` 安装 Bubblewrap。

## 5. 记忆与治理

```yaml
memory:
  semantic_recall: true          # 每轮附带相关的历史工作片段
  use_memory_fence: true         # 把召回记忆包装成不可信的背景数据
  governance:
    enabled: true
    mode: "shadow"               # shadow（只报告）→ merge-only → full（加上限）
    consolidation_interval: "24h"
    consolidation_batch_size: 8
    auto_merge_confidence: 0.95      # MERGE 门槛
    auto_reinforce_confidence: 0.90  # REINFORCE 门槛（落库成员原文）
    auto_archive_confidence: 0.90    # ARCHIVE 门槛（可逆）
    max_active_global: 120       # active 记忆上限（仅 full 模式生效）
    max_active_per_workspace: 200
    archive_after: "4320h"       # 180 天；按龄归档（仅 full 模式）
    pause_while_run_active: true # 前台 run 永远优先
```

记忆整理始终使用稳定的 `memory_extract` 语义角色。如需为它指定专用模型，只在
`models.roles.memory_extract` 中配置；没有该覆盖时使用 `models.auxiliary`。
memory 的行为配置不再选择模型角色。

- `mode`：`shadow` 什么都不写（只报告"会做什么"）；`merge-only` 应用有门槛的
  MERGE/REINFORCE/ARCHIVE；`full` 再加 active 上限 + 按龄归档。**在把
  `max_active_global` 按真实稳态重定之前，不要切到 `full`**——上限过低对大库
  会一次性群归档。
- pinned 和用户确认的记忆对所有自动修改免疫。
- 保持 `shadow`，先观察 `~/.selfmind/reports/memory-consolidation/` 下的
  Markdown 校准报告；真实历史上的判决足够准确后，再主动切到
  `merge-only`。`full` 的容量上限与按年龄归档应最后启用。
- 完整逻辑见 `docs/memory-governance.zh-CN.md`。

## 6. Gateway 与 IM 渠道

常驻 daemon。CLI、IM、cron、HTTP 全部汇聚到它。

```yaml
gateway:
  addr: "127.0.0.1:8765"       # 绑定地址；公网绑定必须配 token
  token: ""                    # 共享密钥；非 loopback 绑定时强制
  automatic_run_recovery: true # false 停止 daemon 自动创建子 run；显式 /resume 仍可用
  presence_idle_timeout: "0"   # 已弃用的兼容配置；presence 只跟随客户端进程存活
  pending_notify_after: "15m"  # CLI 已附着时超过 T1 才补推；CLI 脱离后立即推送
  outbound_retention: "336h"   # 终态投递记录保留 14 天；0 表示禁用清理
  delivery_max_message_chars: 3500
  delivery_retry_attempts: 3
```

`automatic_run_recovery: false` 是 daemon/provider 中断自动续跑的 fail-closed
运维回滚开关。它不会丢弃持久计划、effect 证据或 recovery handoff，也不会改变
历史数据语义。用户可先通过 `/task` 审查，再使用精确的 `/resume <run_id>` 路径。

每个 IM 平台是 `gateway` 下的子段，默认全部关闭：

```yaml
  weixin:                      # 个人/企业微信（iLink），主要的微信通道
    enabled: true
    owner_person_id: "person_xxx"   # 被绑定为你的发送者（见安全提示）
    account_id: "xxx@im.bot"
    token: "xxx@im.bot:xxxx"
    dm_policy: "allowlist"     # open | allowlist | disabled（见安全提示）
    allow_from: ["openid@im.wechat"]
    group_policy: "disabled"
  telegram_token: ""           # Telegram bot token
  feishu: { enabled: false, app_id: "", app_secret: "", verification_token: "", encrypt_key: "" }
  qq:     { enabled: false, app_id: "", secret: "", token: "" }
  wechat: { enabled: false, app_id: "", app_secret: "", token: "" }   # 公众号
```

> **安全 —— 微信 DM 策略。** 配了 `owner_person_id` 后，DM 策略放行的每个发送者
> 都会被绑定为**你本人**（你的任务、记忆、workspace）。默认 `dm_policy: open`
> 因此会让任何私聊这个号的陌生人变成你。**对外启用前请把 `dm_policy` 设为
> `allowlist`，并在 `allow_from` 里填你自己的 openid。**内置的
> `selfmind weixin login` 会自动为扫码微信用户完成该配置，并绑定到当前 CLI
> person；手工编写 YAML 时也必须守住同一条安全约束。

## 7. 任务

```yaml
tasks:
  inbox_enabled: true
  default_list_limit: 10
  auto_archive_done_after: "720h"      # 30 天；"0" 关闭该归档类
  auto_archive_cancelled_after: "168h" # 7 天
  maintenance_debounce: "5m"       # 安静窗口后再做语义治理
  maintenance_max_wait: "15m"      # 持续有新 run 时也不会无限等待
  maintenance_batch_max_runs: 10   # 单次模型调用最多处理的 run 数
  maintenance_quota_probe_initial: "15m" # quota 403 后首次探测间隔
  maintenance_quota_probe_max: "4h"      # 指数退避探测的最长间隔
  maintenance_soft_probe_initial: "15m"  # 200/空响应且输出耗尽后的首次探测间隔
  maintenance_soft_probe_max: "1h"       # 软故障指数退避的最长间隔
  maintenance_llm_timeout: "2m"    # 单次分析器模型调用的超时上限；过紧会导致 deadline 重试耗尽并丢学习
```

自动归档只碰陈旧、终态、未 pin、无活跃 run 的任务。

run 后合并执行的任务标签与记忆提取始终使用稳定的 `memory_extract` 角色。
高级模型选择只放在 `models.roles.memory_extract`；tasks 只控制行为和调度。

后台维护走两级链路：先用角色自身的配置，再回落到 `models.auxiliary` 这个
共享地板。除了把 `models.auxiliary` 指向一个你信得过的 provider 之外，
无需为故障切换配置任何东西。该链路绝不会回退到主编码模型。
auxiliary 可以有意与 primary 指向同一个物理模型；链路仍不会额外添加一个
隐式 primary 位置。

两级都会按物理路由去重，所以没有自身 override 的角色会解析到
`models.auxiliary`，最终诚实地得到一条单级链路。要获得真正的故障切换，
需要把角色 override 配到与 `models.auxiliary` 不同的 provider 上：

```yaml
models:
  auxiliary:
    provider: "minimax"      # 所有后台角色的回落地板
    model: "MiniMax-M3"
  roles:
    memory_extract:
      provider: "kimi-coding"
      model: "kimi-for-coding"
```

`selfmind model` 中的 `memory_extract` 行会显示解析出的回落目标；当两级
共享同一 endpoint 和凭据时，它会明确说明没有可用回落。

`memory.governance.model_role`、`tasks.maintenance_model_role` 和
`tasks.maintenance_fallback_roles` 均已废弃。模型选择现在只属于 `models`；旧的
fallback 列表曾用于在 auxiliary 地板之前插入额外角色槽位。
`selfmind config doctor` 会识别这些键，`selfmind config upgrade` 会先备份再移除。

run 终态事务会立即保存可重放证据。三个批处理参数只延迟可逆的任务标签和
长期记忆治理，不延迟最终回答，也不影响最近对话连续性。批次绝不跨 tenant、
person 或 workspace。空值或零时长使用产品默认值。

只要维护角色解析到相同 endpoint 和凭据，即使角色名或模型名不同，也会共享
同一个持久化 quota 熔断器。首次 quota 403 会暂停该路由上的积压作业，但不会
消耗它们的重试次数。SelfMind 在 `maintenance_quota_probe_initial` 后只放行一个
半开探测；若仍是 quota 错误，则指数退避到 `maintenance_quota_probe_max`。
探测成功后会自动关闭熔断并重放暂停作业。`/diag` 会显示被阻塞的路由和下次
探测时间。如果配额耗尽期间也必须继续治理，请把 fallback 角色配置到不同的
provider 或凭据。

如果接口返回 HTTP 200，但没有可用文本，同时结束原因为 `max_tokens`
等输出耗尽状态，SelfMind 会使用 `maintenance_soft_probe_initial` 和
`maintenance_soft_probe_max` 打开较短的软熔断。这样既不会让每个积压批次
反复发送同一份维护提示词，又能比确认配额耗尽更早恢复。Provider/网络错误
不会再触发递归拆分批次；只有多 run 返回结构损坏时才允许二分重试。
`/diag models` 会按维护路由和模型角色显示调用、失败、熔断跳过和 token 用量。

## 8. 诊断：飞行记录器

把真实对话录到本地，方便把坏 turn 变成回归测试（见 README "诊断与回归固化"）。

```yaml
flight_recorder:
  enabled: true
  keep: 30            # 保留最新 N 轮，自动清理更旧的；纯本地，不上传
```

## 9. 定时任务与委派

```yaml
cron:
  enabled: true       # 定时作业（日报、存活探针）
delegation:           # 多智能体子任务；留空则用默认模型策略
  provider: ""
  model: ""
  api_key: ""
  max_retries: 3
  max_iterations: 50
```

## 10. 意图与工作续接（高级）

```yaml
intent:
  mode: "hybrid"       # 显式命令/续接线索的匹配方式
  thresholds:
    direct: 0.8
    ask: 0.55
continuity:
  mode: "safe"         # shadow | safe | full | off
```

普通语言永远会到达 agent；这些旋钮只调显式命令和续接检测。大多数用户用默认即可。

`continuity.mode` 控制 `fast_classifier` 对历史 run 卡片的有界判断。`shadow`
只记录判断而保留旧的确定性路径；`safe`（默认）可直接执行只读进度查询、新工作
判断和对当前活动 run 的明确指导，但恢复历史 run 前仍要求用户选择；`full` 还可
执行明确的历史恢复；`off` 关闭这次模型调用。无论哪种模式，显式 ID、回复元数据、
`/resume`、`/choose`、`/new --run` 和独立的“继续”控制都保持确定性。该调用使用
`models.roles.fast_classifier` 覆盖或 `models.auxiliary`，关闭 thinking，并共享
六秒总时限。

## 11. MCP 服务器

```yaml
mcp:
  servers:
    - name: "my-tools"
      transport: "stdio"        # stdio | http | streamable_http
      command: "my-mcp-server"  # stdio 用
      args: []
      url: ""                   # Streamable HTTP 端点
      headers: {}                # 可选 HTTP 请求头
      auth: {}                   # 可选 bearer 或 user/pass 静态认证
      env_filter: []             # 可选 stdio 环境变量允许列表
```

默认为空。daemon 启动时通过官方 MCP Go SDK 连接服务器、协商兼容协议版本、
读取分页工具目录，并把实时工具列表变更同步到 Dispatcher。无法完成初始化或
工具发现的服务器会保留为 gateway 健康问题，并显示在 `selfmind doctor` 中。
每个服务器都应配置唯一且稳定的 `name`；省略时会按 endpoint 生成确定性的兼容名
并打印启动警告，重名则直接拒绝，不会静默覆盖已有服务器。

MCP 工具默认属于未分类外部工具。每次调用都需要一次性人工批准，即使
`full-auto` 也一样；在有经过评审的逐工具信任策略之前，不能复用 grant 或 smart
裁决。SelfMind 只向远端发送工具的公开输入字段，并剥离 tenant/person/run scope、
回调等所有 daemon 内部的顶层下划线字段。远程服务器目前支持静态请求头、Bearer Token
和 Basic Auth；SelfMind 尚未提供交互式 MCP OAuth 流程。

## 12. Agent 调优（高级）

```yaml
agent:
  max_iterations: 90            # 每轮工具循环最大步数
  action_tool_budget: 12        # 使用工具的回合初始 action 预算
  action_tool_budget_step: 6    # 有新证据时单次扩展量
  action_tool_budget_limit: 64  # action 工具硬上限
  max_budget_extensions: 9      # 最多扩展次数
  max_retries: 3
  log_level: "INFO"             # DEBUG | INFO | WARN | ERROR
  llm_max_retries: 5            # 传输层重试次数
  llm_retry_base: "300ms"       # 退避基数
  llm_retry_cap: "30s"          # 退避上限
  llm_stream_idle_timeout: "180s"  # SSE 流卡住多久后中断
  approval_triage_timeout: "30s"   # smart 模式廉价裁决模型的前台预算
  approval_wait: "30m"             # 有实时/健康端点可回答时占用 run 的等待预算
  approval_wait_unattended: "30s"  # 当前没有端点可回答时占用 run 的等待预算
editor:
  large_paste_chars: 1000       # TUI 大段粘贴识别
  large_paste_lines: 10
tui:
  theme: "auto"                 # auto | dark | light | mono
history:
  persistence: "save-all"      # save-all | none
  max_bytes: 524288
  load_entries: 200
evolution:
  enabled: true                 # 技能审查 + 确定性工作流画像
  mode: "observe"               # observe | shadow | auto-readonly（旧模式也只观测）
  min_complexity_threshold: 5
  nudge_interval: 10
  shadow_after_observations: 3
  promote_after_observations: 5
  min_shadow_runs: 3
  max_shadow_failure_rate: 0.05
```

工具预算对所有语言和任务类型统一生效，不使用关键词分类；只有持续产生
新证据才会扩展，简单回答也不会因此被强制调用工具。默认值已按常规使用
调好；只在排查传输抖动或调工具循环时才动。

TUI 不会绘制全屏背景。`auto` 跟随终端能力，`dark` / `light` 选择固定对比度，
`mono` 去掉颜色但保留结构和强调。`Enter` 提交，`Ctrl+J` 插入可靠换行，
`Ctrl+V` 从 GUI 剪贴板附加图片；删除完整 `[Image #N · name]` token 就会移除
附件。输入历史按用户隔离；`history.persistence: none` 只关闭磁盘写入，不影响
当前会话内回调。

`approval_triage_timeout` 与主模型的传输超时相互独立。辅助或显式配置的
`fast_classifier` 如果未在该预算内返回，smart 模式会安全降级为人工审批。
默认值是 30 秒；设置过短会把可用的推理型廉价模型误判成不可用。

审批等待值是资源预算，不是回答失效时间。存在实时进程时使用
`approval_wait`；没有实时进程，且没有可路由的 IM 账号，或首选 IM 最近状态为
`pending_session`、`failed`、`sent_unconfirmed` 时，使用
`approval_wait_unattended`。短预算结束后 run 会释放执行槽并进入 parked，但审批
仍可在七天内回答。因而 Weixin 故障期任务默认可能约 30 秒就 park，这是预期的
恢复行为，不代表审批被拒绝。调用方自身剩余的 deadline 还可能进一步缩短这两个
配置值。

自进化画像是由已完成 run 的持久事件确定性派生，不会增加前台模型调用。
默认的 `observe` 只记录画像，不启用批处理建议。`shadow`、
`auto-readonly` 和 `auto` 别名仍为配置兼容而保留，但普通已完成 run 现在只会
增加观测次数：不会增加 shadow 命中、复活 degraded 候选或授权运行时建议。
`batch_read` 建议必须具有单独验证过的候选与基线比较契约；当前画像器不会生成
这种契约。自动进化永远不会批处理写操作、shell 命令、凭证或网络动作。

## 13. 更新检查与反馈

```yaml
updates:
  enabled: true
  channel: "auto"         # auto（跟随已安装版本线）| latest | next
  check_interval: "15m"   # 使用本地缓存，不阻塞 TUI 启动

feedback:
  repository: "gnfy/selfmind"
  labels: []
  endpoint: ""
```

启动检查只负责发现新版本并提示，不会在工作过程中静默替换二进制。
`selfmind update check` 只查询版本；`selfmind update` 会调用当前包管理器安装、
验证新二进制，并在安全回合边界重启正在运行的 gateway。所有升级提示统一
使用 `selfmind update`。版本相同时也会刷新 npm 安装，以恢复本地临时替换过的
包内容；当前构建比通道版本更新时不会自动降级，除非显式使用 `--force`。
预发布版本在
`channel: auto` 下跟随 `next`，稳定版本跟随 `latest`。

`selfmind feedback` 默认只生成本地脱敏报告；显式使用 `--send` 时，才会通过
已登录的 GitHub CLI 向配置的仓库创建 Issue。SelfMind 不保存 GitHub token。

---

## 最小可用配置

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

其余全部回退到合理默认值。随需求增长再加 IM 渠道、逐角色例外和记忆治理。
**任何改动后都要重启 daemon。**
