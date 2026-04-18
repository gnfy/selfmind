# 多端统一架构说明

> 文档更新时间：2026-04-15

---

## 一、解决的问题

**场景**：用户在公司用 CLI 发起一个代码修改任务，下班后在微信问"那个任务完成了吗"。

**期望**：微信能查到 CLI 创建的任务，并给出准确回答。

**以前的问题**：
- 每个平台（CLI/微信/钉钉）维护独立的对话历史
- 平台间上下文无法共享
- 用户在微信问"继续"，系统不知道指的是哪个任务

---

## 二、核心设计原则

### 1. 一个用户，一个智能体

不管用户从哪个端接入（CLI / 微信 / 钉钉 / Web），都归属于同一个全局用户。

```
用户身份不按渠道划分，而是按 unified_uid（全局用户ID）划分
```

### 2. 聊天历史按渠道隔离

每个入口的对话历史独立存储，互不污染。

```
CLI 的代码上下文  → 不会带入微信的闲聊
微信的闲聊       → 不会污染 CLI 的工作上下文
```

### 3. 任务与记忆全局共享

任务、项目、进度存在用户全局空间，不绑定任何渠道。

```
在 CLI 创建的任务  → 微信可查
在微信创建的任务  → CLI 可继续
```

### 4. 从哪来回哪去

用户在哪个端发指令，结果只返回到那个端。不主动跨端推送，不刷屏。

---

## 三、关键概念

### Unified UID（全局用户ID）

用户在各平台的唯一身份标识，通过 `IdentityMapper` 将平台身份映射到全局用户。

| 平台 | 平台身份标识 | 映射到 unified_uid |
|------|-------------|-------------------|
| CLI | hostname + 随机token | `cli_hostname_xxxxx` |
| 微信 | 微信 openid | `wechat_oxxxxxxx` |
| 钉钉 | 钉钉 userid | `dingtalk_xxxxx` |

### Channel（渠道）

表示对话来自哪个入口：

- `cli` — 命令行/TUI 界面
- `wechat` — 微信
- `dingtalk` — 钉钉
- `web` — 网页端

### Intent（意图）

用户消息分为三类意图：

| 意图 | 说明 | 处理方式 |
|------|------|---------|
| `IntentTask` | 需要执行、创建任务 | 进入 Agent 推理循环，写全局任务 |
| `IntentCasual` | 闲聊、问答 | 直接回复，不建任务，不写轨迹 |
| `IntentContinue` | 继续当前任务 | 加载全局 current_task，继续执行 |

---

## 四、数据存储结构

### 历史记录（trajectories）

按 unified_uid + channel 隔离存储。

```
unified_uid = "wechat_oxxxxxx"
channel = "wechat"

查询：SELECT content FROM trajectories
      WHERE tenant_id = ? AND channel = ?
      ORDER BY created_at DESC LIMIT 10
```

### 全局任务（tasks）

不绑定 channel，全局共享。

```
unified_uid = "wechat_oxxxxxx"
title = "修改 agent.go 添加日志"
status = "in_progress"
```

### 当前任务指针（current_task）

每个用户只有一个"当前进行中任务"。

```
用户说"继续" → 直接定位到这个任务
```

### 任务上下文（task_context）

记录每个任务的关键步骤，不按 channel 隔离。

```
task_id = 42
role = "assistant"
content = "执行 tool: read_file, result: ..."
```

---

## 五、模块说明

### IdentityMapper（身份映射）

位置：`internal/kernel/identity/mapper.go`

```
Resolve(platform, platformID) → unified_uid
Bind(platform, platformID, unified_uid)
GetPlatforms(unified_uid) → []string
```

### TaskManager（任务管理）

位置：`internal/kernel/task/manager.go`

```
CreateTask(unified_uid, title) → task_id
SetCurrentTask(unified_uid, task_id)
GetCurrentTask(unified_uid) → (task, messages)
AppendContext(unified_uid, channel, role, content)
UpdateTaskStatus(unified_uid, task_id, status)
ListTasks(unified_uid) → []Task
```

### IntentClassifier（意图分流）

位置：`internal/gateway/router/intent.go`

```
Classify(input) → IntentTask | IntentCasual | IntentContinue
```

基于关键词规则判断，不调用 LLM。

### StorageProvider 接口

