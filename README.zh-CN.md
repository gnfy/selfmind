# SelfMind 中文说明

[English README](README.md) · [中文开发指南](docs/development-guide.zh-CN.md)

SelfMind 是一个用 Go 编写的个人 AI Agent 运行时。它的目标不是一次性的聊天机器人，而是一个可以长期运行的个人工作代理：本地 TUI、后台 gateway、CLI 命令、IM/Webhook 渠道共享同一个身份、任务状态、工作目录、长期记忆和 Skills。

简单说：

- 日常交互：运行 `selfmind`。
- 需要 24 小时后台工作：运行 `selfmind gateway start`。
- 配置模型：运行 `selfmind model`。
- 增加新模型厂商：优先使用 `Custom endpoint (enter URL manually)` 配置 OpenAI-compatible 接口，不需要重新构建版本。

## 当前能力

已经可以用于个人版日常使用：

- 单一用户入口 binary：`selfmind`。
- 本地终端 TUI：支持 slash commands、工具调用状态、记忆、Skills、checkpoint、curator。
- 常驻 gateway：`selfmind gateway start/run/status/stop/restart`。
- gateway 客户端命令：`send`、`status`、`stop`、`tasks`、`workspaces`、`approvals`、`approve`、`reject`、`workspace add`、`workspace use`、`new`、`id`。
- 通用 IM Webhook：`POST /v1/im/{platform}`。
- IM outbound 基础层：支持通用 webhook/Telegram 回发、长消息拆分、失败重试和持久 outbound 队列。
- 账号绑定 API：CLI、微信、飞书、QQ relay 等渠道可以绑定到同一个 `person_id`。
- 持久任务运行状态：run heartbeat、异常重启标记 interrupted、最近事件查询和 gateway `/events` 控制响应。
- workspace 隔离：文件、搜索、补丁、终端工具会限制在允许的工作目录内。
- 长期记忆、历史会话检索、Skill 管理、后台复盘、Skill curator。
- Hermes 风格的原生工具调用：OpenAI-compatible provider 优先使用 native tool calls，失败后回退到旧文本工具调用格式；工具层包含重复失败/无进展 guardrail 和敏感信息脱敏。
- `models.roles` 模型路由：编码、记忆提取、后台复盘、Skill 整理、语义召回可以使用不同模型。
- 动态模型 runtime：支持 `provider_profiles`、实时模型列表、本地模型列表缓存，以及 Codex CLI、Claude Code、Gemini CLI、Qwen CLI 的 best-effort 登录复用。

仍属于第一版或后续规划：

- 官方飞书、微信、QQ SDK adapter 还没有完整接入。
- IM 原生审批按钮、企业平台官方 SDK、富媒体附件和更完整的平台签名/加密模式还需要生产级加固。
- SaaS 管理后台、租户模型密钥托管、计费策略、队列/worker 横向扩展还未完成。

## 运行要求

- 本地开发需要 Go 1.26+。
- 至少配置一个模型供应商，才能获得真实 AI 回复。
- 官方发布包当前优先面向 Linux 服务器：`linux-amd64` 和 `linux-arm64`。
- Windows / macOS 可以用于本地开发和调试，但当前不是官方发布目标。

## 构建与运行

构建用户入口：

```sh
go build -ldflags="-s -w" -o selfmind ./cmd/selfmind
```

Windows：

```powershell
go build -ldflags="-s -w" -o selfmind.exe ./cmd/selfmind
```

开发时运行：

```sh
GOWORK=off go run ./cmd/selfmind
```

不要使用：

```sh
go run cmd/selfmind/main.go
```

这个命令只会编译单个文件，可能导致 Go 把 `selfmind/internal/...` 当成标准库路径去查找。请使用 `go run ./cmd/selfmind` 或 `go build ./cmd/selfmind`。

`cmd/selfmindd` 只保留为隐藏兼容 wrapper，不作为用户构建或启动入口。

## 配置文件

SelfMind 只使用一个 YAML 配置文件，不需要 `.env`。

默认路径：

```text
~/.selfmind/config.yaml
```

指定任意配置路径：

