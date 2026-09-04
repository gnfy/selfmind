# Thread 化工作历史重构方案

> 生命周期：archived（实现记录；已由 task-capsule-work-history-redesign.zh-CN.md 取代）；
> 项目所有者曾于 2026-09-02 批准。
> 日期：2026-09-02  
> 复审日期：2026-09-16  
> 范围：Task、Run、跨端连续性、工作历史、Attention、上下文选择与本地数据迁移。  
> 前置结论：`main-turn-work-continuity.md` 已暂停；其尚未完成的真实 IM、重启、
> correction 与 stranger-isolation 证据门禁由本方案继续承担。

> 归档说明：schema v11 的 Thread/Run/Attention 分离仍是有价值的中间结果，但
> “每个 root Run 先创建 Thread”与“parent edge 同时承担自然语言连续性”的边界已被
> 后续 daily-driver 证据否定。新方案保留 Run-owned execution 和 derived Attention，
> 并进一步把语义 Task 整理与精确 Run 恢复分开。

## 1. 结论

SelfMind 仍然需要一个跨 Run、跨端点的稳定聚合对象，但不应继续使用当前
“需要用户维护完成状态的 Task”模型。

本方案决定采用以下目标形态：

- 用 **Thread** 取代当前内部 Task 领域模型；不要同时保留 `tasks` 和
  `work_threads` 两套事实来源。
- 每个普通 root Run 先属于一个默认 `unlisted` 的 Interaction Thread。
- 只有具备持续工作价值的 Thread 才提升为 `work + listed`，并出现在
  `/tasks` 中。
- Thread 不保存执行状态；`active`、`needs_attention`、`monitoring`、
  `resumable`、`settled` 全部从 Run 和真实等待对象动态派生。
- 用户正常使用不需要执行 `complete`。一个 Thread 没有活动 Run 或待处理事项
  时自动沉降为历史，但仍然可以自然语言检索和继续。
- Memory 只保存稳定的个人偏好和习惯。任务和闲聊都进入可检索历史，但不会
  因此把每条消息写成长期 Memory。

一句话原则：

> 所有交互都可被回忆，但只有值得继续管理的工作才成为可见 Task；分类可以晚一点发生，也必须能够修正。

### 1.1 实施状态（2026-09-03）

Batch 0–5 已落地：schema v11、WorkTimeline、Run 派生 Attention、默认 unlisted
Interaction、确定性提升、exact-Run resume、历史/Attention 分离搜索、兼容命令和带
备份的本地历史重置均已完成。完整 `selfmind selfcheck` 为 64/64，通过 npm 安装包
lifecycle smoke；所有者本地库已验证 v10→v11、重置备份、清空及 daemon 重启。

Batch 6（2026-09-03 审查修复，见 §12）：同域自然语言 RESUME 已改为在
`work_select` 工具调用时直接认领并在同一回合继续；dismiss 拒绝、Attention 取代与
证据门禁、证据式提升、状态判定收敛和 watcher finalization 精确 child 正在实施，
随后进入 §13.3 的观察指标窗口。

本计划原计划继续完成 24 小时与 7 天 daily-driver 指标观察，以及真实 CLI↔IM、
restart、cross-workspace 和 correction 证据收集；这些尚未闭合的证据由后继
Task Capsule 计划接管，本文件不再贡献 active priority。

## 2. 为什么现在需要调整

### 2.1 当前模型把三种生命周期压进一个 Task

当前 Task 同时承担：

1. 每个 root Run 的自动历史分组；
2. 跨端继续、检索和展示的稳定索引；
3. 类似工单的 `in_progress / waiting / done / archived` 生命周期。

前两项是个人 AI 网关需要的工作历史能力，第三项却要求用户维护一份隐形待办
台账。CLI 没有显式“会话管理”本来是 SelfMind 自动整理的理由，但现在变成了
“每条消息自动创建 Task，再要求用户逐条关闭”，与原目标相反。

### 2.2 Run 已经拥有真正的执行事实

执行是否开始、完成、中断、等待输入或验证不足，本来就是具体 Run 的事实；
审批、澄清、watcher、Plan、handoff、artifact 也都有自己的持久对象。Thread
再次保存 `status`、`active_run_id`、`blocked_reason` 和 `next_steps`，形成重复
权威，并使晚到的 Run finalization、用户 complete 和等待对象互相覆盖。

