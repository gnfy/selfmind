# 执行并发与多任务调度 — 设计方案

本文覆盖从"单任务不挂"到"多任务并行 + 后台执行 + 远程 Runner"的完整演进。
`docs/worker-pool-design.md` 仍然拥有 worker pool 本身的拓扑与安全审计；本文接在
它的 §3 之后，负责调度策略、后台 run 分级、资源锁、事件面与执行信封。

触发事件：2026-08-03 一次 `patch` 工具调用卡死 → `/stop` 无效 → 整个 daemon 的
agent 循环停摆 17 分钟。根因不是 patch 算法本身，而是**三层缺陷叠加**，见 §1。

---

## 1. 现状盘点（已核对代码，不是推测）

### 1.1 已经具备的地基

| 能力 | 位置 | 状态 |
| --- | --- | --- |
| N worker 并发上界 + 按 key 串行 | `internal/runpool/pool.go` | ✅ 已实现，race 测试覆盖 |
| 每 worker 独立 Agent（各自 `runMu`） | `internal/app/workerpool.go` | ✅ `InitAgent`+`InitTools` 各一份 |
| 不同 workspace 并行 | `router/gateway.go:workspaceSerialKey` | ✅ |
| **同 workspace 只读并行** | 同上，`strategy.MayWriteWorkspace()==false` 返回空 key | ✅ **已完成，无需再排期** |
| 同 workspace 写串行 | 同上 | ✅ |
| `SELFMIND_WORKERS` 1–16，默认 1 | `app/workerpool.go:workerCount` | ✅ 默认 1 = 旧路径字节等同 |
| 停滞看门狗 | `runpool/watchdog.go` + `router/events.go:59` | ⚠️ 已接线但**打不到执行体**，见 1.2 |
| 进程执行信封（带 ctx） | `tools/execution_engine.go:ExecutionRequest` | ✅ 已含 RunID/CallID/LeaseID/Roots/Sandbox/Timeout/Durable |
| daemon 自有执行（无活跃 run） | `tools.DurableExecutionScope` | ✅ external watch 已在用 |
| 重启恢复 queued/watch | `httpapi/run_recovery.go`、`RequeueSystemQueued` | ✅ |

### 1.2 三层缺陷（8-03 事故的完整链路）

**① 工具执行体不可取消。** `internal/tools/tool.go:15`
`Execute(args map[string]interface{}) (string, error)` —— **没有 `context.Context`**。
纯计算循环（本次是 `patch.go:299-313` 的 LCS 兜底，n²/2 个子段各做一次 `strings.Join`
+ LCS）无法被中断。已有的 `runpool.WithWatchdog` 会 `cancel(ctx)`，但 ctx 进不到
执行体，**看门狗形同虚设**。这是"机制存在但无效"，不是"机制缺失"。

**② `/stop` 是记账操作，不是终止操作。** 取消路径把 control.db 写成 `cancelled`
并发出 `run.outcome`，但 goroutine 照跑。**数据库状态与进程实况在这一刻分叉**，此后
所有诊断面（`/status`、`/tasks`、doctor）读到的都是假象。

**③ 卡死的资源泄漏范围大于一个 worker。** `runpool/pool.go:46-60` 的获取顺序是
key 锁 → worker slot → `fn` 内 `ag := <-g.agents`，三者**全部由 defer 在 `fn` 返回时
释放**。`fn` 永不返回 ⇒ agent 不归还 + semaphore 不释放 + **key 锁永久不释放**。

> 后果放大：`workers=4` 时单次卡死不会全局停摆，但**该 workspace 从此永久无法执行
> 任何写任务**，且外部无任何信号。这比全局停摆更难诊断，必须在开并发**之前**修掉。

单 worker（今天）时，缺陷 ③ 直接升级为整机停摆：`kernel/agent.go:600` 的
`a.runMu.Lock()` 在 `RunConversation` 开头、`defer Unlock`，Agent 全局共享 ⇒ CLI /
微信 / cron / HTTP 所有端一起排在这把锁后面，对外只表现为 `/status` 永远 "running"。

### 1.3 队列与调度的表达能力缺口

