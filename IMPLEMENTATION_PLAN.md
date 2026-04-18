# SelfMind 核心能力补齐：技术实现细节与指令集

本文档旨在为不同模型或后续会话提供明确的代码实现指导，确保在补齐 SelfMind 与 Hermes 的差距时逻辑一致。

---

## 模块 1：工具链激活与注册 (Tool Wiring)
**目标**：将已实现但处于“离线”状态的工具接入核心循环。

### 关键路径
- `internal/tools/builtin.go`: 基础文件工具集。
- `internal/tools/extended.go`: 扩展能力工具集。
- `internal/gateway/cli/controller.go`: TUI 与工具的交互层。

### 详细指令
1.  **激活 `patch` 工具**：
    - 修改 `internal/tools/builtin.go`，在 `RegisterBuiltins(d *Dispatcher)` 函数中添加：
      ```go
      d.RegisterTool(NewPatchTool())
      ```
2.  **激活 `clarify` 工具**：
    - 修改 `internal/tools/extended.go`，在 `RegisterExtendedTools(d *Dispatcher)` 函数中添加：
      ```go
      d.RegisterTool(NewClarifyTool())
      ```
3.  **TUI 回调绑定**：
    - 在 `internal/gateway/cli/controller.go` 的初始化逻辑中，实现 `tools.RegisterClarifyCallback`，将 TUI 的输入事件发送给 `clarify` 工具的阻塞等待逻辑。

---

## 模块 2：持久化事实记忆 (Long-term Fact Memory)
**目标**：实现跨会话的“事实存储”（Fact Memory），用于保存用户偏好和环境定论。

### 架构设计
- **数据库**：在 SQLite 中增加 `facts` 表。
  - `id`: UUID
  - `target`: 'user' (偏好) | 'memory' (技术/环境事实)
  - `content`: 文本内容
  - `created_at`: 时间戳
- **工具实现 (`internal/tools/memory.go`)**：
  - `action`: `add` | `replace` | `remove`
  - `target`: `user` | `memory`
  - `content`: 事实描述
  - `old_text`: (仅用于 replace/remove) 匹配旧条目的关键词。

### 详细指令
1.  **数据层**：更新 `internal/kernel/memory/sqlite_provider.go` 以支持 `facts` 表的 CRUD。
2.  **注入逻辑**：在 `internal/kernel/agent.go` 的 `BuildSystemPrompt` 中，查询所有 `facts` 并按照以下格式注入 System Message：
    ```text
    <MEMORY>
    - [User Preference]: 用户喜欢使用 Go 1.26
    - [Environment]: 项目根目录位于 /work/selfmind
    </MEMORY>
    ```

---

## 模块 3：危险操作审批中间件 (Approval Middleware)
**目标**：防止 Agent 在无监管状态下执行破坏性指令。

### 实现方案
1.  **定义中间件**：在 `internal/tools/middleware.go` 中创建 `ApprovalMiddleware`。
2.  **检测规则**：
    - **Shell 匹配**：`rm`, `> /dev/`, `chmod`, `chown`, `kill`, `pkill`, `shutdown`。
    - **文件系统**：路径包含 `/etc/`, `/root/`, `/dev/` 或项目外的绝对路径。
3.  **触发流程**：当命中规则时，中间件应调用全局的 `ClarifyFn` 请求用户输入 `[y/N]`。
4.  **注册**：在 `internal/app/tools.go` 的 `InitTools` 中通过 `disp.InjectMiddleware(ApprovalMiddleware)` 注入。

---

## 模块 4：后台进程注册表 (Process Registry)
**目标**：对标 Hermes 的 `terminal(background=true)` 功能，实现异步任务追踪。

### 实现细节
1.  **注册表**：在 `internal/tools/` 下新建 `process_registry.go`，维护一个 `map[string]*exec.Cmd`。
2.  **工具修改**：
    - 修改 `ExecuteCommandTool` 的 `Execute` 方法，解析 `background` 参数。
    - 若为后台执行，使用 `cmd.Start()` 而非 `CombinedOutput()`，并生成 UUID 存入注册表。
3.  **新增 `process` 工具**：
    - `action="list"`: 返回所有存活进程的 UUID 和启动命令。
    - `action="poll"`: 返回进程的 stdout 缓冲区内容（需通过 `io.Pipe` 实时捕获输出到内存缓冲区）。
    - `action="kill"`: 强杀进程并清理注册表。

---

## 开发规范 (Standards)
1.  **语言**：Go 1.26+。
2.  **模式**：所有工具必须实现 `Tool` 接口。
3.  **安全性**：禁止硬编码绝对路径（除了 `~/.selfmind` 默认路径），优先使用 `os.UserHomeDir()`。
4.  **简洁性**：注释使用中文，代码逻辑保持扁平。
