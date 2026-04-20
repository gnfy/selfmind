# SelfMind 开发指南

> 面向开源贡献者的完整项目文档。涵盖架构设计、代码组织、核心流程、以及已知的用户体验缺口。

---

## 目录

1. [项目概述](#1-项目概述)
2. [快速上手](#2-快速上手)
3. [架构总览](#3-架构总览)
4. [核心包详解](#4-核心包详解)
5. [配置系统](#5-配置系统)
6. [工具系统](#6-工具系统)
7. [Skill 自进化](#7-skill-自进化)
8. [TUI 开发](#8-tui-开发)
9. [多租户与安全](#9-多租户与安全)
10. [已知缺口与待办](#10-已知缺口与待办)

---

## 1. 项目概述

**SelfMind** 是一个生产级的多租户 AI Agent 内核，使用 Go 1.26+ 开发，`CGO_ENABLED=0`，编译为纯静态二进制。

### 设计哲学

- **配置驱动**：尽量减少代码硬编码，模型供应商、参数、工具链均由 `config.yaml` 动态定义
- **Clean Architecture**：严格分层（Gateway → Kernel → Tools），依赖单向流动
- **多租户隔离**：每个租户独立 SQLite 数据库，租户 ID 贯穿所有数据操作
- **异步优先**：长时间操作（自进化、后台任务）不阻塞主会话

### 项目结构

```
cmd/selfmind/           # 唯一入口，组装所有组件
internal/
  kernel/               # 核心推理引擎（Agent、Context、Reflection）
    agent.go            # 推理循环 + 事件通道
    context_engine.go  # Token 预算管理 + 消息构建
    reflection.go       # 自进化决策 + Skill 归档
    backend.go          # ToolBackend 接口定义
    llm/               # LLM 适配器（Anthropic / OpenAI / OpenRouter / Gemini / MiniMax）
    memory/             # SQLite 提供商（FTS5 全文搜索、Checkpoint、Fact）
    identity/          # 平台 ID → 统一 UID 映射
    task/              # 全局任务管理器 + Cron 调度器
  gateway/              # 平台消息适配层
    router/            # 统一消息处理器 + Intent 分类
    cli/               # Bubble Tea TUI 控制器
    wechat/            # WeChat 适配器（存根）
    telegram/          # Telegram 适配器（存根）
  tools/                # 工具系统
    dispatcher.go      # 注册表 + Middleware 管道
    builtin.go         # 内置工具（Terminal、ReadFile、Grep 等）
    extended.go        # 扩展工具（WebSearch、Vision、TTS 等）
    middleware.go      # Auth / TenantIsolation / Approval / RateLimit
    skill_loader.go    # SKILL.md 解析 + 动态工具注册
    mcp_client.go      # MCP 服务器连接管理
  ui/                   # TUI 组件（Bubble Tea / Lip Gloss）
internal/platform/      # 平台层（Config 加载）
```

---

## 2. 快速上手

### 构建

```sh
git clone https://github.com/your-org/selfmind.git
cd selfmind
go build -ldflags="-s -w" -o selfmind ./cmd/selfmind
```

### 配置

```sh
mkdir -p ~/.selfmind
cat > ~/.selfmind/config.yaml << 'EOF'
providers:
  anthropic_api_key: "sk-ant-your-key-here"

agent:
  soul: "You are SelfMind, a helpful AI assistant."
  provider: "anthropic"
  model: "claude-sonnet-4-7"
  max_iterations: 90
  max_retries: 3

evolution:
  enabled: true
  mode: "auto"
  min_complexity_threshold: 3
  auto_archive_confidence: 0.8
  nudge_interval: 10        # 每 N 次工具调用触发一次自进化

storage:
  data_dir: "~/.selfmind/data"

mcp:
  servers: []
  # 示例：连接一个本地 MCP 服务器
  # - name: "filesystem"
  #   transport: "stdio"
  #   command: "npx"
  #   args: ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
EOF
```

### 运行

```sh
./selfmind
# TUI 亮起，直接输入内容即可对话
Ctrl+C 退出
```

---

## 3. 架构总览

### 请求生命周期

```
用户输入
    │
    ▼
Gateway.Handle(unifiedUID, channel, input)
    │
    ├─ IntentClassifier ──► Casual Chat（直接回复，不触发 Agent）
    │
    ├─ IdentityMapper ─────► 查找/创建 unifiedUID
    │
    └─ Agent.RunConversation ─────────────────┐
         │                                      │
         ├─ ContextEngine.BuildMessages()      │
         │     ├─ memory.GetFacts()            │
         │     ├─ memory.SearchSessions()      │
         │     └─ 构建 prompt                  │
         │                                      │
         ├─ LLM.Chat() ──► Stream 响应         │
         │     │                               │
         │     └─ ExtractToolCalls() ──► Dispatch(tool, args)
         │              │                       │
         │              │         Middleware 管道
         │              │           │           │
         │              │           ▼           │
         │              │      Tool.Execute()   │
         │              │           │           │
         │              │           ▼           │
         │              └────► 工具结果 ───────┘
         │
         ├─ 工具调用计数 ≥ nudgeInterval？
         │     └─ goroutine triggerEvolutionReview()  [异步，不阻塞]
         │
         └─ return response
```

### 关键设计决策

**1. 为什么用 SQLite 而不是内存存储？**
- 多租户隔离需要持久化；每个租户独立 DB 文件
- FTS5 全文搜索支持跨会话上下文召回
- Cron 任务和 Background Process 需要持久化状态

**2. 为什么不每个工具一个文件？**
- Go 没有 Python 那样的运行时 AST 工具发现机制
- `tools/builtin.go` 和 `tools/extended.go` 按功能分组（文件系统 / Web / 系统），比按工具分文件更实用

**3. ToolBackend 接口是什么？**
```go
// internal/kernel/backend.go
type AgentBackend interface {
    Dispatch(name string, args map[string]interface{}) (string, error)
    GetToolDefinitions() []map[string]interface{}
}
```
所有工具注册到 `Dispatcher`，`Agent` 通过这个接口与工具层解耦。这是 `kernel` 和 `tools` 两个包之间的唯一依赖。

---

## 4. 核心包详解

### 4.1 kernel/agent.go — 推理循环

**入口**：`Agent.RunConversation(ctx, tenantID, channel, initialPrompt)`

**核心循环**（最多 `maxIterations` 轮）：
```
for {
    resp, usage, err := a.streamChatWithRetry(ctx, messages)
    messages += assistant(resp)
    calls = ExtractToolCalls(resp)
    if calls == nil { break }  // 无工具调用 → 任务完成

    // 并行执行所有工具调用
    for each call {
        result, err := a.backend.Dispatch(call.Name, call.Args)
        messages += tool(result)
    }

    // 自进化触发（异步，不阻塞）
    toolCallCount += len(calls)
    if toolCallCount >= nudgeInterval {
        triggerEvolutionReview(history)  // goroutine
    }
}
```

**工具调用提取**：使用正则从 LLM 输出中提取 `[TOOL:tool_name:{"arg":"val"}]` 格式。

**事件通道**：`EventChannel chan string` 发送 `tool_start:name`、`tool_end:name:duration:result` 事件，TUI 用这些事件渲染工具执行状态。

### 4.2 kernel/context_engine.go — Token 管理

```go
type ContextEngine struct {
    maxTokens    int  // 模型上下文上限，如 128000
    reserved     int  // 留給系统 prompt 的空间，如 512
    provider     llm.Provider  // 用于计算 token 数量
}
```

**BuildMessages** 流程：
1. 计算可用空间：`available = maxTokens - reserved - systemPromptTokens`
2. 从最新消息向历史回溯，保留在可用空间内的消息
3. 如果历史中有轨迹数据（`SaveTrajectory`），追加到 context 中
4. 返回截断后的消息列表

### 4.3 kernel/reflection.go — 自进化引擎

**触发条件**（需同时满足）：
- `EvolutionConfig.Enabled == true`
- 工具调用计数 ≥ `min_complexity_threshold`
- 复杂度评估为 medium 或 high

**触发后流程**：
```
Reflect(ctx, history)
    ├─ assessComplexity(steps)  ──► skip if trivial
    ├─ scanExistingSkills()     ──► 注入已有 skill 列表到 prompt
    ├─ buildReviewPrompt(history, existing)
    │     └─ 单一 unified prompt，告知已有 skills 避免重复
    ├─ LLM.ChatCompletion(prompt)
    └─ parseReviewResponse(resp)

    └─► ArchiveSkill(result)
          ├─ scanForDangers(content)  ──► 凭证泄露 / 危险命令
          ├─ 确定目标文件路径（新建或已有）
          ├─ 原子写入（tmp file + rename）
          └─ notifyCh ──► TUI 通知
```

**已有 Skill 扫描**：这是 SelfMind 与 Hermes 的关键差异——LLM 在决策时会看到"已有什么"，避免重复创建 `auto-skill-时间戳.md`。

### 4.4 kernel/memory/ — 持久化层

**SQLite Provider**：`dataDir/tenantID/memory.db`

关键表：
- `trajectories` — 完整消息历史，含 `channel` 列实现渠道隔离
- `sessions` — 会话索引，用于 FTS5 全文搜索
- `facts` — 持久化的事实（user preferences、environment facts）
- `checkpoints` — Agent 状态快照，用于崩溃恢复
- `background_processes` — 后台进程（PID 持久化，启动时验证存活）

**FTS5 配置**：
```sql
CREATE VIRTUAL TABLE sessions_fts USING fts5(
    session_id, channel, content, summary,
    content='sessions', content_rowid='rowid'
);
```
查询时 ` MATCH` 关键字搜索，`rank` 排序。

### 4.5 kernel/llm/ — 模型适配器

统一接口：
```go
type Provider interface {
    ChatCompletion(ctx context.Context, messages []Message) (string, error)
    Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
    StreamChat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error)
}
```

适配器列表：
| 文件 | 供应商 | 默认模型 |
|------|--------|---------|
| `adapters.go` | Anthropic | claude-sonnet-4-7 |
| `adapters.go` | OpenAI | gpt-4o |
| `adapters.go` | OpenRouter | anthropic/claude-sonnet-4 |
| `adapters.go` | Gemini | gemini-2.5-flash |
| `adapters.go` | MiniMax | MiniMax-Text-01 |

**动态 Key 加载**：适配器支持 `KeyGetter func() string`，允许从 DB 加载 per-tenant API key，实现多租户 API key 隔离。

---

## 5. 配置系统

**配置文件**：`~/.selfmind/config.yaml`

**加载顺序**（后面的覆盖前面的）：
1. `defaultConfigTemplate` 硬编码默认值
2. `LoadConfig()` 读取 `~/.selfmind/config.yaml`
3. 环境变量 `SELF_` 前缀（如 `SELF_PROVIDERS_ANTHROPIC_API_KEY`）

### 完整配置字段

```yaml
providers:
  anthropic_api_key: ""      # Anthropic API Key
  openai_api_key: ""          # OpenAI API Key
  openrouter_api_key: ""     # OpenRouter API Key
  gemini_api_key: ""         # Google Gemini API Key
  minimax_api_key: ""       # MiniMax API Key

  # 自定义供应商
  custom:
    - name: "ollama"
      base_url: "http://localhost:11434/v1"
      api_key: "ollama"
      model: "llama3"

agent:
  soul: "You are SelfMind..."  # 系统 prompt
  provider: "anthropic"         # 默认供应商名
  model: "claude-sonnet-4-7"   # 默认模型
  max_iterations: 90           # 最大推理轮次
  max_retries: 3               # LLM 调用失败重试次数
  log_level: "INFO"            # 日志级别（当前未使用）

evolution:
  enabled: true
  mode: "auto"                            # "auto" | "nudge" | "manual"
  min_complexity_threshold: 3              # 触发所需最小工具调用数
  auto_archive_confidence: 0.8            # 自动归档置信度阈值
  nudge_interval: 10                      # 每 N 次工具调用触发一次

storage:
  data_dir: "~/.selfmind/data"

mcp:
  servers: []
  # - name: "filesystem"
  #   transport: "stdio"        # "stdio" | "http"
  #   command: "npx"
  #   args: ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
  #   url: "http://localhost:8080"   # for http transport
  #   headers:
  #     Authorization: "Bearer token"
  #   env_filter: ["API_KEY", "TOKEN"]   # 屏蔽的环境变量

delegation:
  provider: "anthropic"
  model: "claude-sonnet-4-7"
  api_key: ""
```

---

## 6. 工具系统

### 6.1 架构

```
LLM 输出 "[TOOL:tool_name:args]"
    │
    ▼
dispatcher.Dispatch(tool_name, args)
    │
    ├─ Middleware 管道（Auth → TenantIsolation → Approval → RateLimit → Logging）
    │
    ├─ 工具注册表查找
    │
    └─ tool.Execute(args)
          │
          └─ return result string
```

### 6.2 内置工具（builtin.go）

| 工具名 | 功能 | 关键参数 |
|--------|------|---------|
| `terminal` | 执行 Shell 命令 | `command: string` |
| `read_file` | 读取文件 | `path: string`, `offset: int`, `limit: int` |
| `write_file` | 写入文件 | `path: string`, `content: string` |
| `list_files` | 列出目录 | `path: string` |
| `search_files` | 搜索文件内容 | `pattern: string`, `path: string` |
| `patch` | 文本替换编辑 | `path`, `old_string`, `new_string` |
| `glob` | 文件名模式匹配 | `pattern: string`, `path: string` |
| `get_current_time` | 获取当前时间 | `format: string`（可选） |

### 6.3 扩展工具（extended.go）

| 工具名 | 功能 |
|--------|------|
| `web_search` | 搜索网页 |
| `web_extract` | 提取网页内容 |
| `vision` | 图片分析（base64 输入） |
| `tts` | 文字转语音 |
| `execute_code` | 执行 Python 代码 |
| `session_search` | 搜索历史会话 |
| `skill_loader` | 加载/管理 skills |
| `todo` | 任务列表管理 |
| `delegate_task` | 委托给子 Agent |

### 6.4 添加新工具

```go
// internal/tools/my_tool.go
package tools

type MyTool struct {
    BaseTool
}

func NewMyTool() *MyTool {
    return &MyTool{
        BaseTool{
            name:        "my_tool",
            description: "Does something useful",
            schema: ToolSchema{
                Type: "object",
                Properties: map[string]PropertyDef{
                    "input": {Type: "string", Description: "Input text"},
                },
                Required: []string{"input"},
            },
        },
    }
}

func (t *MyTool) Execute(args map[string]interface{}) (string, error) {
    input, _ := args["input"].(string)
    if input == "" {
        return "", fmt.Errorf("input is required")
    }
    // do work
    return "result: " + input, nil
}
```

**注册**：在 `internal/app/tools.go` 的 `InitTools()` 中添加：
```go
d.RegisterTool(NewMyTool())
```

### 6.5 Middleware

执行顺序：**Auth → TenantIsolation → Approval → RateLimit → Logging**

| Middleware | 作用 |
|------------|------|
| `AuthMiddleware` | 校验请求来源 |
| `TenantIsolationMiddleware` | 注入 `_tenant_id` 到 args |
| `ApprovalMiddleware` | 危险命令（如 `rm -rf /`）需要用户确认 |
| `RateLimitMiddleware` | 按 tenant 限流 |
| `LoggingMiddleware` | 记录工具调用前后状态 |

---

## 7. Skill 自进化

### 7.1 工作流程

```
Agent 完成一个复杂任务（≥3步，多工具）
    │
    ▼
triggerEvolutionReview() [goroutine, 不阻塞]
    │
    ├─ scanExistingSkills()  ──► 扫描 ~/.selfmind/skills/
    ├─ buildReviewPrompt(task_history, existing_skills)
    │     └─ 告知 LLM 已有哪些 skills，要求避免重复
    ├─ LLM.ChatCompletion(prompt)
    │     └─ 返回：SKIP / CREATE|skill-name|content / UPDATE|skill-name|content
    ├─ parseReviewResponse()
    └─ ArchiveSkill(result)
          ├─ scanForDangers(content)  ──► 凭证泄露 / 危险命令
          ├─ 原子写入（先写 .tmp，再 rename）
          └─ notifyCh ──► TUI 显示 "💾 skill xxx created"
```

### 7.2 复杂度评估

```
toolTypes = 从 steps 中提取的唯一工具数
stepCount = len(steps)

high:    toolTypes ≥ 3 OR stepCount ≥ 6
medium:  toolTypes ≥ 2 OR stepCount ≥ 4
low:     stepCount ≥ 2
trivial: stepCount < 2  → 跳过
```

### 7.3 安全扫描

`scanForDangers()` 检查以下危险模式：

| 类别 | 模式 |
|------|------|
| 凭证泄露 | `curl/wget` 带 `${API_KEY}`、`ghp_*`、`sk-ant-*`、`AKIA*` |
| 危险命令 | `rm -rf /`、`>; /etc/`、命令注入 |
| 敏感路径 | `~/.ssh/`、`~/.hermes/.env`、`~/.aws/`、`~/.docker/config` |
| 环境 dump | `printenv`、`os.environ`、`process.env` |

### 7.4 Skill 格式

```markdown
---
name: log-archiver
description: 自动归档和清理日志文件
triggers:
  - "清理日志"
  - "归档日志文件"
parameters:
  - name: log_dir
    type: string
    description: 日志目录路径
  - name: days
    type: int
    description: 保留多少天内的日志
examples:
  - "帮我清理 /var/log 下的旧日志"
---

# Log Archiver

将指定目录下的日志文件压缩归档，并清理超过指定天数的旧文件。

## 步骤

1. 使用 `list_files` 列出日志目录
2. 使用 `terminal` 执行压缩
3. 清理旧文件

## 陷阱

- 不要删除正在写入的日志文件
- 确认目录路径正确后再执行删除
```

### 7.5 Skill 加载

`skill_loader.go` 在启动时扫描 `~/.selfmind/skills/`：
- 扁平格式：`SkillName.md`
- 目录格式：`SkillName/SKILL.md`

解析 YAML front matter 提取 `triggers`，将匹配的工具注册到 dispatcher。LLM 调用时通过正则匹配触发词决定是否激活 skill。

---

## 8. TUI 开发

### 8.1 架构

```
Bubble Tea 程序
    │
    ├─ Controller（controller.go）─► 输入处理 + 消息路由
    │     ├─ slash commands（/help, /exit, /status, /tasks）
    │     ├─ 输入历史（↑↓ 翻页）
    │     └─ 粘贴检测（复用 OSC 52）
    │
    ├─ Layout（layout.go）─► 整体布局（Sidebar + Editor + StatusBar）
    │
    ├─ Components
    │     ├─ Editor（chat bubbles、stream rendering）
    │     ├─ Sidebar（会话列表）
    │     └─ StatusBar（模型、Token 计数、工具调用统计）
    │
    └─ EventChannel ─► 监听 Agent 的 tool_start / tool_end 事件
          └─ 渲染工具执行动画
```

### 8.2 关键交互

| 交互 | 实现 |
|------|------|
| 打字 | `inputH` 处理，回车发送 |
| 粘贴 | 检测 paste 模式，多行一次性发送 |
| 上/下 切换历史 | `inputHistory` 数组游标 |
| 鼠标选择复制 | Block selection 模式 + OSC 52 剪贴板 |
| 工具执行中 | `tool_end` 事件触发动画 |
| Ctrl+C idle | 退出程序 |
| Ctrl+C 有任务 | 取消当前任务，继续会话 |

### 8.3 添加 Slash Command

在 `controller.go` 的 `inputH` 函数中，找到 slash command 分支，添加：

```go
case "/mycommand":
    result, err := a.gateway.Handle(ctx, unifiedUID, "cli", "/mycommand arg")
    a.AppendAssistantMsg(result)
    return nil, nil
```

---

## 9. 多租户与安全

### 9.1 租户隔离

**数据层**：每个租户独立 DB 文件 `dataDir/tenantID/memory.db`。
```go
// internal/kernel/memory/sqlite_provider.go
dbPath := filepath.Join(dataDir, tenantID, "memory.db")
```

**工具层**：`TenantIsolationMiddleware` 自动注入 `_tenant_id` 到所有工具调用参数。
```go
args["_tenant_id"] = tenantID
```

**Agent 层**：`RunConversation(ctx, tenantID, channel, prompt)` 的 `tenantID` 贯穿所有数据库操作。

### 9.2 已知缺口

**CLI 模式下 tenantID 是 hardcoded**：
```go
// main.go
tenantID := "user1"
```
多租户逻辑在代码中已完整实现，但 CLI 入口未读取真实身份。Web/API 模式可通过 `unifiedUID` 参数传入。

### 9.3 API Key 安全

- `config.yaml` 中明文存储 API key（local 工具，可接受）
- SQLite `secrets` 表支持 per-tenant key 覆盖（`GetSecret()`）
- 日志中自动脱敏（`RedactingFormatter`）

---

## 10. 已知缺口与待办

> 按优先级排列。这些是开源用户体验的关键障碍，建议按此顺序修复。

### P0 — 用户跑不起来的关键障碍

| 缺口 | 描述 | 修复位置 | 状态 |
|------|------|---------|------|
| **零启动引导** | 缺 API key 时 TUI 亮起但用户收到 mock response，不知道要配置 | `app/agent.go` mockProvider 返回引导文本 | ✅ 完成 |
| **无 Skill 管理命令** | 自进化创建的 skill 用户完全看不到，无法 list/delete | `controller.go` 添加 `/skills` 命令 | ✅ 完成 |
| **多租户空壳** | `tenantID := "user1"` hardcoded，CLI 无法使用真实租户隔离 | `main.go` 从 `SELF_TENANT_ID` 环境变量读取 | ✅ 完成 |

### P1 — 影响生产使用的

| 缺口 | 描述 | 修复位置 | 状态 |
|------|------|---------|------|
| **日志系统残缺** | 所有 `println`/`log.Printf`，`log_level` 配置是摆设，无结构化日志 | 引入 `slog`，重写所有日志调用 | ✅ 完成 |
| **Bubble Tea 崩溃处理** | 程序异常时 `fmt.Printf` 直接破坏 TUI 布局 | 改用 `program.Send` 恢复 alt screen | ✅ 完成 |
| **工具参数无校验** | dispatcher 不验证 LLM 传入的 args 是否符合 schema | `dispatcher.go` 添加 schema 验证 | ✅ 完成 |
| **无 `/help` 命令** | 用户不知道有哪些 slash commands | `controller.go` 添加 `/help` handler | ✅ 完成 |

### P2 — 功能完善

| 缺口 | 描述 | 修复位置 |
|------|------|---------|
| **工具生态贫瘠** | 缺少 browser、github、gmail、calendar、jupyter 等常用工具 | `tools/extended.go` 补充 |
| **配置无校验** | 写错 key 名静默失败，无报错 | `platform/config/loader.go` 添加 Validate() |

### P3 — 长期演进

| 缺口 | 描述 |
|------|------|
| Cron 调度器完整实现 | `task/cron/scheduler.go` 目前是存根 |
| MCP 工具自动发现 UI | 用户无法看到有哪些 MCP 工具可用 |
| 多 channel 统一会话历史 | WeChat/Telegram/CLI 的历史尚未真正打通 |
| Web UI | 只有 CLI TUI，无 Web 界面 |

---

## 附录 A：编译与测试

```sh
# 编译
go build -ldflags="-s -w" -o selfmind ./cmd/selfmind

# 运行测试
go test ./...

# Vet 检查
go vet ./...

# 完整构建验证
go build ./... && go test ./... && go vet ./...
```

## 附录 B：关键文件索引

| 文件 | 行数 | 职责 |
|------|------|------|
| `cmd/selfmind/main.go` | ~100 | 入口，组装所有组件 |
| `internal/kernel/agent.go` | ~450 | 推理循环，事件通道 |
| `internal/kernel/context_engine.go` | ~200 | Token 预算，消息截断 |
| `internal/kernel/reflection.go` | ~500 | 自进化决策，Skill 归档 |
| `internal/kernel/memory/sqlite_provider.go` | ~400 | SQLite + FTS5，提供商 |
| `internal/gateway/cli/controller.go` | ~550 | Bubble Tea TUI 控制器 |
| `internal/gateway/router/gateway.go` | ~250 | 统一消息处理器 |
| `internal/tools/dispatcher.go` | ~300 | 工具注册表 + Middleware 管道 |
| `internal/tools/builtin.go` | ~200 | 内置工具实现 |
| `internal/platform/config/loader.go` | ~200 | 配置加载 + 默认模板 |

## 附录 C：Hermes 对比要点

| 维度 | Hermes (Python) | SelfMind (Go) |
|------|----------------|---------------|
| 代码组织 | 单文件 8K-12K 行 | 模块化（69 个 .go 文件） |
| 自进化 | 每轮结束同步，8 次迭代，无去重 | 异步，复杂度触发，带去重 |
| 工具生态 | 40+ 工具，MCP 自动发现 | 基础工具集， MCP 支持 |
| 多租户 | Profiles 进程隔离 | SQLite per-tenant |
| 日志系统 | 三层日志 + RedactingFormatter | 残缺，log_level 未实现 |
| Skill 管理 | `hermes skills list/delete` | ❌ 暂无命令 |
| TUI | React/Ink + JSON-RPC | Bubble Tea/Lip Gloss（更现代） |

---

*文档版本：2026-04-20 | SelfMind v0.1*
