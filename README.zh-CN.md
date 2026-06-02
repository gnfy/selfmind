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
- gateway 客户端命令：`send`、`status`、`stop`、`tasks`、`workspaces`、`workspace add`、`workspace use`、`new`、`id`。
- 通用 IM Webhook：`POST /v1/im/{platform}`。
- 账号绑定 API：CLI、微信、飞书、QQ relay 等渠道可以绑定到同一个 `person_id`。
- workspace 隔离：文件、搜索、补丁、终端工具会限制在允许的工作目录内。
- 长期记忆、历史会话检索、Skill 管理、后台复盘、Skill curator。
- Hermes 风格的原生工具调用：OpenAI-compatible provider 优先使用 native tool calls，失败后回退到旧文本工具调用格式。
- `models.roles` 模型路由：编码、记忆提取、后台复盘、Skill 整理、语义召回可以使用不同模型。

仍属于第一版或后续规划：

- 官方飞书、微信、QQ SDK adapter 还没有完整接入。
- IM 出站发送、失败重试、长消息拆分、原生审批按钮还需要生产级加固。
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

1. 选择供应商：OpenAI、Anthropic、Google、已保存的自定义 endpoint，或 `Custom endpoint (enter URL manually)`。
2. 输入或保留 API key。
3. SelfMind 尝试实时拉取供应商模型列表。
4. 选择模型；如果拉取失败，可以手动输入模型名。
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

storage:
  type: "sqlite"
  data_dir: "~/.selfmind/data"

gateway:
  addr: "127.0.0.1:8765"
  token: ""
  drain_timeout: "30s"

agent:
  soul: "You are SelfMind, a helpful AI assistant."
  max_iterations: 90
  max_retries: 3
  log_level: "INFO"

memory:
  auto_extract_interval: 5
  auto_extract_min_chars: 80
  semantic_recall: true
  use_memory_fence: true

evolution:
  enabled: true
  mode: "auto"
  min_complexity_threshold: 3
  auto_archive_confidence: 0.8
  nudge_interval: 10

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

mcp:
  servers: []

cron:
  enabled: true

delegation:
  provider: ""
  model: ""
  api_key: ""
  max_retries: 3
  max_iterations: 50

editor:
  large_paste_chars: 1000
  large_paste_lines: 10
```

旧配置仍兼容：`providers.openai_api_key`、`providers.anthropic_api_key`、`providers.gemini_api_key`、`agent.provider`、`agent.model` 仍会读取，但新的保存格式会使用 `model` 和嵌套 `providers`。

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
| `/skills` | Skill 的 list/view/search/archive/pin/unpin/delete/stats。 |
| `/memory` | 查看或删除长期记忆。 |
| `/curator` | 查看或运行 Skill curator。 |
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
| `GET` | `/v1/tasks` | 查看任务列表。 |
| `GET` | `/v1/tasks/current` | 查看当前任务和 active run。 |
| `POST` | `/v1/workspaces/register` | 注册本地 workspace。 |
| `GET` | `/v1/workspaces` | 查看 workspace。 |
| `GET` | `/v1/gateway/status` | 查看 gateway 进程状态和 active runs。 |
| `POST` | `/v1/gateway/shutdown` | 请求 gateway 优雅停机。 |

聊天消息不会跨渠道自动镜像。CLI 里的聊天不会自动发到 IM，IM 消息也不会自动显示到 CLI。共享的是任务、run、workspace、memory 和 skill 状态。

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

新增模型厂商：

- OpenAI-compatible 厂商：优先用 `selfmind model` 的自定义 endpoint，不需要写代码。
- 新协议族：在 `internal/kernel/llm` 实现新的 adapter，并在 `internal/app/agent.go` 接入。

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