- `task_queue` **没有 class / priority / not_before 列**（schema：id, tenant_id,
  person_id, channel, platform, platform_user_id, content, approval_mode,
  workspace_id, status, created_at, restarts, task_id, idempotency_key, run_id）。
- 出队是**严格 FIFO**：`control/queue.go:410` `ORDER BY created_at ASC, rowid ASC`。
- 用户溢出任务、watcher finalization、cron 触发**全部混在这一张表**。
- `maintenance_jobs` 是独立表（✅ 已天然隔离），但没有"不得抢占前台"的显式约束。

⇒ "三类工作分级"不只是策略问题，**schema 层就没有表达能力**，必须先迁移。

### 1.4 每人一个 active run 的实现位置

`httpapi/run_coordinator.go:61` `beginActive` 基于 `c.active map[personID]*activeRun`，
一人一槽。**正因为唯一，steering 与审批天然无歧义**。放开成 foreground/background
时，所有读 `active[personID]` 的地方都要重读：steering 投递、`/status`、
`enqueueBehindActive`、`endActive` 后的 drain、run recovery。这是本计划风险最高的一处。

### 1.5 其它必须同批处理的债

- **provider 并发与 429**：`worker-pool-design.md` §3 的 "per-provider 上限"标注
  deferred。单次调用均 37k token（7-31 复盘），3 worker 并发会按倍数放大限流与成本。
  **开并发的前置条件，不是可选项。**
- **control.db 写放大**：8-03 单个 run 20 分钟写 3653 条事件，其中 3504 条是
  `tool.output` 行级事件；库 224MB / 19.7 万事件、无保留策略；控制库
  `SetMaxOpenConns(1)`（`control/store.go:125`）。并发会把它变成新瓶颈。
- **`runpool.Run` 只接受单一 key**：多资源锁（workspace + build trigger + 部署目标）
  需要**有序多锁获取**，否则两个 run 各持一半即死锁。这是 API 变更。
- **`worker-pool-design.md` §5 遗留 TODO**：工具后端 dispatch 与后台进程注册表审计。
  实测 `ProcessRegistryForArgs` 已按 tenant 分区 + `sync.Map`/mutex 保护，形状正确，
  但需要一次确认性 pass 才能关闭。

---

## 2. 目标模型

```
Gateway            接收、持久化、身份、事件同步
   ↓
Run Scheduler      分级、并发上界、workspace / resource 锁、前台保留
   ↓
Worker Pool        每 worker 一次一个 Agent（保留各自 runMu，不拆）
   ↓
Tool Executor      可取消、可观测、可远程化（单一执行信封）
```

**执行单位是 Run，不是共享的全局 Agent。** 本轮**不移除** Agent 内部的 `runMu`——
"多个独立单线程 Agent worker"改动更小，且与未来 Runner 的进程模型一致。

### 2.1 三类工作，三种资源语义

| 类型 | 占 worker | 队列 | 说明 |
| --- | --- | --- | --- |
| **Foreground run** | 是 | 前台，最高优先 | 用户在 CLI/IM 当前等待的回合 |
| **Background run** | 是 | 后台，受 per-person 上限 | 仍调模型和工具（后台分析、生成 PR） |
| **External watcher** | **否** | 独立 poller | 只轮询外部状态；完成后**提交一个 finalization run** |
| **Maintenance** | 否（独立 job） | `maintenance_jobs` | 标签、记忆、skill review；最低优先，永不抢占 |

External watcher 走 `tools.DurableExecutionScope`，已经不占 agent worker ✅ ——
本计划只需保证 finalization run 进入正确的优先级档位。

### 2.2 调度优先级（从高到低）

1. 用户审批与 steering（永远不排队，直达目标 run）
2. 当前 CLI / IM 前台任务
3. watcher finalization
4. 用户明确提交的后台任务（`send --async`）
5. cron
6. maintenance（记忆、标签、skill review）

**至少保留一个 worker 给前台**，避免后台占满后用户连交互都变迟钝。

---

## 3. 分阶段方案

### 阶段 0 — Patch 与取消可靠性（P0，独立可发）

修复今天这次事故本身，**不依赖任何并发改动**。