“一次模型回合结束”与“一段工作永远结束”也不是同一个事实：前者可以可靠自动
记录，后者通常不需要用户声明。用户下周仍可继续一个已经沉降的 Thread，不应先
把它从 `done` 重新打开。

### 2.3 真实运行已经出现稳定症状

截至 2026-09-02 20:50 CST，最近 7 天的本地 daily-driver 数据为：

| 指标 | 实测 |
| --- | ---: |
| Runs | 59 |
| root Runs | 57 |
| child continuations | 2 |
| 使用过工具的 Runs | 44 |
| 具有 Plan 的 Runs | 11 |
| 结束在 interrupted/waiting/blocked/verification 的 Runs | 27 |

其中“确认执行”“执行完毕了吗？”“已刷新 MFA，继续”“可以了，你执行吧”等明显
依赖前文的输入，大多仍被建成独立 root。另一方面，“你好”“你是什么模型？”和
“1234”也拥有 Task。真实数据说明：

- 句子长短不能区分任务和闲聊；
- 是否调用工具不能区分——查询型闲聊可以调用工具，代码任务也可能直接回答；
- 是否创建 Plan 只能作为强证据，不能覆盖所有工作；
- 脱离多轮上下文的 ingress 二分类无法稳定判断“确认执行”属于哪项工作。

这些数据大部分产生于当前 Main-turn continuity 切换前，因此不能单独证明最新
连续性实现仍然失败；但足以证明“所有 root 消息都成为可见、待关闭 Task”的领域
模型不适合长期运行。

### 2.4 状态已经影响连续性候选和上下文成本

完整 resume context 已受精确 `parent_run_id` 保护，Task 状态不会单独触发整个
handoff/events/artifacts 注入，这是必须保留的安全底线。但当前状态仍会：

- 让 `/resume` 把所有非终态 Task 当成可恢复工作；
- 让启动摘要长期提示历史 verification/interrupted 标签；
- 让 `work_search` 对每次搜索无条件附带多条未认领 Run；
- 通过 Task card 的 title/status/summary 占用召回候选和 prompt 预算；
- 让 `done/archived` 反过来决定一个 Run 是否能进入隐式连续性候选；
- 让 `/task complete` 同时过期审批/澄清并取消队列，证明 Task 已经不只是展示标签。

因此当前问题不仅是 UI 列表难用，也会增加 Main 从无关候选中判断历史工作的成本
和误选风险。

### 2.5 现在修改的总代价最低

当前 `task_id` 已分布到约 22 张控制表和 233 个 Go 文件（包括测试）。这说明稳定
聚合身份确实有未来价值，也说明继续把新能力接到错误语义上会迅速增加迁移成本。
项目仍在 beta、所有者允许清空自己的工作历史，所以现在适合纠正模型；但发布
迁移仍必须备份并保留其他用户的历史，不能把本地可清空等同于产品可以自动删库。

## 3. 目的与目标

### 3.1 目的

让 SelfMind 在不要求用户管理会话或工单的前提下，自动形成低噪声、可搜索、可
跨端继续的工作历史；同时让上下文只由精确 Run 关系和有界检索进入模型。

### 3.2 目标

1. 普通闲聊和一次性问答不会填满 `/tasks` 或 `/resume`。
2. 真正的多步工作、产生持久结果的工作和需要用户处理的工作自动成为可见 Thread。
3. 第一次没有识别成工作也不会丢失；后续继续时可以提升并关联已有 Run。
4. Task/Thread 的展示分类永远不能授予执行范围、认领 parent 或注入完整上下文。
5. 不增加 ingress LLM classifier，也不为每条消息增加第二次 Main 调用。
6. CLI、IM、HTTP 和 cron 使用同一套 person-scoped Thread/Run 事实。
7. 原始 transcript 继续保持 channel-local；跨端只共享结构化历史。
8. Memory 继续保持 preference-only，不吸收运行状态和普通聊天流水。
9. 新鲜库、v10 升级库、数据库恢复和本地历史清理均有可重复验证。

### 3.3 非目标

- 不改变一人一个 active Run 的 Phase-1 并发契约。
- 不重新引入 `fast_classifier` 或 post-run Task 路由器来决定执行归属。
- 不让 semantic recall 成为连续性或 Thread 提升的权威。
- 不共享 CLI 与 IM 的原始聊天 transcript。
- 不因为 Thread 重构同时设计 SaaS、远程 Runner 或组织级项目管理。
- 不追求第一次消息就百分之百判断“任务还是闲聊”。

