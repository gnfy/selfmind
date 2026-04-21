# SelfMind

**生产级多租户 AI Agent 内核**，使用 Go 1.26+ 编写，支持自主自我进化、动态 Skill 加载和现代化 Bubble Tea TUI。

```
┌─────────────────────────────────────────────────────────────┐
│   CLI   /   WeChat   /   DingTalk   /   Telegram   /  Web  │
└──────────────────────────┬──────────────────────────────────┘
                           │
                  ┌────────▼──────────┐
                  │  Gateway          │  意图分类 · 身份映射 · 任务路由
                  │  (gateway/)      │  多渠道统一响应
                  └────────┬──────────┘
                           │
                  ┌────────▼──────────┐
                  │  Kernel            │  Agent 推理循环 · Token 预算管理
                  │  (kernel/)        │  自进化引擎 · FTS5 记忆系统
                  └────────┬──────────┘
                           │
                  ┌────────▼──────────┐
                  │  Tools            │  中间件链 (Auth → Tenant → Approval → Rate)
                  │  (tools/)        │  内置工具 · MCP 客户端 · Skill 加载器
                  └───────────────────┘
```

## 核心特性

| 类别 | 详情 |
|------|------|
| **多租户记忆** | 每个租户独立 SQLite + FTS5 全文搜索，跨会话上下文recall |
| **自进化** | ≥3 工具调用的复杂任务完成后，自动异步归档新 Skill（非阻塞） |
| **动态 Skill** | 运行时加载 `.md` Skill 文件，增量能力无需重新编译 |
| **MCP 客户端** | 通过 stdio 或 HTTP 连接任意 MCP 服务器 |
| **多渠道** | CLI / 微信 / 钉钉 / Telegram — 统一身份 + 任务上下文 |
| **Bubble Tea TUI** | 高密度终端 UI，斜杠命令、流式渲染、智能粘贴 |
| **纯 Go 构建** | `CGO_ENABLED=0`，静态二进制，零运行时依赖 |
| **LLM 提供商** | Anthropic · OpenAI · OpenRouter · Gemini · MiniMax，配置即插拔 |
| **中间件链** | Auth → TenantIsolation → Approval → RateLimit → Logging |
| **崩溃恢复** | Agent 状态检查点持久化到 SQLite，重启后恢复 |

---

## 快速开始

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
  anthropic_api_key: "sk-ant-..."

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
  nudge_interval: 10

storage:
  data_dir: "~/.selfmind/data"

mcp:
  servers: []

cron:
  enabled: false
EOF
```

### 运行

```sh
./selfmind
# 或指定租户 ID（用于多租户隔离）
SELF_TENANT_ID=user2 ./selfmind
```

---

## 模型推荐

SelfMind 支持多模型混搭，以下是各场景的推荐搭配，帮助你快速上手：

### 推荐配置

| 场景 | 推荐模型 | 推荐理由 |
|------|---------|---------|
| 主力推理（日常对话、代码、写作） | **MiniMax M2.7** | 性价比极高，支持超长上下文，响应速度快 |
| 复杂 Agent 任务（多步推理、工具调用） | **MiniMax M2.7-highspeed** | 高速度版本，适合高频调用 |
| 备用 / 精准任务 | **Anthropic Claude Sonnet 4** | 推理能力强，工具调用稳定 |
| 成本敏感场景 | **Google Gemini 2.5** | 免费额度充足，API 价格低 |

### 获取 MiniMax API Key

推荐使用 MiniMax，**新用户有免费 token 额度**：

👉 [https://platform.minimaxi.com/subscribe/token-plan?code=9Yt7HxCaov&source=link](https://platform.minimaxi.com/subscribe/token-plan?code=9Yt7HxCaov&source=link)

注册后在控制台创建 API Key，填入 `config.yaml` 的 `minimax_api_key` 字段即可。

### 配置示例

```yaml
providers:
  minimax_api_key: "eyJ..."   # MiniMax（推荐，新用户有免费额度）

agent:
  provider: "minimax"          # 默认使用 MiniMax
  model: "MiniMax-M2.7-highspeed"  # 推荐速度优先版本