位置：`internal/kernel/memory/storage.go`

所有存储后端（SQLite / Postgres / MySQL）统一接口：

```
SaveTrajectory(ctx, tenantID, channel, traj)
GetLatestContext(ctx, tenantID, channel) → [][]byte
```

---

## 六、数据流示意

### 用户从微信发消息

```
微信消息
    ↓
IdentityMapper.Resolve("wechat", openid) → unified_uid
    ↓
IntentClassifier.Classify(message) → IntentTask
    ↓
TaskManager.CreateTask(unified_uid, title) → task_id
    ↓
TaskManager.SetCurrentTask(unified_uid, task_id)
    ↓
Agent.RunConversation(ctx, unified_uid, "wechat", message)
    ↓
  ContextEngine.BuildMessages → 只加载 channel="wechat" 的历史
  → LLM 推理
  → TaskManager.AppendContext(..., "wechat", ...)
  → MemoryManager.SaveTrajectory(unified_uid, "wechat", traj)
    ↓
回复直接返回到微信
```

### 用户在微信说"继续"

```
微信消息："继续"
    ↓
IdentityMapper.Resolve → unified_uid
    ↓
IntentClassifier.Classify → IntentContinue
    ↓
TaskManager.GetCurrentTask(unified_uid) → (task, messages)
    ↓
Agent.RunConversation(ctx, unified_uid, "wechat", "继续执行当前任务")
    ↓
TaskManager.AppendContext → 追加到全局 task_context
    ↓
回复返回微信
```

### 用户在 CLI 查任务进度

```
CLI 输入："查进度"
    ↓
IdentityMapper.Resolve("cli", hostname) → unified_uid
    ↓
IntentClassifier.Classify → IntentTask
    ↓
TaskManager.ListTasks(unified_uid) → 返回所有全局任务
    ↓
格式化输出，返回 CLI
```

---

## 七、已有基础（2026-04-15 实现）

- [x] `trajectories` 表加 `channel` 字段，历史按渠道隔离
- [x] `IdentityMapper` — platform+platformID → unified_uid 映射
- [x] `TaskManager` — 全局任务表 + current_task 指针 + task_context
- [x] `IntentClassifier` — 轻量级意图分流（规则判断，不调用 LLM）
- [x] `StorageProvider` / `MemoryManager` / `ContextEngine` / `Agent` 签名加 `channel` 参数

---

## 八、后续接入说明

### 微信接入

在 `gateway/` 下新增微信适配器：

```go
// gateway/wechat/adapter.go
type WeChatAdapter struct {
    identity *identity.IdentityMapper
    tasks   *task.Manager
    intent  *router.IntentClassifier
    agent   *kernel.Agent
}

func (a *WeChatAdapter) HandleMessage(openid, content string) string {
    uid, _ := a.identity.EnsureBound(context.Background(), "wechat", openid)

    intent := a.intent.Classify(content)
    switch intent {
    case router.IntentContinue:
        task, msgs, _ := a.tasks.GetCurrentTask(context.Background(), uid)
        // ... 构造"继续"prompt
    case router.IntentTask:
        taskID, _ := a.tasks.CreateTask(context.Background(), uid, content)
        // ...
    case router.IntentCasual:
        return a.directAnswer(content)
    }

    resp, _ := a.agent.RunConversation(ctx, uid, "wechat", prompt)
    return resp
}
```

### 钉钉接入

同理，新增 `gateway/dingtalk/adapter.go`，使用相同的 `identity`、`task`、`agent`。

### Web 端接入

新增 `gateway/web/handler.go`，channel 填 `"web"`。

---

## 九、核心能力不受影响

以下模块**完全未改动**，保持原有逻辑：

| 模块 | 状态 |
|------|------|
| Agent 推理循环 | 不变 |
| ReflectionEngine 反思与技能归档 | 不变 |
| Dispatcher 工具调度 | 不变 |
| Middleware 链（Auth / Approval / RateLimit） | 不变 |
| SkillLoader 动态技能加载 | 不变 |
| ContextEngine Token 管理 | 仅加 channel 参数，不改截断逻辑 |

---

## 十、联系方式

项目：`/work/selfmind/`
二进制：`selfmind-binary`（18MB 静态编译，无 CGO 依赖）

如有问题，直接在项目 issue 中提。