- `patch.go` LCS 兜底加硬上界（文件行数 / 子段数 / wall-clock 三选一），超限直接返回
  "hunk not found"。**倾向直接删除整个兜底**：命中条件 `score(行数) >
  len(searchPattern)/3(字节数)` 量纲就是错的，命中后会把 `bestStart..bestEnd` 整段
  替换为 replLines，即"悄悄删掉文件一大块"，期望收益为负。
- `validateOperations:420/430` 对同一 hunk 重复调用 `fuzzyFindAndReplace`，去重。
- `fuzzyEqual` 内的 `regexp.MustCompile` 提到包级；`parseV4APatch` 每行编译 4 条正则
  同样提出循环。
- 修 `fuzzyFindAndReplace:288` 的 `append(lines[:matchStart], replLines...)` 原地改写
  别名 bug（且匹配后未 `break`）。
- **回归测试**：用 8-03 的真实输入（`cw2-aws-run.md` 2297 行 + 上下文对不上的 hunk）
  作为用例，断言毫秒级返回 "hunk not found"。实测未修复版本外推需 ~8.6 分钟；
  `gcp-run.md`（3705 行）约 36 分钟。

**验收**：同样的 patch 输入，返回时间 < 1s，且不修改任何文件。

---

### 阶段 1 — 统一工具执行信封（P0，Runner 的地基提前到这里）

> 从原方案的最后一步提前到第二步：**真实取消本来就需要它**，而它就是远程 Runner
> 的协议本体。分两次做没有意义。

- `Tool.Execute` 接收 `context.Context`：
  `Execute(ctx context.Context, args map[string]interface{}) (string, error)`。
  纯计算工具（patch/read/搜索）在循环内周期性检查 `ctx.Err()`。
- **两条执行路径并成一条**：现在 exec 形工具走 `tools.Execute(ctx, ExecutionRequest,
  args)`（已带 ctx 与完整信封），其余走无 ctx 的 `Tool.Execute`。统一后**所有**工具
  调用都携带同一个信封：

  ```
  run_id / call_id / tool / args
  environment_lease_id / execution_scope / credential_refs
  deadline / resource_lock_keys
  ```

- 执行状态机固定为：`accepted → started → progress → completed | cancelled | uncertain`。
  本地 worker、后台 worker、未来 VPC/云 Runner **共用这一套**。
- `EnvironmentLease` 与认证**绑定 run，不绑定 CLI / 微信 / cron 来源**（现状已如此，
  写成不变量固化）。
- 这一步落地后，**现有 `runpool.WithWatchdog` 立即生效**，不需要新建超时机制。

**验收**：`/stop` 能在宽限期内真实终止一个纯计算工具；看门狗能杀掉无进展的 run。

---

### 阶段 1.5 — 队列分级 schema 与事件保留（P0，后续每一步都压在它上面）

- `task_queue` 增列：`class`（foreground / background / watch_finalization / cron）、
  `priority`（整数，来自 §2.2）、`not_before`（延迟调度）。带迁移，存量行按
  `idempotency_key` 前缀回填（`external-watch:` → watch_finalization，
  `steering:` → foreground，其余按来源）。
- 出队从 `ORDER BY created_at` 改为 `ORDER BY priority ASC, created_at ASC, rowid ASC`，
  并支持 `not_before <= now`。
- **工具输出有界化 + 事件保留**同批做：`tool.output` 行级事件改为有界摘要 + artifact
  引用；control.db 加保留策略（当前 224MB / 19.7 万事件 / 无保留）。
- 保留 `restarts` 幂等语义与重启恢复路径不变。

**验收**：迁移后重启，queued / background / watch 状态可恢复且顺序符合优先级；
单个 run 的事件量相比 8-03 那次下降一个数量级。

---

### 阶段 2 — Worker 生命周期与隔离（P0）

- **归还条件**：worker 只有在**执行 goroutine 真正退出**后才归还 pool。取消后进入
  宽限期（建议 30s，可配）。
- **Quarantine**：超过宽限期未退出的 worker 标记 `quarantined`，不再接任务。
  **关键：quarantine 必须同时释放/转移它持有的 `runpool` key 锁**——否则该 workspace
  永久锁死（§1.2 ③）。做法是把 key 锁的归属从"defer 释放"改为"由 worker 生命周期
  状态机持有"，quarantine 时显式转移为 tombstone 并允许新 run 获取（同时在
  `/status` 标注该 workspace 曾有未回收的执行体）。