```

---

## 配置参考

**文件**: `~/.selfmind/config.yaml`

```yaml
# ── API 密钥（至少配置一个）─────────────────────────────
providers:
  anthropic_api_key: ""       # Anthropic (Claude)
  openai_api_key: ""          # OpenAI (GPT-4)
  openrouter_api_key: ""      # OpenRouter (聚合)
  gemini_api_key: ""          # Google Gemini
  minimax_api_key: ""         # MiniMax
  # 自定义 Provider（如本地 Ollama）
  custom:
    - name: "ollama"
      base_url: "http://localhost:11434/v1"
      api_key: "ollama"
      model: "llama3"

# ── Agent 行为 ────────────────────────────────────────
agent:
  soul: "You are SelfMind, a helpful AI assistant."  # 系统提示词
  provider: "anthropic"     # 默认提供商名称
  model: "claude-sonnet-4-7"
  max_iterations: 90        # 单次任务最大推理循环
  max_retries: 3            # LLM 调用重试次数
  log_level: "INFO"          # DEBUG | INFO | WARN | ERROR

# ── 自进化配置 ────────────────────────────────────────
evolution:
  enabled: true
  mode: "auto"              # auto | nudge | manual
  min_complexity_threshold: 3  # 触发 review 的最小工具调用数
  auto_archive_confidence: 0.8 # 自动归档置信度阈值
  nudge_interval: 10         # 每 N 次工具调用触发一次 review

# ── 存储 ──────────────────────────────────────────────
storage:
  data_dir: "~/.selfmind/data"

# ── MCP 服务器 ───────────────────────────────────────
mcp:
  servers: []
  # 示例 — stdio 模式:
  # - name: "filesystem"
  #   transport: "stdio"
  #   command: "npx"
  #   args: ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
  # 示例 — HTTP 模式:
  # - name: "my-server"
  #   transport: "http"
  #   url: "http://localhost:8080"
  #   headers:
  #     Authorization: "Bearer token"
  #   env_filter: ["API_KEY", "TOKEN"]  # 日志中脱敏的环境变量

# ── Cron 调度器 ──────────────────────────────────────
cron:
  enabled: false

# ── 子 Agent 委托 ─────────────────────────────────────
delegation:
  provider: "anthropic"
  model: "claude-sonnet-4-7"
  api_key: ""
```

---

## 架构详解

### 请求生命周期

```
用户输入
  │
  ▼
Gateway.Handle(unifiedUID, channel, input)
  │
  ├─ IntentClassifier ──► 闲聊/快捷问题（直接回复，不走 Agent）
  │
  ├─ IdentityMapper ─────► 平台 ID → 统一 UID 映射
  │
  └─ Agent.RunConversation ────────────────────────────────┐
       │                                                    │
       ├─ ContextEngine.BuildMessages()                    │
       │     ├─ memory.GetFacts()                        │
       │     ├─ memory.SearchSessions() (FTS5)          │
       │     └─ 构建 system prompt + 记忆上下文          │
       │                                                    │
       ├─ LLM.Chat() ──► 流式响应                        │
       │     │                                         │
       │     └─ ExtractToolCalls() ──► Dispatch         │
       │              │                    │            │
       │              │        中间件管道                   │
       │              │      Auth → Tenant →             │
       │              │       Approval → Rate            │
       │              │                    │            │
       │              │        Tool.Execute()           │
       │              └────► 工具结果 ──────────────────┘
       │                                                    │
       ├─ tool_call_count >= nudge_interval?              │
       │     └─ triggerEvolutionReview() [async goroutine] │
       │                                                    │
       └─ return 响应
```

### 目录结构

```
cmd/selfmind/
    main.go                  # 入口，仅做组件组装