```sh
./selfmind -f ./config/config.yaml
./selfmind --config ./config/config.yaml gateway start
./selfmind -f ./config/config.yaml model
```

也可以通过环境变量指定：

```sh
SELF_CONFIG=/etc/selfmind/config.yaml ./selfmind gateway run
```

默认配置文件不存在时，SelfMind 会自动创建。使用显式 `-f` 路径时，`selfmind model` 这类需要写配置的命令也会初始化文件。

## 配置模型

交互式配置：

```sh
selfmind model
```

交互流程参考 Hermes：

1. 选择供应商：OpenAI、Anthropic、Google、`Custom endpoint (enter URL manually)`、Coding CLI 登录复用入口，或其它内置 provider profile。
2. 输入或保留 API key；Codex CLI、Claude Code、Gemini CLI、Qwen CLI 会尝试复用已有 CLI 登录。
3. SelfMind 尝试实时拉取供应商模型列表并写入本地缓存。
4. 选择模型；如果实时列表不可用，会使用 fallback 列表或允许手动输入模型名。
5. 选择结果写入 `config.yaml`。

常用非交互命令：

```sh
selfmind model current
selfmind model list
selfmind model set openai gpt-4o
selfmind model set custom:local-llm qwen2.5-coder
```

### 配置示例

```yaml
# 默认对话模型。model.provider 可以指向 providers、providers.custom 或 provider_profiles。
model:
  provider: "openai"
  default: "gpt-4o"

# 核心 provider 配置区，适合 OpenAI / Anthropic / Google 这类一等公民供应商。
providers:
  openai:
    # 支持 ${ENV_NAME}，运行时会从环境变量展开。
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
  # 一次性或本地自定义 endpoint，例如 Ollama、本地模型网关、临时企业代理。
  # 使用时把 model.provider 设置为 custom:ollama。
  custom:
    - name: "ollama"
      base_url: "http://localhost:11434/v1"
      api_key: ""
      protocol: "openai_compatible"
      model: "llama3"
      # 可选：给本地模型补充上下文长度等元信息。
      models:
        llama3:
          context_length: 8192

# 可扩展 provider 注册表。适合 MiniMax、Kimi、DeepSeek、Z.AI、OpenRouter、
# 企业内部模型网关，以及未来协议兼容的新厂商。
provider_profiles:
  minimax:
    api_key: "${MINIMAX_API_KEY}"
    base_url: "https://api.minimax.io/anthropic"
    protocol: "anthropic_messages"
    model: "MiniMax-M2.7"
  kimi-coding:
    api_key: "${KIMI_CODING_API_KEY}"
    base_url: "https://api.kimi.com/coding/v1"
    protocol: "openai_compatible"
    model: "kimi-for-coding"

# 可选：SelfMind 自己的 JSON 凭证文件，用来把密钥从 config.yaml 拆出去。
auth:
  credentials_file: "~/.selfmind/auth.json"

# 本地数据目录，SQLite 会保存会话、记忆、任务、运行状态等数据。
storage:
  type: "sqlite"
  data_dir: "~/.selfmind/data"

# 常驻 gateway 配置。CLI、IM/Webhook 会通过它共享任务状态。
gateway:
  addr: "127.0.0.1:8765"
  # 为空时默认本机使用；部署到服务器或开放网络时建议设置 token。
  token: ""
  drain_timeout: "30s"
  # 通用 outbound webhook，可用于自定义 IM relay 回发。
  outbound_webhook_url: ""
  outbound_webhook_token: ""
  # Telegram 回发 token；其它平台可以接入 gateway webhook。
  telegram_token: "${TELEGRAM_BOT_TOKEN}"
  delivery_max_message_chars: 3500
  delivery_retry_attempts: 3

# Agent 主循环配置。
agent:
  soul: "You are SelfMind, a helpful AI assistant."
  max_iterations: 90
  max_retries: 3
  log_level: "INFO"

# 长期记忆和语义召回配置。
memory:
  auto_extract_interval: 5
  auto_extract_min_chars: 80
  semantic_recall: true
  use_memory_fence: true

# 后台学习/复盘相关配置，用于沉淀 memory 和 skill。
evolution:
  enabled: true
  mode: "auto"
  min_complexity_threshold: 3
  auto_archive_confidence: 0.8
  nudge_interval: 10

# 角色模型路由。不同后台任务可以使用更便宜或更快的模型。
models:
  source: "local"
  roles:
    # 主编码/任务执行模型。
    coding_agent:
      provider: "openai"
      model: "gpt-4o"
    # 记忆抽取模型。
    memory_extract:
      provider: "google"
      model: "gemini-1.5-flash"
    # 任务结束后的后台复盘模型。
    background_review:
      provider: "google"
      model: "gemini-1.5-flash"
    # Skill 整理、归档、治理模型。
    skill_curator:
      provider: "google"
      model: "gemini-1.5-flash"
    # 历史会话语义召回模型。
    semantic_recall:
      provider: "google"
      model: "gemini-1.5-flash"

# MCP server 列表，默认关闭。
mcp:
  servers: []

# 定时任务入口，个人版可先保持默认。
cron:
  enabled: true

# 多 Agent 委派配置；为空时使用默认模型策略。
delegation:
  provider: ""
  model: ""
  api_key: ""
  max_retries: 3
  max_iterations: 50

# 大段粘贴识别阈值，用于 TUI 输入体验。
editor:
  large_paste_chars: 1000
  large_paste_lines: 10
```