- **隔离**：一个 worker 卡住，其它 worker 继续工作；同 workspace 的后续写任务在
  quarantine 完成前排队并**可见地**报告原因。
- **可观测**：worker 状态进入 `/status`：

  ```
  workers: 3 total · 1 busy · 1 queued · 1 quarantined
  run_xxx: patch · no progress 18s
  ws_08e6ced0: write lock held by quarantined worker (since 10:09:12)
  ```

- **等锁必须可见**：8-03 的新 run 阻塞在 `runMu` 上、外部只看到 "running"。
  任何"在等待调度/等待锁"的状态都要出现在 `/status` 与 `/diag`。
- 关闭 `worker-pool-design.md` §5 遗留 TODO（工具后端 dispatch + 后台进程注册表确认
  性 pass；实测形状已正确）。

**验收**：一个 patch 卡住，另一个 workspace 的任务仍能开始；卡住的 workspace 在
quarantine 后可恢复接受新写任务；`/status` 能说清楚"谁卡了、卡多久、影响谁"。

---

### 阶段 3 — 多级调度与后台 run（P1，风险最高）

per-person 规则从"每人最多一个 active run"改为：

- 每人最多 **一个 foreground run**
- 每人允许 **有限数量 background run**
- **每个 task 最多一个 active run**（新增，防止同任务并发写）

```yaml
execution:
  workers: 3
  foreground_reserved: 1
  max_background_per_person: 2
```

高级项默认按机器资源自动选择并隐藏，避免继续增加配置负担。

实现要点：

- `runpool` 的单一 `sem` 拆成"保留槽 + 通用槽"，前台可抢占保留槽，后台只能用通用槽。
- `c.active` 从 `map[personID]*activeRun` 改为 `map[personID]*personRuns{fg, bg[]}`，
  并**逐一重读**所有调用点：steering 投递、`/status`、`enqueueBehindActive`、
  `endActive` 后 drain、run recovery、`activeRunIDs`。
- **不变量（硬性）**：审批与 steering 永远解析到唯一 run。
  - 显式带 run id → 直接路由。
  - 只有一个 run 在等待用户 → 自动关联。
  - 多个候选 → **明确询问用户选择，绝不猜**。
  - 该不变量对 CLI（`selfmind approve` / `stop` 不带 id）与 IM 同等适用。
- **provider 并发上限 + 429 退避**在这一步成为前置条件（见 §1.5）：按 provider 路由
  限流，一个 provider 被限不得阻塞其它路由。

**验收**：前台任务提交后台后 CLI 可立即继续新工作；provider 429 时只限制对应
provider；后台任务永远抢不到最后一个前台保留槽。

---

### 阶段 4 — Workspace 与资源并发（P1）

- 不同 workspace：并行读写。
- 同 workspace 只读：并行（**已实现**）。
- 同 workspace 写：默认串行。
- **Git 项目需要同仓并行写**：每个后台 task 创建独立 `git worktree`，完成后合并。
  - 该 run 的 `ExecutionScope` 可写根**必须指向 worktree**，不是 workspace 根——
    否则违反"可写视图由 ExecutionScope 派生、不得来自 cwd"的既有铁律。
  - worktree 生命周期要能被崩溃恢复（daemon 重启后清理孤儿 worktree）。
- **非 Git 目录**：继续 workspace 独占写锁。
- **云端副作用资源锁**：同一个 build trigger、同一个部署目标不得重复触发。
  - ⚠️ `runpool.Run` 目前只接受**单个 key**。多资源锁需要改成**有序多锁获取**
    （按 key 字典序排序后依次获取），否则两个 run 各持一半即死锁。这是 API 变更，
    需要独立的 race 测试。

**验收**：两个不同仓库的编码任务并行完成；同仓两个写任务不会互相覆盖；
同一部署目标不会被两个 run 同时触发。

---

### 阶段 5 — 统一事件面与 TUI attach（P1）

统一操作面：

