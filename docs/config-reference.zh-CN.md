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
- **密钥**：API key 可直接写文件里，也可用 `${环境变量名}` 引用（加载时展开）。
  多用户主机上把文件设为 `chmod 600`。
- **时间**用 Go 时长字符串：`"300ms"`、`"30s"`、`"5m"`、`"24h"`。

---

## 1. 模型与 provider（必填）

唯一必须配的东西：谁来回答你。

```yaml
models:
  primary:
    provider: "codex-cli"    # provider id、custom:<名字> 或 profile id
    model: "gpt-5.6-sol"
    reasoning: "xhigh"       # 可选；省略或 auto 表示模型默认值

providers:                   # 一等公民供应商
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
  custom: []                 # 一次性/本地 endpoint（Ollama、网关）

provider_profiles:           # 可扩展注册表（Kimi、MiniMax、DeepSeek、OpenRouter…）
  kimi-coding:
    api_key: "${KIMI_API_KEY}"
    base_url: "https://api.kimi.com/coding"
    protocol: "anthropic_messages"
  codex-cli:
    base_url: "https://chatgpt.com/backend-api/codex"
    protocol: "codex_responses"
```

- `protocol` 取值：`openai_chat`、`openai_compatible`、`anthropic_messages`、
  `codex_responses`，按你的 endpoint 协议选。
- **复用外部 CLI 登录**：Codex CLI、Claude Code、Gemini CLI、Qwen CLI 的凭据
  可直接复用，无需 API key（这些 profile 可以不写 `api_key`）。
- 三个一等公民之外的供应商可走 `provider_profiles`，再由
  `models.primary.provider` 引用该 profile。
- provider 配置只管理连接和认证；模型选择统一写在 `models.primary`。
  endpoint 中旧的 `model` 字段仅为兼容历史配置继续读取。

### 通用 Provider 请求扩展