## 4. 领域语言

### Interaction

一次用户输入及其对应的回答。每次 Interaction 都可审计和检索，但不一定值得在
工作列表中长期展示。

### Conversation

一个端点内的原始聊天记录。Conversation 保持 channel-local，不是跨端共享对象。

### Run

一次 Main 执行尝试，拥有执行范围、Plan、工具、审批、结果和终态。Run 是执行
生命周期的唯一权威。

### Thread

一组通过确定性继续关系关联的 Runs。Thread 提供稳定标题、摘要、搜索引用和用户
整理属性，不拥有执行状态。

### Work Thread

已经被提升为值得长期展示和继续管理的 Thread。用户界面的 `/tasks` 实际列出的是
Work Threads；可以继续沿用命令名，不要求用户学习新的内部术语。

### Attention

从活跃 Run、待审批、待澄清、watcher 或未认领可恢复 Run 动态得到的“现在需要
处理什么”。Attention 是查询投影，不是 Thread 状态。

### Memory

用户长期稳定的偏好、习惯和纠正。Memory 与 Conversation、Thread、Run、Artifact
和工作历史保持独立。

本方案获批时，将把以上已确认术语补入根 `CONTEXT.md`，并用一份简短 ADR 记录
“Thread 取代 Task 生命周期”的不可逆架构决定。

## 5. 核心不变量

1. **Run 掌握执行事实。** Thread 不持久化 `running/waiting/done/blocked`。
2. **Parent edge 掌握继续关系。** 只有精确、经网关校验的 `parent_run_id` 可以
   加载 parent Run 的 handoff、Plan、events、artifacts 和 checkpoint。
3. **展示不改变权限。** `kind`、`visibility`、title、pin、archive 和自动提升只
   影响列表与检索排序。
4. **默认可恢复、默认不打扰。** 未列出的 Interaction 仍保留在 work spine 和
   历史索引；它可以被搜索和后续提升，但不会进入普通工作列表。
5. **延迟且可逆。** 模糊输入先保持 unlisted，比错误制造永久 Task 更安全；第二轮
   和后续运行证据可以提升，用户也可手动 listed/unlisted/archive。
6. **Main 负责语义建议，网关负责提交。** Main 可以在正常 Run 内提出 Thread
   disposition；网关用真实 Plan、工具效果、等待对象和 person/scope 校验后提交。
7. **无额外前台模型调用。** Thread 分类不能成为 Run 前置步骤。
8. **Memory 不兜底历史。** Thread 分类失败由搜索和提升修复，不能把所有消息保存
   到个人长期 Memory。

## 6. 目标流程

```text
用户输入
  -> 身份、端点、显式控制和 daemon-origin 校验
  -> 有 active Run?
       是 -> 持久化 steer
             -> Main 在安全检查点理解
                  相关 -> 更新当前 Run/Plan，必要时提升当前 Thread
                  独立 -> 排队为新的 unlisted Interaction Thread
                  历史继续 -> 排队为精确 parent child，提升目标 Thread
       否 -> 创建 root Run + unlisted Interaction Thread
             -> 组装 person spine + 有界检索上下文
             -> Main 正常回答/执行
             -> 网关依据实际证据计算 Thread disposition
                  无持续价值 -> 保持 unlisted，进入历史
                  有持续价值 -> work + listed
                  需要处理 -> work + listed，并由 Attention 投影展示
```

这个流程不要求 ingress 判断“任务或闲聊”。判断发生在 Main 已经理解内容、Run
已经产生真实证据之后；即使第一次保持 unlisted，未来仍能从完整保留历史中找到。

## 7. 自动提升规则

### 7.1 确定性强证据（工作证据）

出现任一条件时，网关可直接把 Thread 提升为 `work + listed`：

- 创建了多步骤 Plan；
- 产生非只读效果、修改文件或工作输出 Artifact（不含用户输入附件和内部
  tool-output spool）；
- 创建审批、澄清或 watcher（创建即立即提升）；
- Run 结束为 waiting、blocked 或 verification incomplete；interrupted 只在该
  Run 留下工作证据时计入；
- 创建精确 child continuation（parent edge），或 active steer 被 Main 接纳为
  相关工作；
- handoff 含 next steps 或改动文件；
- 用户显式 pin、命名、列出或选择该 Thread；
- 明确的 `/new` 工作入口创建了 Thread。