### 模型配置字段说明

`model` 是当前默认模型选择：

| 字段 | 说明 |
|---|---|
| `model.provider` | 默认 provider ID，可以是 `openai`、`anthropic`、`google`，也可以是 `custom:<name>` 或 `provider_profiles` 里的 ID。 |
| `model.default` | 默认模型名，例如 `gpt-4o`、`kimi-for-coding`、`MiniMax-M2.7`。 |

`providers` 是核心 provider 配置区，适合放 SelfMind 内置的一等公民供应商：

| 字段 | 说明 |
|---|---|
| `providers.openai` | OpenAI 官方接口配置。默认协议通常是 `openai_chat`。 |
| `providers.anthropic` | Anthropic Messages API 配置。默认协议通常是 `anthropic_messages`。 |
| `providers.google` | Google Gemini 的 OpenAI-compatible endpoint 配置。 |
| `providers.custom` | 一次性或本地自定义 endpoint 列表，例如 Ollama、本地网关、临时企业代理。使用时 `model.provider` 写成 `custom:<name>`。 |

`provider_profiles` 是可扩展 provider 注册表，适合放 MiniMax、Kimi、DeepSeek、Z.AI、OpenRouter、企业内部模型网关，或未来新增但协议兼容的厂商：

| 字段 | 说明 |
|---|---|
| `provider_profiles.<id>` | `<id>` 会成为 provider ID，例如 `kimi-coding`、`minimax`。 |
| `api_key` | API key，支持 `${ENV_NAME}` 环境变量展开。也可以留空，改用环境变量或 `auth.credentials_file`。 |
| `base_url` | 供应商 endpoint。OpenAI-compatible 通常以 `/v1` 结尾；Anthropic-compatible 通常填供应商的 Anthropic 网关根地址。 |
| `protocol` | 协议族：`openai_chat`、`openai_compatible`、`anthropic_messages`、`codex_responses`。只要新厂商兼容这些协议，就不需要改代码。 |
| `model` | 该 profile 的默认模型名。 |

`auth.credentials_file` 是可选的本地 JSON 凭证文件，用来把密钥从 `config.yaml` 里拆出去：

```yaml
auth:
  credentials_file: "~/.selfmind/auth.json"
```

凭证解析优先级是：命令行/角色显式传入的 key、`config.yaml` 中的 `api_key`、`auth.credentials_file`、环境变量、外部 CLI 登录复用。外部 CLI 登录复用只覆盖 Codex CLI、Claude Code、Gemini CLI、Qwen CLI，并且不会刷新 OAuth token。

推荐选择：

- OpenAI、Anthropic、Google：优先写 `providers`。
- Ollama、本地模型、临时 OpenAI-compatible endpoint：写 `providers.custom`。
- MiniMax、Kimi、DeepSeek、Z.AI、OpenRouter、企业内部模型网关：优先写 `provider_profiles`。
- 不想把密钥写进 YAML：写 `auth.credentials_file` 或用环境变量。

