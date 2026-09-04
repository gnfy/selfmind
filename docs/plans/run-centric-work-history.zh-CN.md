# Run 为中心的工作历史方案（移除 Task）

> 生命周期：active  
> 日期：2026-09-04  
> 审批：项目所有者  
> 复审日期：2026-09-18  
> 取代：`task-capsule-work-history-redesign.zh-CN.md`（归档）、
> `threaded-work-history-redesign.zh-CN.md`（已归档）  
> 范围：Task/Thread 的移除、执行恢复关系、检索键、命令面、审批授权作用域。  
> 同版本交付：上下文经济（§9），因为延迟工具机制已经建好且处于关闭状态。

## 0. 一页读完

### 0.1 结论

Task 不再是领域对象。执行归 Run，历史归 person 级 Work Journal，理解用户归
Memory，做事能力归 Skill，待处理事项由控制对象派生。

删表是**最后一步**，不是第一步：先把 Task 今天实际承担的四处权威搬走，之后删除
只是收尾。

### 0.2 已定的决定

| # | 决定 |
| --- | --- |
| 1 | Task/Thread 最终完全移除；先搬权威，删表收尾 |
| 2 | 批次减法先行；加法（封存、分层摘要、后台整理）等真实使用证据才做 |
| 3 | 记忆管道不做工程改动，只投可见性；拒绝从行为推断偏好 |
| 4 | 审批授权作用域从 Task 换成 person + 操作类别 |
| 5 | 停驻审批的认领键从 thread_id 换成 person + fingerprint + 精确 Run |
| 6 | 检索写入按 `run_id`，读取按 resume 链分组 |
| 7 | **`/tasks` 与 `/task` 命令删除**；用户面只保留 `/status`、`/resume`、`/search` |
| 8 | **pin 与 archive 功能删除**；Run 级 dismissal 是唯一的隐藏手段 |
| 9 | 不预设产品判据，靠真实使用数据的周期性审计 |
| 10 | 迁移约束先放松；v11 是最后一个公开过的 schema，必须始终可升级 |
| 11 | 上下文经济与本方案**同版本交付**（§9）：机制已建好且关闭，先做不依赖用量证据的三项 |

### 0.3 事实依据（可核对）

**Task 没有挣到权威。**

| 观察 | 值 |
| --- | ---: |
| 当前库 Runs / Threads | 16 / 15 |
| 只有一个 Run 的 Thread | 14 / 15 |
| 重置前 7 天 Runs / Threads | 61 / 27 |
| 重置前只有一个 Run 的 Thread | 24 / 27 |
| 重置前精确续接边 | 3 / 61（约 5%） |
| 最大的一个 Thread | 32 个 Run、20 个频道、0 条续接边 |
| `task_references` | 0 |

最后两行是关键：唯一一个真正多 Run 的分组，恰恰是**错误**聚合了 32 个互不相关的
Run；而用户从未使用过 Task 引用。跨 Run 续接的真实发生率约 5%，这个比例足以证明
`resumes_run_id` 值得存在，远不足以支撑一个领域对象。

**改名成本。** `parent_run_id` / `ParentRunID`：21 个非测试 Go 文件、38 个 Go
文件、134 处、7 份文档、1 个 eval 用例。

**上下文构成**（189 次真实 provider 调用的平均值，来自
`provider.call.context_breakdown` 事件）：

| 切片 | 平均 token | 占比 |
| --- | ---: | ---: |
| `tool_schemas`（41 个工具） | 10,845 | 29.3% |
| `current_tool_results` | 9,950 | 26.9% |
| `history` | 9,630 | 26.0% |
| `task_runtime` | 1,197 | 3.2% |
| `stable_system` | 1,099 | 3.0% |
| `recall` / `memory` / `skill` | ~300 / 0 / 0 | <1% |
| **`estimated_total`** | **36,978**（峰值 54,130） | |

三个大头量级相当，**移除 Task 只触及 3.2%**。所以移除 Task 的收益是降低结构复杂度
和错误率，不是省 token；上下文成本是一条独立的、更大的线，见 §9。

**记忆管道是通的**（2026-09-03 探针，走真实生产路径，未使用 `/remember`）：
自然语言陈述一句持久偏好后约 2 分钟，`facts` 0→1、`canonical_memories` 0→1；下一
回合该记忆真正注入 prompt 并改变了回答（`last_accessed_at` 更新、`occurrences`
2→3、`confidence` 0.6→0.8）。此前 66 回合 0 条记忆不是缺陷，是那些回合（发布、
核查、排障）确实没有陈述过偏好。**"越用越懂用户"不需要工程改动，需要使用。**

**Skill 的 0 是正确行为**：16 条 workflow observation 凑不出"三次独立且验证成功
的可比工作单元"这个门槛。