internal/
    kernel/                  # 核心推理引擎（无外部依赖）
        agent.go             # Agent 推理循环 + 事件通道
        backend.go           # AgentBackend 接口（kernel ↔ tools 唯一耦合点）
        context_engine.go    # Token 预算管理（128K 窗口）
        reflection.go        # 自进化决策引擎
        skill_store.go       # Skill 注册表 + 归档存储
        evolution_test.go    # 自进化逻辑测试
        llm/                 # LLM 适配器（Anthropic · OpenAI · OpenRouter · Gemini · MiniMax）
        memory/              # SQLite FTS5 记忆系统
            sqlite_provider.go   # 存储引擎
            storage.go           # Storage 接口
            fact.go              # 事实存储
        task/
            manager.go           # 全局任务管理器
            cron/scheduler.go   # Cron 调度器
        identity/            # 平台 ID → 统一 UID 映射

    gateway/                # 平台适配层（接收外部消息）
        router/
            gateway.go       # 统一网关：意图分类 + 任务路由
        cli/
            controller.go    # Bubble Tea TUI 控制器（输入/渲染/快捷键）
        wechat/              # 微信适配器（桩实现）
        telegram/            # Telegram 适配器（桩实现）
        channel/
            bridge.go        # 跨平台桥接

    tools/                  # 工具系统
        tool.go              # Tool 接口 + BaseTool + 参数校验/类型转换
        dispatcher.go        # 工具注册表 + 中间件管道
        builtin.go           # 内置工具（terminal · read_file · write_file · list_files · search_files · patch · glob · get_current_time）
        extended.go          # 扩展工具（web_search · web_extract · vision · execute_code · session_search · todo · delegate_task）
        middleware.go        # 中 中间件（Auth · TenantIsolation · Approval · RateLimit · Logging）
        skill_loader.go      # SKILL.md 解析器 + 运行时工具注册
        mcp_client.go        # MCP 客户端（stdio + HTTP）
        tool_backend.go      # tools 包对 kernel.AgentBackend 的具体实现
        delegate.go          # 子 Agent 委托
        memory.go            # 记忆工具
        process.go           # 进程/管道工具
        process_registry.go  # 进程注册表
        web_extract.go       # 网页内容提取
        vision.go            # 图像分析

    app/                     # 应用层初始化
        agent.go             # Agent 初始化
        gateway.go           # Gateway 初始化
        tools.go             # 工具注册
        storage.go           # 存储初始化
        migration.go         # Hermes → SelfMind 数据迁移
        delegation.go        # 委托初始化
        multi_agent.go       # 多 Agent 协调

    platform/                # 平台工具
        config/loader.go     # YAML 配置加载器
        log/log.go          # 结构化日志
        adapter.go          # 通用平台接口
internal/ui/
    components/
        editor.go            # 输入框（支持大粘贴占位符 · Paste 事件拦截）
    common/common.go        # 通用样式
    layout/layout.go        # 布局系统

docs/
    development-guide.md     # 开发指南
    selfmind-evolution-design.md  # 自进化设计文档
    selfmind-evolution-roadmap.md # 自进化路线图
```

---

## 工具系统

### 工具接口

```go
// internal/tools/tool.go
type Tool interface {
    Name() string
    Description() string
    Schema() ToolSchema          // OpenAI tool schema 格式
    Execute(args map[string]interface{}) (string, error)
}
```

### 内置工具

| 工具 | 说明 | 关键参数 |
|------|------|----------|
| `terminal` | 执行 Shell 命令 | `command: string` |
| `read_file` | 读取文件 | `path: string`, `offset: int`, `limit: int` |
| `write_file` | 写入文件 | `path: string`, `content: string` |
| `list_files` | 列出目录 | `path: string` |
| `search_files` | 文件内容搜索 | `pattern: string`, `path: string`, `file_glob?: string` |
| `patch` | 查找替换编辑 | `path: string`, `old_string: string`, `new_string: string` |
| `glob` | Glob 模式匹配 | `pattern: string`, `path: string` |
| `get_current_time` | 当前时间戳 | `format?: string` |
| `web_search` | 网页搜索 | `query: string` |
| `web_extract` | 提取页面内容 | `url: string`, `question: string` |
| `vision_analyze` | 图像分析 | `image_url: string`, `question: string` |
| `execute_code` | 执行 Python 代码 | `code: string` |
| `session_search` | 搜索会话历史 | `query: string`, `limit?: int` |
| `todo` | 任务列表 CRUD | `action: string`, `todos: []` |
| `delegate_task` | 派生子 Agent | `goal: string`, `context: string`, `toolsets: []string` |

### 中间件管道

```
AuthMiddleware              → 验证 API Key / 租户凭证
        ↓