### Provider Profiles 与认证复用

SelfMind 当前的模型解析走 `internal/modelruntime`。它支持：

- 内置 provider：`openai`、`anthropic`、`google`、`codex-cli`、`claude-code`、`gemini-cli`、`qwen-cli`、`openrouter`、`minimax`、`kimi-coding`、`deepseek`、`zai`、`alibaba-coding-plan`。
- 自定义 OpenAI-compatible endpoint：运行 `selfmind model`，选择 `Custom endpoint (enter URL manually)`。
- `provider_profiles`：当厂商只是 base URL、协议、模型名变化时，直接在 YAML 中配置，不需要重新构建。
- P2 认证复用只覆盖 Codex CLI、Claude Code、Gemini CLI、Qwen CLI。其它厂商使用 API key、环境变量展开或 `auth.credentials_file`。

示例：

```yaml
model:
  provider: "kimi-coding"
  default: "kimi-for-coding"

provider_profiles:
  kimi-coding:
    api_key: "${KIMI_CODING_API_KEY}"
    base_url: "https://api.kimi.com/coding/v1"
    protocol: "openai_compatible"
    model: "kimi-for-coding"
  minimax:
    api_key: "${MINIMAX_API_KEY}"
    base_url: "https://api.minimax.io/anthropic"
    protocol: "anthropic_messages"
    model: "MiniMax-M2.7"

auth:
  credentials_file: "~/.selfmind/auth.json"
```

`auth.credentials_file` 是 SelfMind 自己的 JSON 凭证文件，格式如下：

```json
{
  "providers": {
    "codex-cli": { "access_token": "..." },
    "kimi-coding": { "api_key": "..." }
  }
}
```

如果复用外部 CLI 登录，SelfMind 会尝试读取常见本地认证文件，例如 `~/.codex/auth.json`、Claude Code、Gemini CLI、Qwen CLI 的 OAuth 文件。当前不会刷新 OAuth token；过期后需要回到原 CLI 重新登录。

MiniMax 和 Kimi Coding Plan 接入：

- MiniMax Coding Plan：使用 `provider: "minimax"`，`base_url: "https://api.minimax.io/anthropic"`，`protocol: "anthropic_messages"`，模型用 `MiniMax-M2.7` 或 `MiniMax-M2.7-highspeed`。
- Kimi Coding Plan：使用 `provider: "kimi-coding"`，`base_url: "https://api.kimi.com/coding/v1"`，`protocol: "openai_compatible"`，模型固定用 `kimi-for-coding`。
- 如果要走 Kimi Code 的 Anthropic-compatible endpoint，把 `provider_profiles.kimi-coding.base_url` 设置为 `https://api.kimi.com/coding`，`protocol` 设置为 `anthropic_messages`，模型仍然使用 `kimi-for-coding`。
- 如果要用普通 Kimi Open Platform API，而不是 Coding Plan 额度，使用自定义 OpenAI-compatible endpoint，`base_url` 设置为 `https://api.moonshot.ai/v1`。

旧配置仍兼容：`providers.openai_api_key`、`providers.anthropic_api_key`、`providers.gemini_api_key`、`providers.openrouter_api_key`、`providers.minimax_api_key`、`agent.provider`、`agent.model` 仍会读取，但新的保存格式会使用 `model`、嵌套 `providers` 和 `provider_profiles`。

## 模型路由

默认模型放在：

```yaml
model:
  provider: "openai"
  default: "gpt-4o"
```

不同任务角色可以通过 `models.roles` 指定不同模型：

- `coding_agent`：主 Agent 编码和任务执行。
- `memory_extract`：记忆事实提取。
- `background_review`：任务结束后的后台学习复盘。
- `skill_curator`：Skill 整理和治理。
- `semantic_recall`：历史会话语义召回。

个人版从本地 YAML 读取。后续 SaaS 版可以沿用同样的 role 名称，从数据库中的租户、用户、workspace、预算策略里解析模型配置。

## 本地 TUI

启动：