**Work Spine 已经是不可变日志。** 全仓库对 `trajectories` 没有任何 UPDATE 或
DELETE，历史重置也不碰它。但默认只回放 16 条（读窗 32 条），更老的条目根本不读，
且完全没有封存、分段、摘要的概念。

### 0.4 明确不做

- 不再用任何名字（Thread、Capsule、Topic）重建一个必须被正确维护的语义容器。
- 不在 ingress 用模型判断"任务还是闲聊"，不恢复 `fast_classifier` 的连续性权威。
- 不新增第二个后台 extractor。
- 不让"存下来的推断"获得提醒权威（见 §4 不变量 3）。
- 不为 Completion Contract 新增 `finish_run` 字段。
- 不新建工具目录分层子系统——机制已存在，只做解锁与选名单（§9）。

## 1. 目标模型

    Person
      ├─ endpoint-local Conversations   原始聊天，仅本端展示，不跨端复制
      │
      ├─ Runs                           执行、Plan、工具、审批、watcher、证据、恢复
      │    └─ resumes_run_id            指向被精确恢复的那一个 Run
      │
      ├─ Work Journal                   每个 Main 回合一条不可变记录（今天的 Work Spine）
      │
      ├─ Memory                         稳定偏好、习惯、纠正
      │
      └─ Skills                         从重复且验证成功的 Work Unit 学到的方法

    Attention = 从 active Run、pending 审批、pending 澄清、live watcher
                和满足条件的可恢复 Run 派生

核心对象五个：**Run**（执行事实）、**Work Entry**（一次人机回合的记录）、
**Memory**（长期理解用户）、**Skill**（可复用方法）、**Conversation**（端点本地
聊天）。Attention 是即时派生的视图，不是对象。

### 1.1 Work Entry 就是今天的 Work Spine 条目

它不是新概念，是已经在跑的东西。存储：`memory.db` 的
`trajectories(id, content, channel, created_at)`，键为常量 `"spine"`，按 person
分库（`~/.selfmind/data/person_<id>/memory.db`）。每个 Main 回合追加一行，`content`
是这个 JSON（`internal/kernel/context_composer.go` 的 `spineEntry`）：

    {"kind":"spine.turn.v1",
     "user":      "<用户原话，已剥掉网关装饰块>",
     "assistant": "<本回合最终答复>",
     "files":     ["<从本回合工具调用参数确定性提取的路径>"],
     "source":    "cron",                  // 仅非交互来源
     "task_id":   "task_..."}               // 仅标签溯源，不影响模型看到什么

**刻意不包含**工具调用、工具结果和 system prompt——它们留在 run events 里，由召回
按需取。源码注释写得很直接：spine 绝不能变成工具日志。

实测（所有者本地库）：68 条，2026-08-26 至 2026-09-03，单条 160–1,344 字节。写入端
上限 user 2,400 / assistant 4,000 字节，读取端 1,200 / 1,600 字节，默认回放最近
**16** 条为交替的 user/assistant 消息。全仓库对该表**没有任何 UPDATE 或 DELETE**，
连历史重置也不碰它——不可变是已经成立的事实，不是待建的性质。

所以本方案对它只做两件事：Task 移除后 `task_id` 字段失去意义（Batch 3 清理），
以及 Batch 4 在其上增加封存与分层摘要（门控）。**条目形状与不可变性不需要改动。**

"任务"作为自然语言概念继续存在，但不再对应任何数据库实体。

## 2. Task 今天实际承担的权威

这一节是本方案的核心工程。方案的前身文档把这四条当成"已经不成立的约束"来写，
实际代码里四条都成立——它们是**待办**，不是现状。

| # | 权威 | 现状 | 搬到哪里 |
| --- | --- | --- | --- |
| 1 | **权限** | 审批授权 `scope_kind='task'`、`scope_id` 为 task id，刻意跨同一 Task 的多个 Run 存活（`internal/control/approval_grants.go`） | **person + 操作类别**（例如"这个工作区里的只读 gcloud 查询"）。自证，不依赖归组判断 |
| 2 | **审批续接** | `ClaimApprovalResumeAuthorization` 按 `(tenant, person, thread_id, fingerprint)` 认领停驻审批（`internal/control/approvals.go`） | **person + fingerprint + 精确 Run**，不经任何容器 |
| 3 | **Attention 可见性** | Attention 与可恢复列表读 `t.visibility`（排除 hidden/archived）并按 `t.pinned` 排序（`internal/control/work_timeline.go`、`run_continuity.go`） | **删除**。pin 与 archive 功能不保留；Run 级 `attention_dismissed_at` 是唯一隐藏手段，排序纯派生 |
| 4 | **恢复的执行范围** | 重新排队的工作从 **Task** 取 `WorkspaceID`（`automatic_run_recovery.go`、`run_recovery.go`、`approval_resolver.go`、`context_selector.go`） | **Run**。`runs.workspace_id` 已存在，执行根本来就取自 Run |
| 5 | **事件归属** | `task_events` 没有 tenant/person 列，归属靠 `JOIN threads` 推导——一个展示分组决定"这些事件是谁的" | **Run**（`threads` 保留为历史行兜底）。实施时才发现，所以本节标题的"四处"是当时的认知，不是完整清单 |