TenantIsolationMiddleware   → 自动注入 _tenant_id 参数
        ↓
ApprovalMiddleware          → 高危命令（rm -rf 等）人工审批
        ↓
RateLimitMiddleware         → 按租户限流
        ↓
LoggingMiddleware           → 记录所有调用输入/输出
        ↓
Tool.Execute(args)          → 工具实际逻辑
```

### 编写新工具

**Step 1: 实现 Tool 接口**

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
            description: "对输入文本执行有用的处理",
            schema: ToolSchema{
                Type: "object",
                Properties: map[string]PropertyDef{
                    "input": {Type: "string", Description: "要处理的输入文本"},
                },
                Required: []string{"input"},
            },
        },
    }
}

func (t *MyTool) Execute(args map[string]interface{}) (string, error) {
    input, ok := args["input"].(string)
    if !ok || input == "" {
        return "", fmt.Errorf("input 必须是非空字符串")
    }
    return "result: " + input, nil
}
```

**Step 2: 注册到 `internal/app/tools.go`**

```go
// 在 InitTools() 函数中追加：
d.RegisterTool(NewMyTool())
```

**Step 3: 重新编译运行**

```sh
go build -o selfmind ./cmd/selfmind && ./selfmind
```

---

## Skill 系统

Skill 是 `~/.selfmind/skills/` 目录下的 `.md` 文件，使用 YAML front matter 定义元数据：

```markdown
---
name: my-skill
description: "何时使用此 Skill 及其作用"
trigger: ["/my-skill", "/ms"]
parameters: ["input"]
confidence: 0.85
---

# My Skill

LLM 执行此 Skill 的步骤指导。
包含具体操作步骤、示例和边界情况处理。
```

Skills 在启动时由 `SkillLoader` 加载，也可以被自进化引擎自动归档。

### 自进化流程

```
Agent 完成复杂任务（≥3 工具调用，≥4 步）
    │
    ▼
triggerEvolutionReview() [goroutine — 不阻塞用户]
    │
    ├─ scanExistingSkills() ──► 注入已有 Skill 列表到 prompt
    ├─ buildReviewPrompt(task_history, existing_skills)
    ├─ LLM.ChatCompletion() ──► 返回: SKIP | CREATE|name|content | UPDATE|name|content
    ├─ parseReviewResponse()
    └─ ArchiveSkill(result)
          ├─ scanForDangers(content) ──► 检查凭证泄露、危险命令
          ├─ 原子写入（临时文件 → rename）
          └─ notifyCh ──► TUI 显示 "💾 skill xxx created"
```

---

## 添加新 LLM Provider

在 `internal/kernel/llm/` 下实现 `Provider` 接口：

```go
// internal/kernel/llm/provider.go
type Provider interface {
    ChatCompletion(ctx context.Context, messages []Message) (string, error)
    Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
    StreamChat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error)
}
```

然后在 `internal/platform/config/loader.go` 的工厂函数中注册即可。参考 `adapters.go` 中已有的 Anthropic / OpenAI / OpenRouter / Gemini / MiniMax 适配器实现。

---

## 添加新平台渠道

以添加 DingTalk 为例：

**Step 1: 创建适配器**

```go
// internal/gateway/myding/adapter.go
package myding

type Adapter struct {
    gateway *router.Gateway
}

func NewAdapter(gw *router.Gateway) *Adapter {
    return &Adapter{gateway: gw}
}

// HandleMessage 接收平台消息，返回 Agent 回复
func (a *Adapter) HandleMessage(platformUserID, content string) (string, error) {
    ctx := context.Background()
    unifiedUID, err := a.gateway.ResolveUID(ctx, "myding", platformUserID)
    if err != nil {
        return "", err
    }
    resp, err := a.gateway.Handle(ctx, unifiedUID, "myding", content)
    if err != nil {
        return "", err
    }
    if !resp.IsStreaming {
        return resp.Content, nil
    }
    var fullText string
    for event := range resp.Stream {
        if event.Err != nil {
            return fullText, event.Err
        }
        fullText += event.Content
    }
    return fullText, nil
}
```