“工作证据”的统一定义（2026-09-03 审查修复）：Plan、非生命周期的副作用工具
ledger 行、审批/澄清/watcher、parent edge、next steps。生命周期工具
（`finish_run`、`update_plan`、`work_select`、`queue_user_input`）和只读工具
不是证据，否则每次直接回答都会因为 `finish_run` 而被提升。Attention 对
`interrupted` 的判定与提升共用这一定义。OBSERVE 投影永远不会把已 pin 或已有
证据的 Thread 降回 unlisted。

强证据不是“任务定义”的穷举，而是安全的提升下限。一次只读查询可以保持
unlisted；一次无工具的设计讨论在第二次相关交互时可以提升。

### 7.2 Main 的语义建议

Main 已经在正常 Run 中理解用户目标，可以随 `run.outcome` 提供可选、结构化的
display-only 建议：

```text
thread_disposition: keep_unlisted | promote_work
reason: concise semantic reason
```

该字段：

- 不触发额外模型回合；
- 不接受 confidence 阈值作为执行权限；
- 不能选择 parent、workspace、approval 或 delivery；
- 缺失、解析失败或模型不可用时回退到确定性证据；
- 不允许自动归档或删除已有 Work Thread。

如果正常直接回答没有携带该建议，默认保持 unlisted。这样漏掉的只是一次列表
展示，底层历史仍完整；后续继续、搜索或持久效果会自动修正。

### 7.3 多轮提升

多轮是比第一句文本更可靠的证据：

- 同一 active Run 内收到相关 steer：保持同一 Thread，并提升为 work；
- 新 child Run 精确继续 parent：继承 `thread_id` 并提升；
- 从历史搜索后 RESUME（直接模式）：当目标 Run 与当前解释 Run 同属一个执行域
  （相同 workspace 且执行根完全一致）、没有未完成的 loop checkpoint，且当前
  Run 尚未产生任何效果时，网关在 `work_select` 工具调用时原子认领 parent
  （`control.ClaimInteractionContinuation`），把当前 Run 重新指向 parent 的
  Thread，以持久 `plan.updated` 事件恢复 parent 的 Plan，并把 parent 的有界
  resume context 作为工具结果返回，Main 在同一回合继续工作——一个 Main Run，
  不排队。效果发生前的一次纠正改选另一个同域 Run 使用
  `control.RetargetInteractionContinuation`；
- 从历史搜索后 RESUME（转移模式）：执行域或 checkpoint 不匹配时，仍在
  finalization 创建正确 scope 的 exact-parent child，child 在开始前继承 parent
  的 Thread、scope 和 Plan；第二个 Run 是“执行域不能原地改变”的必然结果；
- OBSERVE 只建立 reference，不把“进展怎么样”提升成独立 Work Thread；
- 一段普通讨论后来要求实施：已有讨论 Run 保持历史，新实施 Run 通过精确关联进入
  同一 Thread，Thread 此时提升。

## 8. 目标数据模型

### 8.1 不新增第二个 Work Thread 事实源

本方案不是在 `tasks` 旁增加一张 `work_threads`。目标是让一张 `threads` 表取代
当前 `tasks`，其余表逐步把 `task_id` 改为 `thread_id`。数据库迁移内部可以临时
建表复制，但一个版本提交完成后只能有一个权威表。

### 8.2 threads

建议字段：

```sql
CREATE TABLE threads (
    id               TEXT PRIMARY KEY,
    tenant_id        TEXT NOT NULL,
    person_id        TEXT NOT NULL,
    workspace_id     TEXT,
    kind             TEXT NOT NULL, -- interaction | work | recurring
    visibility       TEXT NOT NULL, -- unlisted | listed | archived
    title            TEXT NOT NULL DEFAULT '',
    summary          TEXT NOT NULL DEFAULT '',
    pinned           INTEGER NOT NULL DEFAULT 0,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL,
    last_activity_at INTEGER NOT NULL
);
```

明确不再保存：

- `status`
- `active_run_id`
- `blocked_reason`
- `next_steps_json`
- `last_channel`

这些信息分别从 Runs、Plan、handoff 和端点路由读取。

### 8.3 runs

`task_runs` 目标上改名为 `runs`，核心关系为：