SelfMind 采用与 OpenAI Python SDK 一致的三个名称：`extra_headers`、
`extra_body`、`extra_query`。Provider 通用参数放在
`provider_profiles.<id>`；只有某个后台角色需要不同参数时，才放在
`models.roles.<role>`。它们统一适用于 OpenAI Chat/Compatible、Anthropic
Messages 和 Responses 协议。可对照
[OpenAI Python 请求扩展说明](https://github.com/openai/openai-python#undocumented-request-params)
与 [DeepSeek 用户隔离说明](https://api-docs.deepseek.com/zh-cn/quick_start/rate_limit)。

```yaml
provider_profiles:
  deepseek:
    extra_headers:
      X-Org-Proxy-Token: "${ORG_PROXY_TOKEN}"
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
- 三个映射中的字符串都支持 `${ENV_VAR}` 展开。
- DeepSeek 将 `user_id` 定义为同一账号下用于调度与隔离的标识，并非凭证；但仍
  建议使用稳定、无业务含义的值，不要写姓名、邮箱、手机号、提示词等隐私信息。
- `selfmind model check` 会显示合并后的 header 来源，以及 body/query 的键名，
  但不会回显 body/query 的值。
- 旧 `headers` 继续兼容；新配置和文档统一使用 `extra_headers`，同名键由新字段
  覆盖。

有类型的传输兼容规则放在 `quirks`，任意厂商请求参数放在 `extra_*`：

```yaml
provider_profiles:
  example-anthropic:
    quirks:
      auth_header: bearer
      tool_schema: anthropic
      thinking_mode: anthropic
      user_identity_field: auto
      http_version: auto
      prompt_cache: false       # 显式 false 可以覆盖内置 true
```

布尔 quirk 省略时继承内置 profile；只有 endpoint 契约不同时才显式写 `true` 或
`false`。匿名身份可用 `auto`、`user_id`、`metadata.user_id`、`off`；HTTP 版本可用
`auto`、`http1`、`http2`。`system_message_mode` 已废弃并被忽略。
`selfmind model check` 会显示最终契约和警告。

## 2. 模型路由

后台任务用比主对话更便宜/更快的模型，让重的记忆/技能工作不占用你的主配额。

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

`reasoning` 和 `service_tier`
都可省略；省略或写 `auto` 时使用 provider/模型默认值，不强制向接口发送。
存在能力元数据时，`selfmind model set` 会动态校验取值，
`selfmind model current` 会显示探测到的默认值。

`kimi-coding` 的全部角色都使用供应商默认的 Anthropic Messages 传输
（`https://api.kimi.com/coding/v1/messages`），与 Hermes 和 Kimi Coding
Plan `/coding` 路径的实际协议一致。角色级 `protocol` 只用于自定义网关
或协议不同的特殊部署；正常使用 Kimi Coding Plan 时应省略。

不需要也不存在 `default` 角色。`vision` 等能力特定角色不会自动继承辅助模型，
需要时应显式配置。旧配置继续逐项列出后台角色也完全兼容。

smart 审批比普通角色继承更严格：它使用显式配置或由 `models.auxiliary` 继承的
`fast_classifier`；旧配置可以
兼容回退到显式配置的 `background_review`，但审批裁决绝不会静默使用
`models.primary`。两条路由都不存在或未及时响应时，操作会升级为人工确认。

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
  auto_extract_interval: 5       # 最多每 N 轮抽取一次持久事实
  auto_extract_min_chars: 80     # 跳过过短的轮次
  semantic_recall: true          # 每轮附带相关的历史工作片段
  use_memory_fence: true         # 把召回记忆包装成不可信的背景数据
  governance:
    enabled: true
    mode: "shadow"               # shadow（只报告）→ merge-only → full（加上限）
    model_role: "memory_extract" # 整理判决用哪个角色
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
  presence_idle_timeout: "5m"  # CLI 空闲这么久后不再算"已附着"（推送转到 IM）
  pending_notify_after: "2m"   # 无人应答的审批/提问在此时长后补推到 IM
  outbound_retention: "336h"   # 终态投递记录保留 14 天；0 表示禁用清理
  delivery_max_message_chars: 3500
  delivery_retry_attempts: 3
```

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
  maintenance_model_role: "memory_extract"  # run 后标签/事实整理用的角色
  maintenance_fallback_roles: ["background_review", "fast_classifier"] # 显式廉价角色故障切换，不会隐式使用主模型
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
`maintenance_fallback_roles` 是有序列表。遇到不可重试的 provider
故障后，SelfMind 会依次尝试其中已显式配置的角色，跳过不存在的角色，
且绝不会隐式回退到主编码模型。所有角色都使用同一家 provider 时，无法
实现服务商级故障切换。例如，可让 Kimi 继续作为默认廉价角色，并添加
MiniMax 备用角色：

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

## 10. 意图路由（高级，很少改）

```yaml
intent:
  mode: "hybrid"       # 显式命令/续接线索的匹配方式
  thresholds:
    direct: 0.8
    ask: 0.55
```

普通语言永远会到达 agent；这些旋钮只调显式命令和续接检测。大多数用户用默认即可。

## 11. MCP 服务器

```yaml
mcp:
  servers:
    - name: "my-tools"
      transport: "stdio"        # stdio | http
      command: "my-mcp-server"  # stdio 用
      args: []
      url: ""                   # http 用
      extra_headers: {}
```

默认为空。每个服务器的工具按需注册。

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
editor:
  large_paste_chars: 1000       # TUI 大段粘贴识别
  large_paste_lines: 10
evolution:
  enabled: true                 # 技能审查 + 确定性工作流画像
  mode: "auto-readonly"         # observe | shadow | auto-readonly
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

`approval_triage_timeout` 与主模型的传输超时相互独立。辅助或显式配置的
`fast_classifier` 如果未在该预算内返回，smart 模式会安全降级为人工审批。
默认值是 30 秒；设置过短会把可用的推理型廉价模型误判成不可用。

自进化画像是由已完成 run 的持久事件确定性派生，不会增加前台模型调用。
`observe` 只记录画像；`shadow` 还会评估受限的只读批处理候选，但不会使用；
`auto-readonly` 只有在观察次数和低失败率门槛都通过后才启用候选。自动进化
永远不会批处理写操作、shell 命令、凭证或网络动作。旧的 `mode: auto` 仍作为
`auto-readonly` 的兼容别名接受。

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
provider_profiles:
  codex-cli:
    base_url: "https://chatgpt.com/backend-api/codex"
    protocol: "codex_responses"
web:
  search_backend: "tavily"
  api_key: "tvly-xxxx"
```

其余全部回退到合理默认值。随需求增长再加 IM 渠道、逐角色例外和记忆治理。
**任何改动后都要重启 daemon。**