第 1 条要特别说明为什么必须换：Task 能当授权作用域，靠的是"这些 Run 是一件事"
这个判断，而那个错聚 32 个无关 Run 的分组证明该判断不可靠——意味着一次授权可能被
31 个无关 Run 继承。这是本方案唯一**安全相关**的改动。

## 3. 检索：写入按 Run，读取按链分组

**现状。** FTS session 的键是 `"task:" + TaskID`（`internal/kernel/agent.go`），
而 `sessions_fts` 与 `session_messages` **既没有 run_id 也没有 person 列**（租户
靠数据库分区）。没有 Task 绑定时退化为首条用户消息的内容哈希。今天没有任何可替代
的稳定分组 id 被物化过：`runs.work_key` 是外部工单号，不是分组键；`resumes_run_id`
链的根也没有成为列。

**目标。** 写入时按 `run_id` 键；需要"连贯"时在**读取**时顺着 `resumes_run_id`
往上找链根并分组。

**为什么不物化链根列。** 同域 in-turn 认领发生在回合中途，那时 session 键已经写
好了，物化列会带来中途改键的问题。而链长最多几跳、跨 Run 续接率约 5%，读取时计算
的成本可以忽略。

**性质上的区别。** Task 声称提供的"连贯分组"，这里用一条自证的边提供，而不是用一
个判断。判断会错，边不会。

## 4. 不变量

1. **Run 掌握执行事实。** 没有任何非 Run 对象持有 running / waiting / blocked /
   done。
2. **精确恢复不可猜测。** `resumes_run_id` 只来自显式 `/resume`、结构化
   approval/clarify/reply、watcher 结构化最终化、daemon 恢复，以及 Main 在正常
   turn 内经网关校验的精确选择。搜索命中、语义相似、同工单号、同工作区、进度查询
   都不写。
3. **推断不得获得提醒权威。** Attention 的输入必须全部是自证事实：正在跑的 Run、
   真实存在的审批行、真实存活的 watcher、Run 自己的终态。任何"我们认为还没完"的
   持久化推断都不进入这个集合。这条是付过代价的：v11 之前 Task 用一个持久化的
   `in_progress` 表达"还没完"，结果是 13 个永不关闭的 open task 和 23 个未认领
   Run。
4. **展示不改变权限。** 没有任何展示或分组字段可以决定 workspace、
   ExecutionScope、approval 或 tool grant。
5. **前台只有一次 Main 理解。** 不在 ingress 增加分类模型调用。
6. **活跃输入默认 steer。** user-origin 输入交给活跃 Main；daemon-origin 文本不
   通过自然语言 steer。
7. **跨端共享结构化状态，不共享原始 transcript。**
8. **后台维护只有一条冻结管道。**
9. **Memory 只收稳定偏好。** 所有 Main 回合都进历史，但只有明确陈述的持久偏好、
   习惯和纠正进 Memory。不从行为推断。
10. **迁移不静默丢数据。** v11 必须始终可升级。

## 5. 多端同步（不依赖 Task）

**有 active Run。** 任一端点的 user-origin 输入持久化进 steer mailbox，Main 在
安全检查点决定：补充当前目标、改 Plan、增加 Work Unit，或排队为新的 root Run。
即使信息对当前步骤无用也不丢弃。

**没有 active Run。** 直接创建 root Run，给 Main：最近 Work Journal、当前
Attention、相关历史命中、Memory。Main 若发现是在继续旧工作，读取旧 Run 并请求
精确恢复；只有真的恢复执行状态时才写 `resumes_run_id`。参考过去、问进度、做类似
修改都不写。

**关闭与恢复。** 用户不需要关闭任何东西：Run 完成后自然退出 Attention；pending
审批、澄清、watcher 仍然显示；`/resume` 恢复 Run；`/status` 显示 active Run 或真实
开放条件；想隐藏提醒时用 dismiss。

## 6. 命令面（净减少）

| 命令 | 变化 |
| --- | --- |
| `/status` | 保留（网关）。active Run 或最强 Attention。**别名 `/task status` 一并删除** |
| `/resume` | 保留（网关）。裸 `/resume` 就是 Attention 列表；参数收窄为 `n` 或 `run_id`，不再接受 task_id |
| `/search` | 保留，但**需要提升为网关命令**，见下 |
| `/events` | 保留，摘要从"当前 task 的事件"改为"当前 Run 的事件" |
| `/diag tasks` | **删除**。它今天直连 `threads` 表统计，没有继任者 |
| `/tasks` | **删除**（含 open/done/archived/all 以及 `search <keyword>`） |
| `/task` | **删除**（含 runs / rename / pin / unpin / complete / archive / merge / references 全部子命令） |