```sh
selfmind
```

指定配置：

```sh
selfmind -f ./config/config.yaml
```

常用 slash commands：

| 命令 | 作用 |
|---|---|
| `/help` | 查看 TUI 命令。 |
| `/status` | 查看 provider、model、运行时间、token、任务和 gateway 状态。 |
| `/tasks` | 查看本地 gateway 任务。 |
| `/skills` | Skill 的 list/view/search/catalog/install/audit/archive/pin/unpin/delete/stats/reload。 |
| `/skills history <name>` | 查看某个 Skill 的学习审计记录。 |
| `/skills undo <change_id>` | 撤销支持回滚的 Skill 学习变更。 |
| `/bundles` | 查看、创建、删除 Skill bundle。 |
| `/reload-skills` | 不重启进程，重新加载磁盘上的 Skill。 |
| `/memory` | 查看、审计、删除或撤销长期记忆。 |
| `/curator` | 查看、预览、生成报告、运行或恢复 Skill curator 操作。 |
| `/checkpoint` | 保存、读取、列出或删除会话 checkpoint。 |
| `/migrate` | 从 Hermes Agent 迁移 Skills。 |
| `/model` | 在 TUI 内查看或切换当前模型。 |
| `/clear` | 清屏。 |
| `/exit` | 退出。 |

快捷键：

| 快捷键 | 行为 |
|---|---|
| `Enter` | 提交输入。 |
| `Shift+Enter` | 插入换行。 |
| `Ctrl+C` | 取消当前 run 或退出。 |
| `Ctrl+V` | 粘贴；大段粘贴会转换成更易读的块。 |

## Skills

Skills 是 SelfMind 的“过程记忆”：可复用的工作流、检查清单、项目约定和踩坑经验。它们按租户存放在：

```text
~/.selfmind/<tenant>/skills/
```

新的 Skill 默认使用目录结构：

```text
<skill-name>/
  SKILL.md
  references/
  templates/
  scripts/
  assets/
```

旧的扁平 `.md` Skill 仍然兼容。安装、创建或修改 Skill 后，可以通过 `/skills reload` 或 `/reload-skills` 热加载，不需要重启 SelfMind。

Catalog 安装参考 Hermes 的 provenance 规则：`skill_catalog` 会把安装来源写入 `~/.selfmind/<tenant>/skills/.catalog/lock.json`，并把来源标记为 `catalog-installed`。同名目录 Skill 或旧版 `.md` Skill 默认不会被覆盖；只有显式使用 `--force` 才会替换。强制重装前，旧副本会先移动到 `~/.selfmind/<tenant>/skills/.catalog/backups/`。Curator 不会自动归档 catalog-installed、manual、bundled 或 pinned Skill。

常用命令：

```sh
/skills list
/skills view codebase-inspection
/skills search docker
/skills catalog
/skills install official/codebase-inspection
/skills install ./my-skill --name my-skill
/skills install https://raw.githubusercontent.com/org/repo/main/path/SKILL.md
/skills install ./my-skill --name my-skill --force
/skills audit
/skills history codebase-inspection
/skills undo <change_id>
/skills reload
/reload-skills
```

可以像 Hermes 一样用 slash command 直接调用某个 Skill：

```text
/codebase-inspection 帮我先检查这个仓库
```

Skill bundle 可以一次加载多个 Skill。Bundle 文件是 YAML，存放在 `~/.selfmind/<tenant>/skill-bundles/`。

```sh
/bundles create backend-dev codebase-inspection,test-driven-change
/bundles list
/backend-dev 按这个组合工作流实现需求
```

Curator 命令：

```sh
/curator status
/curator run --dry-run --report
/curator run --report
/curator restore old-skill
```

Curator 默认只治理 `agent-created` 的 Skill；被 pin 的 Skill 不会被自动归档或删除；手写和 catalog 安装的 Skill 默认不参与自动治理。

## Memory

Memory 用于保存长期有效的用户偏好、项目事实和环境约定，不应该保存一次性任务进度。

```sh
/memory list
/memory history
/memory history user
/memory remove user "喜欢简洁回答"
/memory undo <change_id>
```