```
selfmind send --async "任务"     # 已存在
selfmind runs                    # 新增
selfmind attach <run_id>         # 新增
selfmind stop <run_id>           # 从无参扩展
selfmind steer <run_id> "补充要求" # 从当前 run 扩展
```

- CLI **只实时渲染当前 attach 的 run**。其它后台 run 只出简洁通知：

  ```
  Background run run_7ab3 started.
  Watcher watch_4046 succeeded.
  Run run_7ab3 needs approval.
  ```

- **绝不把多个 run 的流式 delta 混进同一个 TUI transcript**（这是既有
  "origin-carrying run 渲染为结果行、不重放进度"规则的自然延伸）。
- 审批与澄清**永不被抑制**，无论 run 是否 attach。
- IM 简单回复的归属沿用 §3 的唯一性不变量。

**验收**：多个后台任务的输出不会混入当前 CLI；未 attach 的 run 需要审批时用户仍能
第一时间看到并回答。

---

### 阶段 6 — 远程 Runner（P2）

阶段 1 完成后这一步基本是"换实现"：

- 本地 worker、后台 worker、VPC Runner、云 Runner **共用同一个执行信封与状态机**。
- Runner 回传 `accepted → started → progress → completed | cancelled | uncertain`。
- `EnvironmentLease` 与凭据引用随 run 走；执行状态只存引用与策略，**不存原始凭据字节**
  （既有铁律，跨进程后更关键）。

---

## 4. 验收场景总表

| # | 场景 | 由哪个阶段保证 |
| --- | --- | --- |
| 1 | 一个 patch 卡住，另一个 workspace 的任务仍能开始 | 阶段 2 |
| 2 | 前台任务提交后台后，CLI 可立即继续新工作 | 阶段 3 |
| 3 | 两个不同仓库的编码任务可并行完成 | 阶段 3 + 4 |
| 4 | 同仓库两个写任务不会互相覆盖 | 阶段 4 |
| 5 | watcher 等待期间不占 worker | ✅ 已具备，阶段 1.5 保证优先级正确 |
| 6 | `/stop <run_id>` 能真实终止执行并释放 worker | 阶段 1 + 2 |
| 7 | 多个后台任务的输出不会混入当前 CLI | 阶段 5 |
| 8 | provider 429 时只限制对应 provider | 阶段 3（前置条件） |
| 9 | daemon 重启后 queued/background/watch 状态可恢复 | ✅ 部分已具备，阶段 1.5 迁移后重验 |
| 10 | **卡住的 workspace 能自愈，不永久锁死** | 阶段 2（新增，源自 §1.2 ③） |
| 11 | **审批/steering 在多 run 下永不误投** | 阶段 3（新增不变量） |

---

## 5. 执行顺序与开关策略

```
阶段 0  patch + 取消可靠性        ← 独立可发，先止血
阶段 1  统一执行信封（ctx 打通）   ← 让现有看门狗生效，同时是 Runner 地基
阶段 1.5 队列分级 schema + 事件保留 ← 带迁移，后续全部依赖
阶段 2  worker 生命周期与隔离      ← 开并发的最后前置条件
阶段 3  多级调度与后台 run         ← 风险最高，单独发
阶段 4  workspace / worktree 并发
阶段 5  统一事件与 TUI attach
阶段 6  远程 Runner
```

**开关策略**：`SELFMIND_WORKERS` **保持默认 1，直到阶段 2 落地**。首次放开取
`workers=2 / foreground_reserved=1`，跑一轮 soak + eval 后再提默认值。理由：阶段 2
之前，一次工具卡死会永久锁死一个 workspace 且无信号（§1.2 ③）——并发只会让这个
故障更隐蔽，不会更安全。

## 6. 文档归属

- 本文（新增）拥有：调度策略、后台 run 分级、资源锁、执行信封、Runner 演进。
- `docs/worker-pool-design.md` 继续拥有：worker pool 拓扑与并发安全审计；
  其 §3 应加一行指向本文。
- `docs/tool-safety.md` 需在阶段 1 同步：`Tool.Execute` 签名与取消语义。
- 阶段落地时在 `docs/STATUS.md` 各加一行状态；本文不记录进度。