`pin`、`archive`、`complete` 随列表一起消失——它们的服务对象就是那个列表。

**一个必须一起解决的缺口。** `/tasks search <keyword>` 是网关命令，IM 能用；而
`/search` 今天是 **TUI 本地命令**（`Scope: Local`）。如果只删不补，IM 端会**彻底
失去历史检索**。所以 Batch 3 必须把 `/search` 的历史检索部分提升为网关命令，
否则这一步是功能倒退而不是简化。

补完之后覆盖是完整的：现在要处理什么看 `/status`，能继续什么看裸 `/resume`，
找历史用 `/search`，三者都在两端可用。

命令元数据仍然只有一个来源（`internal/gateway/command`），TUI 不维护第二套目录。

## 7. 已知限制（发布时明确写出，不作为待办）

Work Journal 的封存与分层摘要属于"加法"批次，要等真实使用证据，所以 Phase 1 会
带着这个限制发布：

> 默认上下文只回放最近 16 条 Work Journal 条目。更老的历史仍然完整保存并且可以
> 通过检索找到，但不会被自动回放进 prompt。

这不是缺陷描述的委婉说法，而是一个当前就成立的事实：今天就是 16 条。把它写成限制
而不是待办，是为了不让它变成又一次架构重构的理由。

## 8. 批次

### 实施进度（2026-09-04）

| 批次 | 状态 |
| --- | --- |
| C1 解锁延迟机制 | **已完成** |
| Batch 1 精确执行关系改名（schema v12） | **已完成** |
| Batch 2 四处权威搬离 Task（schema v13） | **已完成** |
| Batch 3b C2 延迟名单 / C3 回合级结果预算 | **已完成** |
| Batch 3 检索换键、删命令、`/search` 提升、Run 可无 Task | **大部分完成**，见下 |
| Batch 5 store 重新键控 + 删表 | **进行中**，见 §8.2 |
| Batch 4 / C4 | 门控中，按计划等证据 |

Batch 1 途中发现并修掉一个只有真实 v11 库才能暴露的缺陷：形状采纳分支会把**全部**
迁移标记为已应用却不执行，于是每个已发布的 v11 安装都会静默跳过 v12 及以后的每一步
（`threadedWorkHistorySchemaVersion` 现在钉死在检测器识别的版本）。已发布 schema
fixture 与变异验证都在 `internal/control/released_v11_upgrade_test.go`。

Batch 3 已完成：`/search` 提升为网关命令（补上删除 `/tasks search` 会留下的 IM 检索缺口）、
FTS 会话键从 `task:` 换成 `run:` 并在读取时按 resume 链分组、`/tasks` 与 `/task` 与
`/diag tasks` 删除、裸 `/resume` 接管列表与序号快照、事件归属从 Thread 改为由 Run 推导、
`startRun` 改为接受 `RunOwner`，无 Thread 的 Run 已能完整执行/停驻/成为 Attention/被恢复
（`TestThreadlessRunIsCompleteWork`）。

Batch 3 未完成的一步：**让普通 root 消息真的不再创建 Thread 行**。它不能在这一批做完，
原因见 §8.1——批次边界原先画错了。

途中修掉一个先于本次改动就存在的缺陷：resume 边的竞态兜底匹配的是索引名，而 SQLite 的
唯一约束错误报的是**列名**，所以竞态失败方从来拿不到 `ErrResumeTargetClaimed`，只会拿到
一个原始约束错误。普通测试看不到它，因为 `validateResumeClaimTx` 会先拦住任何已能看到
赢家的快照。

Batch 2 的实际结论比预期干净：Task 从未真正获得权限权威——服务端选项集早已不提供
`task` 作用域，`/approve n task` 今天就会被拒，25 条历史授权全部过期。所以第 1 条是
删除而非搬迁，`person + pattern_key` 已经就是"person + 操作类别"。附带一处安全性
提升：LLM 裁判的自动批准从写出持久 task 授权改为 run 级瞬时授权。


分界线只有一条：**这一批引入新的失败模式吗？** 减法降低出错面，立刻做；加法每一个
都新增一条可能失败的路径，等真实使用证据。

**这条分界线漏了一个轴：可逆性。** 加法/减法说的是"会不会多一条出错路径"，而删表是
减法却不可逆——错误的减法通常只要 revert，错误的删表丢的是数据。

**所有者 2026-09-04 的裁定：这个阶段不要求这种安全级别。** 丢一些数据是可以接受的，
只要不影响两条核心线：**完成任务的质量**和**跨端连续性**。"越用越懂"的机制已经验证
正确，数据可以重新积累。旧数据同理——**能改对就改对，改不动就丢弃**，因为前面那版
设计本身就是错的，把错误的分组精确迁移过来没有价值。

因此删表不需要额外的门，只需要迁移前的常规备份。风险预算花在质量与跨端上，不花在
历史保真上。