```sql
CREATE TABLE runs (
    id                        TEXT PRIMARY KEY,
    thread_id                 TEXT NOT NULL,
    tenant_id                 TEXT NOT NULL,
    person_id                 TEXT NOT NULL,
    workspace_id              TEXT,
    parent_run_id             TEXT NOT NULL DEFAULT '',
    status                    TEXT NOT NULL,
    attention_dismissed_at    INTEGER,
    attention_dismissed_by    TEXT NOT NULL DEFAULT '',
    ...
);
```

- root Run 创建新的 Thread；child Run 继承 parent 的 `thread_id`。
- `parent_run_id` 仍是唯一继续权威和并发认领护栏。
- `attention_dismissed_at` 只表示用户不再希望这个未完成 Run 出现在隐式提醒中，
  不篡改其真实历史 status；显式 resume 仍可重新继续。

### 8.4 事实归属

| 数据 | 第一权威 | Thread 的作用 |
| --- | --- | --- |
| Plan / checkpoint / tool ledger | Run | 只做聚合查询 |
| Approval / clarify / watcher | Run 或自身对象 | 派生 Attention |
| Handoff / artifact / event | Run | 按 Thread 汇总展示 |
| title / summary / pin / archive | Thread | 直接拥有 |
| Conversation transcript | endpoint session | 不跨端复制 |
| Work spine | Person + Run/Thread ref | 跨端轻量连续性 |
| Preference Memory | Person/workspace scope | 与 Thread 独立 |
| Reference/work key | Thread + evidence Run | 搜索提示，不路由 |

### 8.5 Attention 是查询投影

`WorkTimeline` Module 在一次 person-scoped 查询中合并：

1. live Run；
2. pending approval；
3. pending clarification；
4. pending/running watcher；
5. 尚未被 child 认领、未 dismissed，且仍是所在 Thread 最新 Run 的 resumable
   Run（因果取代：同一 Thread 中更晚的 Run 取代更早的 parked Run）；
   `interrupted` 只在该 Run 留下工作证据（见 §7.1）时计入。

排序：同 channel 的条目优先，其后 pinned Thread 与更强的 live 信号优先于时间。
`/status`、attach digest、task card 与兼容的 `Task.status` 投影都读取这一派生，
不各自判断状态；`UpdateTaskStatus` 不再接受 status 参数。

dismiss 规则：`/task <n> complete` 与没有活动 Run 时的 `/stop` 只 dismiss 精确
指定的 Run；当该 Run 仍有 pending approval、pending clarification 或 live
watcher 时拒绝执行，而不是把它们藏起来——需要回答的对象必须先被回答、拒绝或
取消。

不新增一个可与这些事实漂移的 `attention_status`。如果以后性能数据证明需要缓存，
只能增加可重建的 materialized projection，不能成为权威。

## 9. Module 与调用边界

当前 Task 逻辑分散在 gateway handlers、Store 查询、context selector、recall、TUI
和维护任务中。重构应形成一个深的 **WorkTimeline Module**，隐藏：

- Thread 创建与自动提升；
- Run finalization 后的派生视图；
- Attention 聚合与 dismissal；
- person-scoped 搜索与列表排序；
- 精确 parent continuation commit；
- 用户 pin/unlist/archive 等展示操作。

Gateway、IM、TUI、HTTP 和 tools 只使用该 Module 的少量用例接口，不再自行解释
`status`。SQLite 是本地可替代依赖，生产和测试都使用同一个实现；在没有第二个
真实 Adapter 前，不新增只为 mock 而存在的 Go interface。

`kernel` 继续只接收选好的 `TaskRuntimeContext` 后继类型，不依赖 control 或 gateway。
重命名可以在后续批次完成，但最终 prompt 类型也应改为中性的
`RunRuntimeContext/ThreadRuntimeContext`，避免 Task 状态重新成为上下文权威。

## 10. 用户界面和命令

为避免没有价值的命令 churn，可以继续保留 `/tasks` 和 `/task`：

- `/tasks`：列出 `work + listed` Threads，而不是所有非终态记录；
- `/tasks done`：列出动态派生为 settled 的 Work Threads；
- `/tasks search`：搜索完整保留历史，包括 unlisted Interaction；
- `/resume`：只列有精确 resumable Run 的 Threads；每一行必须能解析到 Run；
- `/task <ref> archive`：隐藏 Thread，历史保留；
- `/task <ref> complete`：迁移期兼容别名，含义为“dismiss 该精确 Run 的
  Attention”，不改写任何 Run 的历史 status；若该 Run 仍有 pending approval、
  pending clarification 或 live watcher 则拒绝，并提示先处理这些对象；