**Step 2: 在 `cmd/selfmind/main.go` 中接入**

```go
mydingAdapter := myding.NewAdapter(gwDeps.Gateway)
go func() {
    http.ListenAndServe(":8080", mydingAdapter.HTTPHandler())
}()
```

**Step 3: 实现 `HandleRawMessage(body []byte)` 解析平台 webhook 格式（XML/JSON）**

---

## TUI 使用

### 输入快捷键

| 快捷键 | 行为 |
|--------|------|
| `Enter` | 提交输入（自动展开粘贴占位符） |
| `Shift+Enter` 或 `Ctrl+J` | 插入换行（多行输入） |
| `Ctrl+C` | **有输入内容：** 清空输入缓冲区。**Agent 工作中：** 取消当前任务。**空闲状态：** 退出程序 |
| **粘贴** | 多行粘贴（≥80 行或 ≥8000 字符）显示 `[[ ...N lines... ]]` 占位符，按 `Enter` 展开并提交完整内容 |
| `Ctrl+L` | 清空所有聊天消息 |
| `↑ / ↓` | 导航输入历史 |

### 斜杠命令

| 命令 | 说明 |
|------|------|
| `/help` | 显示所有可用命令 |
| `/status` | 显示 Agent 状态和配置 |
| `/model <name>` | 切换 LLM 模型 |
| `/models` | 列出所有可用模型 |
| `/config` | 显示当前配置 |
| `/tasks` | 列出所有任务（含 cron 和活跃任务） |
| `/sessions` | 列出所有会话 |
| `/retry` | 重试上一次失败的操作 |
| `/undo` | 撤销上一次输入 |
| `/title` | 为当前会话生成标题 |
| `/stop` | 停止当前 Agent 推理 |
| `/clear` | 清空聊天消息 |
| `/exit` | 退出 TUI |

---

## 开发

### 运行测试

```sh
go test ./...                          # 所有测试
go test ./internal/kernel              # kernel 包
go test -v ./internal/tools -run TestDispatcher
```

### 项目约定

- **依赖单向**：依赖只能从外向内。`kernel` 不依赖 `gateway` 或 `tools`。
- **多租户隔离**：每个数据操作都必须携带 `tenantID`，`TenantIsolationMiddleware` 自动注入。
- **工具调用格式**：LLM 输出 `[TOOL:tool_name:{"arg":"val"}]`，由 `kernel/agent.go` 中的正则提取。
- **无硬编码 Provider**：所有 LLM Provider 都在 `config.yaml` 中配置。新增 Provider 只需在 `internal/kernel/llm/` 实现 `Provider` 接口。
- **异步自进化**：自进化在 goroutine 中运行，不阻塞 Agent 响应流。
- **AgentBackend 接口**：这是 `kernel` 和 `tools` 之间的唯一耦合点，允许替换整个工具层（如用于测试）而不影响 Agent 逻辑。

### 调试

```sh
# 查看详细日志
GOLOG_LEVEL=DEBUG ./selfmind

# 查看 Agent 推理过程（JSON 格式）
GOLOG_LEVEL=DEBUG GOLOG_FORMAT=json ./selfmind 2>&1 | jq .
```

---

## 设计决策

**为什么用 SQLite 而不是纯内存？**
多租户隔离需要持久化。每个租户独立数据库文件（`dataDir/tenantID/memory.db`），FTS5 支持跨会话上下文召回。Cron 任务和后台进程也需要持久化状态。

**为什么工具不分文件？**
Go 没有 Python 那样的运行时工具发现机制。按类别分组（`builtin.go` · `extended.go`）更实用，也便于管理包数量。

**为什么需要 AgentBackend 接口？**
它是 `kernel` 和 `tools` 之间唯一的耦合点。可以替换整个工具层（如测试场景）而不触碰 Agent 逻辑。

**为什么不直接在输入框显示完整多行内容？**
Hermes 的做法是大粘贴显示占位符（因为完整的代码/内容会破坏 TUI 布局），按 Enter 展开后提交。这样既保证了终端UI的整洁，又确保了内容的完整性。

---

## License

MIT