### Batch 1（减法）精确执行关系改名

- **做什么**：`parent_run_id` → `resumes_run_id`，含索引名、错误类型、事件 payload
  和校验函数名；把校验、claim、correction、transfer 收敛进一个 RunContinuity 模块。
- **为什么**：`parent` 暗示主题层级或一般因果，让"相关"和"恢复"共用一条路径。
- **怎么做**：先把现有 `ClaimInteractionContinuation` 的校验搬进模块，再改名——
  不要一边搬一边改语义。持久 schema 与新事件不双写，只保留升级迁移与历史事件的
  只读兼容。
- **验收**：显式 resume、approval、clarify、watcher、restart、跨进程唯一认领全绿；
  新代码与新数据不再产生 `parent_run_id`。

### Batch 2（减法）四处权威搬离 Task

- **做什么**：§2 的四条，按 1 → 2 → 4 → 3 的顺序（先权限，最后可见性）。
- **为什么**：这四条是 Task 唯一真正承担的东西。搬完之后 Task 就只是一张没人依赖
  的表。
- **怎么做**：每一条独立提交、独立验收。第 3 条（删 pin/archive）放最后，因为它
  同时要删命令面。
- **验收**：审批授权跨 Run 生效但不跨无关工作；停驻审批仍能被正确的 Run 消费；
  恢复取到正确 workspace；Attention 不再读任何 Thread 列。

### Batch 3（减法）Run 与 Task 解耦、检索换键

- **做什么**：root Run 不再创建 Task；FTS session 键改为 `run_id`，读取时按链
  分组；`/tasks`、`/task`、`/diag tasks` 删除；`/search` 的历史检索提升为网关命令
  （见 §6 的缺口）。
- **为什么**：这一步真正消掉"过早归组"——一次尚未被理解的输入不再立刻获得语义容器。
- **怎么做**：先让 Attention 与 `/resume` 不读 Task，再让 Run 创建允许无 Task，
  最后换检索键并删命令。
- **验收**：没有任何 Task 的 Run 可以完整执行、等待、恢复并正确展示 Attention；
  旧的 `task:` 键会话仍可检索到；IM 端仍能检索历史（否则这一批是功能倒退）。

### 8.1 更正：删表必须在"Run 不再创建 Task"**之前**

方案原先把"root Run 不再创建 Task"放在 Batch 3，把删表放在 Batch 5、证据门之后。这个
顺序是错的，实测数据推翻了它。

**空的 thread id 不是"没有 Task"，是一个碰撞桶。** gateway 路径读 Task 的字段里
`task.ID` 占 335 处，其余全部字段加起来一百出头；而这些 ID 绝大多数被喂给按 thread 键
的 store 查询。这类查询共 **58 处，其中 50 处没有空值守卫**。让 Run 带空 thread id
运行，等于让所有无 Thread 的 Run 共用 `thread_id = ''`：

- `LatestIncompleteLoopCheckpoint` 的条件是 `tenant_id = ? AND thread_id = ? AND run_id <> ?`，
  **连 person 都不过滤**，会把另一个无 Thread Run 的恢复状态和快照恢复进来。
- `LatestHandoff`、`MaterializeRunFinalization`、`resolveOriginRunBlockersTx` 同理跨 Run 串。

这等于把"错聚 32 个无关 Run"以更糟的形式装回来——范围从一个线程里的 32 个扩大到全部
无 Thread 的 Run。同理，"让 `createRootTask` 返回一个未持久化的空 Task 值"这种 null
object 写法也不成立：它保留 51 个声称需要 `*control.Task` 的签名和 87 处恒为空的字段读，
正是 §22.7 拒绝的双轨。

**正确顺序：**

1. 把 58 处按 thread 键的 store 查询改成按 run/person 键。这是真正的工作量，每一处机械
   但必须逐个正确。
2. 改完之后 root Run 才可以不再创建 Task——那时已经没有任何东西读 thread 键。
3. 然后删表。

第 1 步不引入新的失败模式（它是把一个判断换成一个事实），所以按 §8 的分界线它属于减法，
不必等证据门；但它必须在一个干净的已提交基线上做，因为失败模式是跨 Run 状态串用。

**今天没有半成品状态。** 每个 Run 仍然都有 Thread，58 处查询全部正常工作；Task 已经失去
全部权威，`RunOwner` 的能力已就位并有测试证明（`TestThreadlessRunIsCompleteWork`），只是
没有生产路径去用它。

### 8.2 Batch 5 进度：先删无用功能，再重新键控

按 §8.1 的顺序执行。第一阶段是**删除**——没有可达调用方、或目标随 Task 一起消失的功能，
删掉比重新键控便宜得多：

