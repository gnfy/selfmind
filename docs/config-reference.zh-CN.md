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
model:
  provider: "codex-cli"      # 下面的 provider id、custom:<名字> 或 profile id
  default: "gpt-5.5"         # 模型名

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
    model: "kimi-for-coding"
  codex-cli:
    base_url: "https://chatgpt.com/backend-api/codex"
    protocol: "codex_responses"
    model: "gpt-5.5"
```

- `protocol` 取值：`openai_chat`、`openai_compatible`、`anthropic_messages`、
  `codex_responses`，按你的 endpoint 协议选。
- **复用外部 CLI 登录**：Codex CLI、Claude Code、Gemini CLI、Qwen CLI 的凭据
  可直接复用，无需 API key（这些 profile 可以不写 `api_key`）。
- 三个一等公民之外的任何供应商都走 `provider_profiles`，把 `model.provider`
  指向该 profile id 即可。

## 2. 模型路由（角色）

后台任务用比主对话更便宜/更快的模型，让重的记忆/技能工作不占用你的主配额。

```yaml
models:
  source: "local"
  roles:
    memory_extract:          # 记忆写入 + 整理 + 压缩摘要
      provider: "kimi-coding"
      model: "kimi-for-coding"
    background_review:       # 技能/记忆自审、smart 模式审批裁决
      provider: "kimi-coding"
      model: "kimi-for-coding"
    semantic_recall:         # 召回的查询扩展（可选）
      provider: "kimi-coding"
      model: "kimi-for-coding"
    skill_curator: { provider: "kimi-coding", model: "kimi-for-coding" }
    fast_classifier: { provider: "kimi-coding", model: "kimi-for-coding" }
```

角色名固定；没配的角色回退到主模型。指向便宜模型可以把后台工作从主 provider
挪开。

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
> `allowlist`，并在 `allow_from` 里填你自己的 openid。**

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
      headers: {}
```

默认为空。每个服务器的工具按需注册。

## 12. Agent 调优（高级）

```yaml
agent:
  max_iterations: 90            # 每轮工具循环最大步数
  max_retries: 3
  log_level: "INFO"             # DEBUG | INFO | WARN | ERROR
  llm_max_retries: 5            # 传输层重试次数
  llm_retry_base: "300ms"       # 退避基数
  llm_retry_cap: "30s"          # 退避上限
  llm_stream_idle_timeout: "180s"  # SSE 流卡住多久后中断
editor:
  large_paste_chars: 1000       # TUI 大段粘贴识别
  large_paste_lines: 10
evolution:
  enabled: true                 # 技能/自我改进审查
  min_complexity_threshold: 5
  nudge_interval: 10
```

默认值已按常规使用调好；只在排查传输抖动或调工具循环时才动。

---

## 最小可用配置

```yaml
model:
  provider: "codex-cli"
  default: "gpt-5.5"
provider_profiles:
  codex-cli:
    base_url: "https://chatgpt.com/backend-api/codex"
    protocol: "codex_responses"
    model: "gpt-5.5"
web:
  search_backend: "tavily"
  api_key: "tvly-xxxx"
```

其余全部回退到合理默认值。随需求增长再加 IM 渠道、角色模型、记忆治理。
**任何改动后都要重启 daemon。**