- `/stop`：有活动 Run 时取消它；没有时只 dismiss 精确 pinned 的 Run，同样受
  上述拒绝规则约束；
- 后续可以增加更准确的 `dismiss` 文案，但不要求用户通过它维持正常列表卫生。

启动摘要只展示 Attention，不再把所有 listed 或 unsettled Thread 解释成“仍需处理”。

## 11. Context、Recall 与 Memory

### 11.1 Context

- exact parent Run：允许完整的 run-scoped context；
- 没有 parent：只使用当前 Run、workspace、person spine 和有界 recall；
- Thread title/summary 是参考卡片，不是用户指令；
- Thread kind/visibility 不影响 execution scope、approval 或 tool grants。

### 11.2 Work search

`work_search` 应把两种意图分开：

- 普通 query：按用户内容搜索完整 retained history；
- attention query：显式请求当前可恢复/待处理项目。

不能再在每次普通搜索中无条件拼接最多 8 个 unrelated unresolved Runs。Main 可以在
“确认执行”这类缺少字面线索的输入中请求 attention；网关返回小而准确的状态卡。

### 11.3 Memory

- 任务结果、发布状态、文件和计划进入 Work History；
- 普通闲聊进入 endpoint Conversation 和 person work spine；
- 只有稳定偏好、习惯和用户纠正进入 Memory；
- Thread 分类错误通过搜索、提升和用户展示操作修复，不能靠“保存所有消息到
  Memory”兜底。

## 12. 迁移方案

### Batch 0：拍板与基线

- 对当前 Main-turn continuity plan 给出 verdict，保持唯一 active plan。
- 本方案获批后写 ADR，并更新 `CONTEXT.md`、`work-timeline.md`、
  `identity-continuity.md` 和 `STATUS.md` 的领域表述。
- 固定迁移前 24h/7d 指标：root/child 比例、可见 Task 数、unresolved 候选数、
  `/resume` 精确率、人工 complete/archive 次数、额外 provider call 数。

验收：文档契约通过；新 plan 是唯一 active plan；没有代码行为变化。

### Batch 1：建立 WorkTimeline Module

- 在现有 schema 上先集中 Thread projection、Attention、搜索和 presentation 操作。
- Gateway/TUI/IM/tools 通过 Module 用例读取，不再各自解释 Task status。
- 用 temp SQLite 从 Module 的可观察结果测试，不增加假 Adapter。

验收：现有行为可由新 Module 重现；调用方不直接拼装 open/resumable 状态。

### Batch 2：schema v11 Thread 化

- 原子迁移 `tasks -> threads`、`task_runs -> runs` 及关联 `task_id -> thread_id`。
- 去除 Thread 的执行状态字段；Run 增加 attention dismissal。
- 公共升级路径将旧 Task 一对一迁成 `work + listed`，保留全部历史。
- 新鲜数据库和 v10 fixture 都必须通过；迁移前自动备份，较新 schema 必须拒绝写入。

验收：无孤儿 Run/event/artifact/handoff/approval/queue；跨连接 parent 唯一认领仍成立；
v10 数据计数和 owner 分区迁移前后相等。

### Batch 3：Interaction 默认与自动提升

- root Run 默认创建 `interaction + unlisted` Thread。
- 落地确定性强证据与可选 Main disposition。
- child continuation 和相关 active steer 自动提升同一 Thread。
- 防止 promotion flapping：自动化只允许向 work/listed 提升，不自动反向隐藏；反向
  操作只来自用户或 retention archive。

验收：闲聊不进入 `/tasks`；多轮设计到实施会提升；模型建议缺失不影响执行或历史。

### Batch 4：列表、resume、摘要和 recall

- `/tasks`、`/resume`、启动摘要全部切换到 Thread/Attention 查询。
- `work_search` 分开普通历史与 Attention，不再无条件添加 unresolved Runs。
- prompt 去掉 Task lifecycle authority，只保留精确 parent Run 上下文。
- 保持 CLI/IM/HTTP 同一排序、person 分区和 bounded output。

验收：每个 `/resume` 结果都有精确 resumable Run；一次新查询不再被旧等待项目迫使
选择；跨端自然语言仍能找到并继续历史。

### Batch 5：兼容清理与本地重置