| 删除 | 理由 |
| --- | --- |
| `MergeTasks` 及重复检测 | 零生产调用方；`/task merge` 已随 `/task` 删除 |
| `ReassignRun` | 零生产调用方；把 Run 在标签之间搬动是标签列表才需要的 |
| 整个 task-reference 功能 | reference 是用来**定位 Task** 的；Task 消失后它无处可指。实测 0 行、0 次使用，`/task references` 已删。含 control 四个文件、gateway 路由与标注钩子、post-run 分析器的 wire 与**提示词段落**、CLI 迁移命令 |
| `LatestIncompleteLoopCheckpoint` | 零生产调用方，而它的查询是 `tenant_id = ? AND thread_id = ?`——**连 person 都不过滤**，正是 §8.1 描述的最危险碰撞 |

第二阶段引入 `ResumeLineRunIDs`：**一条工作线 = 一条 resume 链的全部 Run**，从任一成员都能取到，
孤立 Run 是长度为一的线。这是"这个 Task 的那些 Run"的替代物——Task 用一个在工作开始时做出的
判断回答它，而这条边只在真的续接过时才记录，所以一条线可以不完整，但不会错。

按 thread 键的查询从 74 处降到 46 处；净删约 2,200 行。剩余部分继续按"每-Run 状态改按
work line 取 → 折叠 work_claim 的搬迁 → root Run 不建 Task → 删表"推进。

### Batch 3b（减法）上下文经济的免证据部分

- **做什么**：§9.3 的 C1（解锁内置工具可延迟）、C2（按类别延迟结构性稀有工具）、
  C3（回合级工具结果累计上限）。
- **为什么**：`tool_schemas` 与 `current_tool_results` 各占约 27–29%，是 `task_runtime`
  的八倍以上；而延迟机制已经建好且关闭，这一批是通电而不是新建。
- **怎么做**：C1 先单独提交并验证行为零变化（cohort 为空）；再逐个量出每个工具
  schema 的真实大小，据此评审 C2 的名单；C3 独立提交。
- **验收**：见 §9.4。C1 之后 `activated_deferred_tools` 仍为 0 且工具全部可用；
  C2 之后 `tool_schemas` 占比降到 20% 以下而工具选择正确率不降。

### 证据门

Batch 4 与 C4 之前，让 Batch 1–3b 的行为在真实使用中跑一段时间，用审计（而非预设
门槛）判断下一步做什么。不设固定天数，但 C4 有一个来自代码本身的硬门槛：**七天真实
用量报告**（§9.2）。

### Batch 4（加法，门控）Work Journal 封存与分层摘要

- **做什么**：超预算时把最旧的一组条目封存为不可变段落，后台生成保留原始条目 id
  和证据引用的摘要；热上下文移除旧条目，数据库不删除；历史再长时生成更高层摘要，
  但始终能回到原始段落。
- **为什么**：解除 §7 的 16 条限制。
- **张力已被指出并裁定**：这一批**增加**注入上下文，而 §9 整节在**减少**上下文。
  §7 的限制只影响**自动回放**——每一轮都同时写入 `trajectories` 和 `sessions_fts`，
  所以 16 条以外的历史一直是可检索的。所有者 2026-09-04 裁定：**只要增加是合理的、
  服务于核心方向，就可以做**，但必须同时回答两件事：如何**不超出上下文上限**，以及
  如何**避免上下文过多导致模型认知漂移**。
- **因此本批的验收多一条硬条件**：封存与分层摘要必须在一个**固定预算**内工作——注入
  的历史量不随会话长度单调增长，而是在预算内替换。摘要不是"再加一段"，是"用更少的
  字节表示同一段"。做不到这一点就不做这一批：更多不相关的旧上下文比没有更糟。
- **怎么做**：摘要是**可重建索引，不是事实来源**；唯一摘要不得反复覆盖自己。
- **验收**：多轮摘要之后仍能回查指定的旧决定、版本、文件和结果；摘要器失败时前台
  不受影响。

### Batch 5（收尾）删除 Task/Thread schema

- **做什么**：删表、删兼容读路径、删残留命名。
- **怎么做**：删除前确认没有新代码仍在产生旧字段。
- **验收**：完整 selfcheck、npm smoke、Linux/macOS、真实 CLI↔IM、restart 与本地
  安装验证通过。

## 9. 上下文经济（同一版本内交付）

上下文成本与 Task 移除同版本交付，因为**机制已经全部建好，只是关着**。

### 9.1 现成的机制

| 部件 | 位置 | 状态 |
| --- | --- | --- |
| `ToolExposureDeferred` 暴露态 | `internal/tools/tool.go:44` | 已实现 |
| `tool_search` 发现工具 | `internal/tools/task_protocol.go:565` | 已实现 |
| 激活路径（按 Work Unit 限定作用域） | `internal/kernel/tool_activation.go` | 已实现 |
| 用量报告已统计 `tool_search` 占比 | `internal/gateway/httpapi/daily_report.go:409` | 已实现 |
| 延迟名单 | `deferredExternalRolloutEnabled = false`、cohort 为**空** | 关闭 |
| 内置工具能否延迟 | `dispatcher.go:247` 要求 `Origin == External` | **不能** |