Memory 和 Skill 的学习变更会写入 `~/.selfmind/<tenant>/learning/`。当学错、过期或不想保留时，先用 history 找到 `change_id`，再用 undo 回滚。

## Gateway 模式

启动长期运行的本地 gateway：

```sh
selfmind gateway start
```

前台运行：

```sh
selfmind gateway run
```

管理 gateway：

```sh
selfmind gateway status
selfmind gateway stop
selfmind gateway restart
```

通过 CLI 向 gateway 派发任务：

```sh
selfmind send "现在任务进度怎么样？"
selfmind send --async "运行测试并修复失败"
selfmind status
selfmind tasks
selfmind stop
selfmind workspaces
selfmind approvals
selfmind approve <approval_id>
selfmind reject <approval_id>
selfmind workspace add .
selfmind workspace use <workspace_id>
selfmind new "实现 checkout 页面"
```

作为 gateway-backed REPL：

```sh
SELF_USE_GATEWAY=1 selfmind
```

gateway 运行时文件位于：

```text
<storage.data_dir>/gateway/
```

包括：

- `gateway.pid`
- `gateway_state.json`
- `gateway.lock`
- `gateway.log`

## Gateway HTTP API

如果 gateway 暴露到 localhost 以外，请在 `config.yaml` 中设置 `gateway.token`。客户端可以使用 `Authorization: Bearer <token>` 或 `X-SelfMind-Token`。

| 方法 | 路径 | 作用 |
|---|---|---|
| `GET` | `/health` | 健康检查。 |
| `POST` | `/v1/message` | 发送 CLI/IM/Web 消息。 |
| `POST` | `/v1/im/{platform}` | 通用 IM Webhook 入口。 |
| `POST` | `/v1/accounts/bind` | 将平台账号绑定到已有 person。 |
| `GET` | `/v1/approvals` | 查看 pending 或指定状态的审批请求。 |
| `POST` | `/v1/approvals/respond` | 批准或拒绝审批请求。 |
| `GET` | `/v1/tasks` | 查看任务列表。 |
| `GET` | `/v1/tasks/current` | 查看当前任务和 active run。 |
| `GET` | `/v1/tasks/events` | 查看当前任务或指定 `task_id` 的最近 run/tool/learning 事件。 |
| `POST` | `/v1/workspaces/register` | 注册本地 workspace。 |
| `GET` | `/v1/workspaces` | 查看 workspace。 |
| `GET` | `/v1/gateway/status` | 查看 gateway 进程状态和 active runs。 |
| `POST` | `/v1/gateway/shutdown` | 请求 gateway 优雅停机。 |

聊天消息不会跨渠道自动镜像。CLI 里的聊天不会自动发到 IM，IM 消息也不会自动显示到 CLI。共享的是任务、run、workspace、memory 和 skill 状态。

IM 异步任务完成后，如果配置了 outbound sender，gateway 会把结果回发到原渠道；否则消息会保留在 `control.db` 的 outbound 队列中，等待后续配置 sender 后重试。当前内置 sender：

- `gateway.telegram_token`：使用 Telegram `sendMessage` 回发到 inbound 的 `channel/chat_id`。
- `gateway.outbound_webhook_url`：把统一 JSON payload POST 到自定义 relay。

长消息会按 `gateway.delivery_max_message_chars` 拆分，失败会按 `gateway.delivery_retry_attempts` 重试。gateway 任务进度可以用 `/status`，最近事件可以用 `/events`。

## Linux 发布

GitHub Actions release workflow：

```text
.github/workflows/release.yml
```

支持 tag 自动发布和手动发布。当前发布产物：

- `selfmind-<tag>-linux-amd64.tar.gz`
- `selfmind-<tag>-linux-arm64.tar.gz`
- `SHA256SUMS.txt`

发布包包含：

- `selfmind`
- `install.sh` / `uninstall.sh`
- `selfmind.service`
- README 和 docs

Linux 安装脚本会创建 `/etc/selfmind/config.yaml`、`/var/lib/selfmind/data` 和 `selfmind` 系统用户。systemd 服务会使用下面的命令启动 gateway：