- 删除旧 Task reducer、`current_task` 黏性、旧 complete lifecycle 和无读者字段。
- 删除 superseded tests/eval/cassettes，而不是双轨长期保留。
- 提供正式的 `selfmind maintenance reset-work-history`：默认 dry-run，`--apply`
  前创建数据库备份，只清理 Thread/Run 及依赖工作历史，保留 identity、accounts、
  workspaces、model settings、credentials references、Memory 和 Skills。
- 拒绝在 live Run、watcher 或 started queue 存在时重置。

验收：所有者可以在备份后清空本地工作历史并从零验证；发布升级不自动删除其他
用户的数据。

### Batch 6：审查修复（2026-09-03）

首个 daily-driver 窗口的审查发现证据与文档缺口。修复项及状态：

| 项 | 内容 | 状态 |
| --- | --- | --- |
| A | dismiss 拒绝：`/task <n> complete` 与无活动 Run 的 `/stop` 只 dismiss 精确 Run，遇 pending approval/clarify 或 live watcher 时拒绝而不是隐藏 | 实施中 |
| B | Attention 取代与 interrupted 门禁：只有 Thread 最新 Run 可为 resumable；`interrupted` 需工作证据；同 channel 条目优先 | 实施中 |
| C | 证据式提升：生命周期工具与只读工具不是证据；审批/澄清/watcher 创建即提升；OBSERVE 投影不降级已 pin 或有证据的 Thread | 实施中 |
| D | 状态判定收敛：`/status`、attach digest、task card、兼容 `Task.status` 投影共用 Attention 派生；`UpdateTaskStatus` 去掉 status 参数 | 实施中 |
| E | watcher finalization：finalization 队列行携带 `ReplyToRunID = watch.RunID`，finalization Run 是 watcher Run 的精确 child；失败时把 watcher Run 标为 `blocked`（resumable）而不只改 Thread 摘要；取消抑制只看该 watcher Run 自己的 `run.cancelled` 事件 | 实施中 |
| F | 同域直接认领：`work_select(resume)` 在工具调用时通过 `ClaimInteractionContinuation` 原子认领同域 parent、恢复 Plan 并返回 resume context（一个 Main Run，不排队）；域或 checkpoint 不匹配仍在 finalization 创建 transfer child | 待观察 |

同批次的证据修复：v11 迁移保留 `hidden→unlisted` 与旧 `kind`（`inbox→interaction`），
迁移后校验无孤儿引用且 parent edge 数量不变；新增 v10 schema-only fixture 的行级
升级测试；`reset-work-history` 同时清理进行中的 Skill 学习证据与 memory 中的
`task:` 会话/run 来源引用；`doctor` 输出 control schema 版本；TUI 的
`/new`、`/resume`、`/task` 元数据改由共享命令目录派生；恢复
`timeline-run-candidates` 的 `continuation.candidates` 状态断言。

验收：Go/eval 门禁通过后进入 24 小时与 7 天观察（§13.3 新增指标）；“实施中”
条目落地后转为“待观察”。

## 13. 验证矩阵

### 13.1 Go tests

1. 新鲜数据库创建 interaction Thread，默认不出现在 `/tasks`。
2. Plan、持久效果、approval、clarify、watcher、resumable outcome 均能提升 Thread。
3. tool-free direct answer 可保持 unlisted，第二次精确继续后提升且历史不丢失。
4. Thread 无 status；activity/attention 从 Run 和等待对象实时派生。
5. Attention dismissal 不改写历史 Run status；显式 resume 可以覆盖 dismissal。
6. `/resume` 的每张卡都 round-trip 到 exact run id。
7. parent claim 继续通过独立数据库连接竞争测试。
8. v10 -> v11 migration 保留 owner、scope、counts 和 parent edges。
9. reset-work-history dry-run/apply/backup/restart/拒绝 live work 全覆盖。
10. stranger identity 无法搜索、列出、提升或恢复其他 person 的 Thread。

### 13.2 Production-path eval

| 场景 | 期望 |
| --- | --- |
| “你好，你是什么模型？” | 回答正常；`/tasks` 不新增 Work Thread |
| 一次性信息查询 | 可使用工具；完成后默认进入历史而非工作列表 |
| “设计一个功能”→“开始实现” | 第二轮关联并提升同一 Thread |
| 发布任务→“确认执行” | Main 使用上下文找到 exact parent，不新建可见“确认执行”Task |
| CLI 任务→IM 问进展 | 返回结构化状态；OBSERVE 不成为独立 Work Thread |
| 新 GCP 查询旁有多个旧等待项 | 作为新工作执行，不弹 unrelated task 选择 |
| semantic_recall 超时 | 本地检索继续，前台不阻塞、不误提升 |
| daemon restart 后 resume | exact parent、Plan、scope 和 Thread 不变 |