`activated_deferred_tools` 实测恒为 0，与上表一致。所以这不是一个要新建的子系统，
而是一个装好了没通电的开关。

### 9.2 已有的用量证据与它的不足

41 个工具在目录里，**只有 16 个被调用过**；6 个工具承担 90% 的调用
（`terminal` 98、`update_plan` 28、`batch_read` 24、`patch` 13、`read_file` 12、
`finish_run` 10，合计 185/205）。25 个工具一次也没被调用过。

但这份证据的窗口只有约 **25 小时 / 16 个 Run**（控制库被重置过），不是 7 天。代码
里那句注释把门槛写得很清楚：cohort"只能来自经过评审的七天用量报告；名字哈希是
确定性的，但不构成某个工具是冷的证据"。**按它自己的标准，用量驱动的延迟现在还没
被授权。**

### 9.3 因此分成两半

**不需要任何用量证据、进本版本（减法）：**

- **C1 解锁机制。** 去掉 `dispatcher.go:247` 的 external-only 限制，让内置工具进入
  可延迟范围。因为 cohort 仍为空，这一步**行为零变化**——它只是让开关可用。
- **C2 按类别延迟结构性稀有工具**（skill 生命周期管理、delegation、evolution 一类）。
  代码注释反对的是"用名字哈希猜冷热"，不是反对经人评审的类别判断；这些工具的稀有
  是结构性的，不需要用量统计来证明。
- **C3 工具结果预算。** `current_tool_results` 平均 9,950（26.9%），与工具目录一个
  量级，却完全不依赖用量证据——它是"一个回合内累积多少工具输出进模型"的策略问题。
  大输出转 artifact 的机制已经存在，需要的是回合级累计上限。

**必须过证据门（加法）：**

- **C4 用量驱动的 cohort。** 需要 7 天真实用量。这正好落在 §8 Batch 3 之后的证据门
  上：等减法批次跑满一段时间，用量报告自然就够了。

### 9.4 量级与验收

41 个工具、10,845 token，平均约 264 token/个。C2 若延迟 15 个左右，约省 4,000
token，即平均总量的约 11%。**注意：单个工具的 schema 大小差异很大，这个数字是用平均
值推的，实施前必须逐个量出真实大小**——本方案不把它当作已验证的结论。

验收：`tool_schemas` 的**绝对 token 数**下降，同时完成率、工具选择正确率、prefix
缓存稳定度不下降，`tool_search` 调用占比不失控。

**8K 里具体是什么**（2026-09-04 实测，26 个直接工具、21,321 字节线上载荷）：

| | 占比 |
| --- | ---: |
| 参数 schema | **67%** |
| 描述散文 | 23% |
| JSON 信封 | 10% |

所以杠杆不在"把描述写短"，而在**参数 schema**，而且高度集中：`watch_external`
(2,233) 与 `update_plan` (2,184) 两个就占整个载荷的 **20.7%**，都是参数撑起来的。
`selfmind` 元数据块不发给模型（`openAIToolDefinitions` 会重建），不计入。

打开这两个看，问题是**同一能力的多代并存**：

- `watch_external` 同时暴露 V1 正则（`success_pattern`/`failure_pattern`）、V2 目标态
  （`target_pattern` + 两个必需的 `terminal_*`）、V3 类型化适配器（`observation_adapter`），
  每个参数还带一段解释自己属于哪一代的散文。实测 3 次调用**全部只用 V1**，V2 和 V3
  一次没用过。
- `update_plan` 的 `related_task_id` 在 28 次调用中用了 **0 次**，且它随 Task 一起作废。

3 次调用不足以断言 V2/V3 在一般情况下无用，但**多代并存本身**与用量无关：它每回合都
逼模型在三套等价机制里挑一套，既是 token 成本，也是正确性风险（schema 自己规定 V1 与
V2 的参数不能混用）。这就是"越往后越难处理"在 schema 上的样子。

**已做（2026-09-04）。** 参考 codex 之后偏离了原提议，理由见下：

- **`watch_external`**：原提议是照 codex 做"工具级选版"。但 codex 的选版依据是回合级配置
  （`match turn_context.multi_agent_version`），而这里选哪一代取决于**被观察的外部系统**，
  只有模型知道，运行时不知道，所以没有可用的选择器。改为采用 codex 的另一半做法——
  **schema 只说值是什么，跨参数约束交给已经存在的校验器**（`ValidateExternalWatchSpec`
  本来就在强制它们并给出精确报错），那条"只能选一种完成模式"的规则移进**按工具裁剪的
  提示词**，一句话替代原先散落在五个参数里的解释。内部 V1/V2/V3 版本词汇也不再泄漏给
  模型——它从来不需要知道自己在对哪一代求值器说话。2,233 → 1,901 字节。