```sh
/usr/local/bin/selfmind -f /etc/selfmind/config.yaml gateway run
```

启动服务前，先编辑 `/etc/selfmind/config.yaml` 配置模型供应商。

## 二次开发入口

重要目录：

```text
cmd/selfmind/              用户入口
cmd/selfmindd/             隐藏兼容 wrapper
internal/cliapp/           selfmind 命令路由、model/gateway/client 命令
internal/app/              storage、agent、tools、gateway 组装
internal/platform/config/  YAML 配置加载、默认值、兼容和保存
internal/kernel/           Agent 循环、记忆、复盘、原生工具调用
internal/kernel/llm/       模型适配器和模型路由
internal/modelruntime/     provider profiles、credentials、model catalog/cache
internal/tools/            内置工具和工具中间件
internal/gateway/          TUI、HTTP API、router、渠道 adapter
internal/control/          身份、workspace、task、run 状态
internal/runtime/gateway/  gateway 进程、pid、lock、state、start/stop
```

新增工具：

1. 在 `internal/tools` 实现 `tools.Tool`。
2. 在 `internal/app/tools.go` 注册。
3. 如果工具触碰文件、终端、网络、记忆或 skill，补 workspace/approval/process middleware。
4. 添加工具和 dispatcher 测试。

工具运行时规则：

- 新应用路径优先使用 `tools.NewRegistry()` 和 `tools.NewDispatcherWithRegistry(...)`。`tools.NewDispatcher()` / `tools.GlobalRegistry()` 只作为旧兼容入口保留。
- 新代码不要把租户、workspace、MCP 或 skill 工具注册到全局 registry。
- dispatcher 参数转换是严格的；非法 integer、number、boolean 应返回错误，不要静默变成 `0` 或 `false`。
- clarify 和审批回调优先通过 dispatcher/registry 上下文注入。

新增模型厂商：

大多数新厂商不需要改代码。

- 一次性 OpenAI-compatible endpoint：运行 `selfmind model`，选择 `Custom endpoint (enter URL manually)`。
- 可复用的本地配置：在 `provider_profiles.<id>` 里添加 `base_url`、`protocol`、`api_key` 和可选 `model`。
- 新协议族才需要新增 Go adapter。当前核心协议族包括 OpenAI Chat Completions、OpenAI-compatible、Anthropic Messages、Google Gemini 的 OpenAI-compatible endpoint、OpenAI/Codex Responses-compatible endpoint。

新增协议族时：

1. 在 `internal/kernel/llm` 实现 `llm.Provider`。
2. 在 `internal/modelruntime` 增加 protocol/runtime metadata。
3. credential discovery 和 model-list fetching 仍放在 `internal/modelruntime`，不要塞进 `internal/app`。
4. `internal/app/agent.go` 只做 protocol 到 adapter 的装配。
5. 补 streaming、native tool calls、认证解析、模型列表和 `models.roles` 路由测试。

新增 IM 平台：

```text
platform adapter
  验证签名
  解析平台 payload
  归一化 inbound message
  可选发送 outbound message
        |
        v
gateway
  身份绑定
  workspace/task/run 状态
  agent dispatch
```

不要把任务状态、长期记忆 ownership 放到平台 adapter 里。gateway 才是身份、任务、workspace、run、handoff、approval 的拥有者。

## 测试

运行全部测试：

```sh
GOWORK=off go test ./...
```

常用检查：

```sh
go test ./internal/platform/config ./internal/cliapp ./internal/runtime/gateway
go build ./cmd/selfmind
git diff --check
```

## 设计约束

- `kernel` 不应该依赖 `tools`、`gateway` 或 `server`。
- Agent 通过 `AgentBackend` 访问工具。
- workspace scoped 工具必须遵守 `allowed_roots`。
- gateway 的任务和身份状态存储在 `control.db`。
- memory/session 状态存储在 `storage.data_dir`。
- Skills 按租户隔离在 `~/.selfmind/<tenant>/skills`。
- 个人版使用本地 YAML；SaaS 版应沿用相同模型 role 概念，把租户/用户/workspace 的 provider policy 和 secrets 移到数据库。

## License

MIT