每个模型支持的 case 必须有 committed cassette；纯迁移、投影和排序使用 Go tests。

### 13.3 Daily-driver 证据

实现后至少观察 24 小时和 7 天两个窗口：

- 新 Interaction 中提升为 Work Thread 的比例；
- 用户手动 promote/unlist/archive/dismiss 的次数和纠正方向；
- `/resume` 中无精确 Run 的条目数，目标为 0；
- `/tasks` 中一次性问答和进度问题的误展示数，目标为 0；
- 自然语言历史继续的成功、澄清、误 NEW 和错误 RESUME 数；
- `work_search` 返回 unrelated attention cards 的数量，普通 query 目标为 0；
- Thread 分类新增的前台 provider calls，目标为 0；
- prompt 中 thread/history slice 的字符和 token 占用；
- CLI↔真实 IM 的查询、继续、审批和 restart 场景。

Batch 6 新增观察指标（2026-09-03）：

- 未解决 Run 数随时间的变化（unresolved count over time），期望随取代与
  dismiss 收敛而不是单调增长；
- listed Thread 占全部 Thread 的比例（listed ratio）；
- `/resume` 可达性：Attention 中每一条都能 round-trip 到精确 Run，目标 100%；
- 每次自然语言继续的 provider 调用数：同域直接认领目标 1，transfer 目标 ≤ 2；
- `work_search` attention 模式相对普通 query 的使用率；
- 手动 dismiss（`/task complete`、无活动 Run 的 `/stop`）次数及其中被拒绝的次数。

7 天不足以证明长期 recall 完整性，但足以发现列表重新膨胀、promotion 失控和
Attention 不消退。保留 14 天指标时钟用于 retention 和长期搜索观察。

## 14. 风险与控制

| 风险 | 控制 |
| --- | --- |
| 工作第一次保持 unlisted | 历史和 spine 不丢；后续 continuation、搜索、Plan 或效果自动提升 |
| 闲聊被误提升 | 只影响展示；用户可 unlist/archive，不影响 parent/scope/context |
| Main disposition 漂移 | 可选且 display-only；确定性证据和网关提交优先 |
| 动态 Attention 查询变慢 | personal scale 先用索引和 bounded query；有数据后才加可重建缓存 |
| v11 迁移范围大 | verified backup、released v10 fixture、计数/分区/孤儿不变量、拒绝较新 schema |
| 清理历史误删身份或 Memory | 正式维护命令、dry-run、显式表白名单、live-work guard、重启验证 |
| 重构期间双轨漂移 | 一个版本只保留一个事实源；批次完成后删除旧读写路径和 superseded tests |

## 15. 回滚

- schema v11 应用前生成可识别的 v10 数据库备份。
- 代码上线后如出现 parent 认领、person 隔离、审批恢复或 scope 错误，立即停止
  rollout，恢复上一兼容 binary 和对应备份；不做有损 down migration。
- Thread promotion 或列表策略错误可以在不回滚 schema 的前提下关闭自动提升，所有
  Runs 仍在历史中；关闭只影响展示，不能恢复旧 ingress classifier。
- 本地 reset 的回滚就是恢复该命令生成的备份；命令必须在输出中明确备份路径。

## 16. Definition of Done

只有同时满足以下条件，才能宣称 Task 生命周期已经被 Thread 模型取代：

1. `threads` 是唯一跨 Run 聚合事实源，旧 `tasks/current_task` 读写路径已删除。
2. Thread 不持久化执行状态；所有活动和 Attention 可从真实事实重建。
3. 普通交互默认不出现在 `/tasks`，但完整历史可检索并可后续提升。
4. `/resume` 只展示 exact resumable Runs，不要求用户维护 complete 才保持整洁。
5. Main 语义分类没有增加 run-external 或第二次前台模型调用。
6. parent Run、workspace、approval、delivery 和 tool authority 不受展示分类影响。
7. v10 升级、新鲜库、本地历史重置、备份恢复全部验证通过。
8. CLI↔真实 IM、restart、跨 workspace 和 correction 场景通过。
9. 完整 `selfmind selfcheck`、文档契约及所有 release eval/cassette 通过。
10. 24 小时和 7 天 evidence clock 未发现 Task 列表重新膨胀或上下文候选污染。