- **`update_plan`**：删掉 `related_task_id`（28 次调用 0 次使用，且随 Task 作废）。
  2,184 → 1,979 字节。**连带发现**：它是 `run_work_units.related_task_id` 的唯一来源，
  所以按 Task 的 skill binding 查询（`GetTaskSkillBinding`）现在已不可达。用到 Skill 时
  需要把它改键到 resume 链根。

合计 21,321 → 20,784 字节（−2.5%）。这个量级印证了前面的判断：**微观优化 schema 的
天花板就是个位数，真正管用的杠杆是把整个工具移出直接集合。**

**参考 codex 的结论**（2026-09-04 读源码）：

| | codex | 我们 |
| --- | --- | --- |
| 延迟内置工具 | **不做**，83 处内置 spec 全是 `defer_loading: None`，只延迟动态/MCP/扩展工具 | 延迟 11 个内置工具 |
| 提示词按可用工具裁剪 | 给 plan 段落写了专门的剥离函数 | 8 个分支自动逐工具裁剪，且传入的定义已剔除 deferred/hidden |
| 参数描述 | 平均 33 字符 | 平均 56 字符 |
| 多代能力 | 工具级选版，模型只看见一代 | 参数级并存（已按上文缓解） |

`update_plan` 的总量对比推翻了"codex 更省"的假设：codex schema 731 + 提示词约 2,400 ≈
3.1KB，我们 2,184 + 730 ≈ 2.9KB，**我们略小**，只是切分相反。真正的差距在延迟内置工具
这件事上——而那是我们走得更远的地方。

**不要用"占比"做验收**——这一版原先写的是"占比降到 20% 以下"，实测证明那是个反向
激励：C2 落地后绝对成本从 10,845 降到 8,113（−25%），而占比从 29.3% **升到** 33.3%，
因为分母（history、tool_results）降得更多。占比会在你成功压缩其它切片时上升，也可以
靠**膨胀历史**来"达标"。（那 6 轮样本都是短问答，分母偏低有采样偏差；但指标本身的
问题与采样无关。）

## 10. Schema 与迁移

**边界。** v11 是最后一个公开发布过的 schema（beta.21 系列已发出），必须始终可
升级。v11 之后的中途 schema 在决定让其他人安装之前可以自由变更。

**每次结构变更。** 变更前备份并做完整性检查；拒绝写入更新的不支持 schema；变更后
校验状态桶不变、无孤儿、owner 不越界、跨进程唯一认领仍成立。

**放松的部分。** 在只有所有者一人使用期间，不为每一次中途 schema 变更准备发布级
升级 fixture。决定让别人安装时，补一次从 v11 到当时版本的升级路径与验证。

## 11. 验证

不预设产品判据（这条是明确决定，见 §0.2 第 9 条）：在没有第二个用户、没有稳定
基线的阶段，预设的阈值大概率是想象出来的。取而代之的是**真实使用数据的周期性
审计**，仓库里的 daily-driver 审计 Skill 就是这个用途。

每次发布仍守住工程底线：build、test、完整离线 eval、带数据迁移验证、安装包启动、
doctor、daemon restart、一次 CLI active steer、一次精确 resume、一个跨端结构化
场景。

必须能证明的行为（用真实场景，不用预设指标）：

1. CLI 开始工作、IM 补充信息，active Run 收到并使用。
2. CLI 关闭重开后能自然询问或继续刚才的工作。
3. 历史超过默认窗口后仍可检索到指定的旧决定、版本、文件和结果。
4. 用户不维护任何 Task。**注意：这不等于列表会变短。** 2026-09-04 实测，删掉
   `/tasks` 之后裸 `/resume` 有 7 条 attention，全部来自前一天，其中 3 条"确认执行"
   和 2 条几乎相同的 gcp 发布。产生长列表的机制是 Run 停在 `waiting_user` 且没人
   dismiss，本方案完全没有触及它。把"没有陈旧列表"算作本方案的收益是错的。
5. 摘要器或语义召回失败时前台仍能执行。
6. 稳定偏好可跨端召回，临时指令不进入长期 Memory。
7. 精确审批、澄清、watcher 和中断恢复仍绑定正确的 Run。
8. 两个进程同时恢复同一个 Run 时只有一个成功。

## 12. 文档影响

实施到对应批次时同步更新：`CONTEXT.md`、`AGENTS.md` 的 Tasks/Context 不变量、
`docs/STATUS.md`、`docs/work-timeline.md`、`docs/identity-continuity.md`、
`docs/context-lifecycle.zh-CN.md`、`docs/command-reference.md` 及中文翻译、
`docs/adr/0004` 标记 superseded 并新增一份"Run 为执行与恢复的唯一权威"的 ADR、
`docs/manifest.yaml` 与生成的 `docs/README.md`。

对应批次未完成前，current capability 文档不得提前描述目标能力为已实现。
