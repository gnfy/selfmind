# Task Capsule 工作历史与执行连续性重构方案

> 生命周期：archived（已由 run-centric-work-history.zh-CN.md 取代）；
> 项目所有者曾于 2026-09-03 确认其架构方向，代码从未开始实施。
> 日期：2026-09-03  
> 复审日期：2026-09-17  
> 范围：Task、Run、工作历史、跨端连续性、上下文选择、后台整理、Plan、
> 外部等待与控制库迁移。  
> 取代：threaded-work-history-redesign.zh-CN.md。旧方案保留为归档实施记录。

> 归档说明：本方案对 Task 权威的诊断成立，但结论走了一条更长的路——它保留了
> Task（改名为 Capsule）并为其新增后台整理、证据快照与完成契约。2026-09-04 的评审
> 决定改为直接移除该对象：执行归 Run，历史归 person 级 Work Journal。诊断与实测证据
> 已被继任方案吸收，Capsule、Organizer、Work Obligation 与 Completion Contract 相关
> 章节不再有效。

## 0. 评审入口

这份方案首先供人评审，其次才是实现清单。评审者应先判断以下五个决定，
不需要先评审所有字段和函数名：

1. **Task 是可逆的语义摘要，不是执行容器。**
2. **Run 是执行事实的唯一所有者，创建 Run 不再要求先确定 Task。**
3. **resumes_run_id 只表示精确恢复，不表示语义相关。**
4. **Task Organizer 在后台整理证据，不参与前台 ingress 或执行授权。**
5. **任务闭环以目标和证据为准，Plan 与 watcher 都只是可选策略。**

若这五点成立，后续 schema、接口和迁移可以按本文批次逐步评审。若其中任一点
不成立，应先停止实现，而不是通过兼容字段把两套模型同时保留下来。

### 0.1 本轮已确认

- 将 parent_run_id 重命名为 resumes_run_id。
- 活跃 Run 收到用户输入时默认 steer，由正在工作的 Main 理解补充信息。
- 历史语义归组允许晚于执行发生，并允许拆分、合并和修正。
- 不要求 Plan 在第一次工具调用前出现，也不要求为了 UI 连续性制造步骤。
- watcher 不是强制流程；系统应选择能够完成目标的合适策略。
- 前台不增加 Task 分类模型调用，不恢复 fast_classifier 的连续性权威。

### 0.2 评审时特别关注

- 自动整理错误是否只能影响展示与检索，而不能改变权限和执行范围。
- 一个 Run 包含多个工作主题时，是否能够关联多个 Task。
- 后台整理延迟或失败时，跨端继续是否仍然可用。
- 精确恢复是否仍能保持 checkpoint、审批、澄清和幂等认领。
- 后续 Run 能否在不伪造 resume 关系的情况下，用证据关闭旧的未解决条件。
- 上下文与工具 schema 的预算是否有可测量的下降。
- v11 真实数据升级是否有备份、孤儿校验和可恢复路径。

### 0.3 本文怎么读

| 你想知道 | 看哪里 |
| --- | --- |
| 要做什么 | §16 实施批次；每个批次自带做什么／为什么／怎么做／验收 |
| 为什么做 | §2 动机，每条都附 §0.4 的可核对实测数据 |
| 怎么做 | §7 恢复契约、§8 Organizer、§13 数据模型、§15 迁移 |
| 做完算什么样 | §17 验收矩阵、§18 指标、§21 完成条件 |
| 明确不做什么 | §3.2 非目标、§22 已拒绝的替代方案 |

### 0.4 事实依据（2026-09-03 实测，可核对）

本文的动机不依赖推测。下面三组数字是评审时应该先看的依据，命令可重跑。

**改名成本**（决定 §7.5 是否值得做）：

| 项 | `parent_run_id` / `ParentRunID` |
| --- | ---: |
| 非测试 Go 文件 | 21 |
| 全部 Go 文件 | 38 |
| 出现次数 | 134 |
| 文档 / eval 用例 | 7 / 1 |

对照：上一版方案提出的 `threads`/`runs` 全表改名涉及 125 个非测试 Go 文件、
233 个 Go 文件、13 个 eval 用例，且不改变任何行为——那是它被否决的原因。本次
改名量级小一个数量级，而且改的是语义而非拼写。

**上下文构成**（决定 §9.5 和 Batch 4 的优先级），取最近 6 次 provider 调用：

| 切片 | token |
| --- | ---: |
| tool_schemas | 10,593–11,022 |
| history | 6,265–11,295 |
| task_runtime | 1,730–1,770 |
| stable_system | 1,099 |
| estimated_total | 24,023–38,497 |

工具 schema 是 task_runtime 的约 6 倍，占请求估算 token 的 28–45%。**上下文的钱
在工具目录，不在 Task 卡片。** Batch 4 的力气必须按这个比例分配。

**Plan 与 watcher 的真实行为**（决定 §11 和 §10 的前提）：主模型换为
deepseek-v4-flash 后，12–21 次工具调用的 Run 曾连续零 Plan 完成工作；一个
`gcloud builds describe` watcher 因执行绑定缺少 `network:shared` 而 `blocked`，
导致四个构建的终态无人确认。两者都说明 Plan 与 watcher 不能充当完成机制。

复核命令：

```bash
grep -rn 'parent_run_id\|ParentRunID' --include='*.go' internal cmd | wc -l
sqlite3 -readonly ~/.selfmind/data/control.db "select json_extract(payload_json,'\$.tool_schemas'), json_extract(payload_json,'\$.task_runtime'), json_extract(payload_json,'\$.estimated_total') from task_events where type='provider.call.context_breakdown' order by created_at desc limit 6;"
```

### 0.5 本轮评审后的修改（2026-09-04）

方向未变；以下六处按评审意见收紧，评审时可优先看这几处：

| 修改 | 位置 | 原因 |
| --- | --- | --- |
| Work Obligation 降级为描述性记录，明确**不是** Attention 的权威或必要条件，并加取代／证据门槛／时间窗三条防陈旧约束 | §4.10、§5(13)、§10.2.1、§10.4、§12.3、§13.6、§22.8 | 它与 v11 之前那个持久化的 Task `in_progress` 是同一类“存下来的推断”，会重现成对故障 |
| Completion Contract 明确为投影，禁止新增 `finish_run` 字段 | §10.1、§22.9 | 现有 8 字段已常被只填两项，再叠八项只会得到套话 |
| Batch 2 与 Batch 3 之间加一道证据门 | §16.1 | v11 落地两天内暴露四类问题，行为仍在快速变化 |
| Batch 4 验收改为工具 schema 占比的绝对门槛 | §16 Batch 4、§18.3 | 实测工具 schema 是 `task_runtime` 的约 6 倍，力气必须按比例分配 |
| 加入可核对的实测依据与“本文怎么读” | §0.3、§0.4 | 让动机可验证，而不是只可阅读 |
| 每个批次补齐做什么／为什么／怎么做／验收 | §16 | 便于逐批评审与验收 |

## 1. 结论

SelfMind 需要同时保留三种不同性质的事实：

1. **正在发生的工作**：由 Run、活跃输入和控制对象表达。
2. **人可以检索的工作历史**：由 Task Capsule 汇总。
3. **一次执行从哪里精确恢复**：由 resumes_run_id 表达。

当前实现已经把执行状态从 Thread 中移到 Run，但仍要求每个根 Run 先创建一个
Thread，并把自然语言历史匹配与精确 Run 认领连接起来。这使“语义上可能相关”
仍然过早地变成“执行上继续这个 Run”。

目标模型不再要求每个 Interaction 立即属于一个 Task。用户消息先进入
Conversation、Work Spine 和 Run；Main 正常理解并完成工作；后台 Task Organizer
再从结构化证据中创建或更新 Task Capsule。历史匹配只建立可逆证据链接，只有明确
恢复未完成执行时才写 resumes_run_id。

一句话原则：

> 先可靠地执行和保留证据，再整理工作历史；语义可以修正，执行权威不能猜测。

## 2. 为什么要再次调整

### 2.1 Thread 重构解决了状态混杂，但没有解决过早归组

schema v11 已经正确地让 Run 拥有执行状态，让 Attention 从真实等待对象派生。
但是每个根 Run 仍同步创建一个 unlisted Thread。后续自然语言 RESUME 还会把当前
Run 重新指向目标 Thread，并认领目标 Run。

这有三个后果：

- 新输入尚未被 Main 完整理解，就已经拥有一个语义容器。
- “查看历史”“继续同一主题”和“恢复中断执行”仍共享 work_select/parent claim。
- 一次错误判断会同时影响 Task 展示、Attention、上下文和执行续接。

### 2.2 parent_run_id 的名称和职责都过宽

“parent”容易被理解为主题层级、会话层级或一般因果关系。实际需要的安全能力更窄：
一个新 Run 是否在恢复某个尚未被认领的精确 Run。

新名称 resumes_run_id 明确表达：

- 它只指向一个具体 Run；
- 它只在恢复执行状态时存在；
- 它不是 Task 关系；
- 它不是“上一轮消息”；
- 它不是普通历史引用。

改名之所以值得做，是因为它改的是语义而非拼写，而且成本已实测（§0.4）：21 个非
测试 Go 文件、134 处、7 份文档、1 个 eval 用例。§7.3 列出的 8 条事务校验几乎就是
现有 `ClaimInteractionContinuation` 已在执行的检查，所以 Batch 1 主要是改名与收敛，
不引入新的执行行为。

### 2.3 任务和闲聊不适合在 ingress 二分类

“确认执行”“继续”“进展怎么样”只有结合活跃工作或历史证据才有含义。一次只读查询
可能是重要工作，一次长讨论也可能只是临时交流。消息长度、关键词、是否调用工具、
是否创建 Plan 都不能独立完成可靠分类。

因此本方案不在 Run 前判断“任务还是闲聊”，也不增加单独的 LLM classifier。
分类和归组发生在 Main 已经产生结果之后，错误只影响可逆历史投影。

### 2.4 Task、Plan 和 watcher 被误当成完成机制

Task 状态无法准确表达用户是否还认为一段工作“未完成”。Plan 是 Main 的执行辅助，
不应成为 UI 仪式。watcher 只适合存在稳定可观察对象的长外部操作，也不应成为所有
异步工作的固定答案。

真正需要持久化的是目标、验收证据、当前责任方、未解决条件和已经尝试过的策略。

### 2.5 上下文的目标是帮助 Main 理解，不是完整搬运数据库

当前 Work Spine、TaskRuntimeContext、workspace、memory、recall 和工具 schema
都可能进入一次请求。实测（§0.4）给出了明确的优先级：**工具 schema 占请求估算
token 的 28–45%，是 task_runtime 的约 6 倍。**

因此本方案对上下文的处理分两个不同量级的动作，不能混为一谈：

- **主要动作**：工具目录分层（§9.5）。这是唯一能显著降低单轮 token 的改动，
  Batch 4 的验收指标必须落在这一项的绝对占比上。
- **次要动作**：Task 卡片限为 0–2 个、完整历史改按需读取（§9.1、§9.3）。它省下的
  是千级 token，价值主要在**减少误注入和候选噪声**，而不是省钱。

新的 Task Capsule 必须是检索索引和紧凑摘要。默认上下文只提供当前工作需要的
最小证据；旧历史、artifact 和工具结果通过按需读取进入。

## 3. 目标与非目标

### 3.1 目标

1. 用户无需创建、关闭或维护 Task，也能获得可检索的工作历史。
2. 普通闲聊保留在 Conversation 和 Work Spine，但默认不创建 Task。
3. 一个 Run 可以贡献给零个、一个或多个 Task。
4. 同一个 Task 可以跨 CLI、IM、HTTP、cron、workspace 和多个 Run 聚合证据。
5. 跨端的新消息在后台整理尚未运行时仍能理解近期工作。
6. 历史 Task 匹配不自动认领旧 Run，不改变执行域或权限。
7. 精确恢复继续保持唯一认领、checkpoint 保真和 person/scope 校验。
8. Organizer、semantic_recall 或后台模型失败时，前台工作仍然可用。
9. Plan 可以在探索后出现，也可以不用；显示内容必须与证据一致。
10. watcher 不可用时，Main 能尝试真正不同的完成策略。
11. 上下文和工具 schema 有明确预算与效果指标。
12. 新鲜库和带数据的 v11 升级库都有可重复验证。

### 3.2 非目标

- 不改变 Phase 1 的一人一个 active Run 并发契约。
- 不共享不同端点的原始 transcript。
- 不把所有聊天写入长期 Memory。
- 不允许 Task Organizer 选择 workspace、权限、审批或执行工具。
- 不让 semantic_recall 成为执行连续性权威。
- 不要求第一次消息立即获得完美 Task 分类。
- 不在本方案中引入 SaaS、远程 Runner 或组织项目管理。
- 不通过项目、工单系统或云厂商关键词实现通用流程。

## 4. 领域模型

### 4.1 目标关系

    Person
      ├─ endpoint-local Conversations
      ├─ person-level Work Spine
      ├─ Active Work Buffer
      ├─ Memory
      ├─ Runs
      │    ├─ plan / events / tools / artifacts / handoff
      │    ├─ approvals / clarifications / external waits
      │    ├─ Completion Contract
      │    ├─ Work Obligations
      │    └─ resumes_run_id ──> exact prior Run
      └─ Task Capsules
           └─ Task Evidence <──> Runs / Work Units / evidence slices

    Attention = projection(active Runs, open Work Obligations,
                           approvals, clarifications, external waits)

Task 与 Run 是多对多语义关系。resumes_run_id 是 Run 与 Run 之间的精确执行关系。
两者不得通过隐式规则互相推导。

### 4.2 Conversation

一个端点内的原始聊天记录。它服务于该端点的阅读和短期对话连贯性，不跨端复制。

### 4.3 Work Spine

person 级的精简工作轨迹。每个 Main turn 保存用户输入、最终回答、触及路径和来源，
不保存工具中间输出。它保证后台整理失败时近期工作仍然存在。

### 4.4 Active Work Buffer

Main 当前理解工作所需的有界视图，由以下事实即时派生：

- 当前 active Run 的目标和结构化状态；
- 当前端点的少量对话尾部；
- 尚未被 Organizer 覆盖的 Work Spine 条目；
- 最近完成但仍可能被继续的少量结构化结果。

它不是新的完整 transcript 表，也不是 Task。Organizer 的处理游标只决定哪些证据
尚待整理，不能决定前台是否可用。

### 4.5 Run

一次可审计的 Main 执行尝试。Run 是执行范围、生命周期、Plan、工具效果、等待对象、
完成证据和恢复状态的唯一所有者。

### 4.6 Task Capsule

一组相关工作证据的可逆语义摘要。它拥有稳定标题、目标、关键事实、决定、产出、
未解决问题、下一步提示、pin/archive 和更新时间，但不拥有执行状态或权限。

用户界面继续称为 Task，内部文档使用 Task Capsule 防止它再次退化成待办状态机。

### 4.7 Task Evidence

Task Capsule 与一个 Run、Work Unit 或 Run 内证据切片之间的带来源链接。链接可以
由 Organizer 提议、由用户确认或纠正，并且能够被重新归组。

### 4.8 Resume Source

被新 Run 精确恢复的旧 Run。该关系持久化为 resumes_run_id，只表示执行连续性。

### 4.9 Attention

当前确实需要人或系统处理的 Run/控制对象投影。Attention 与 Task Capsule 分离；
Task 可以存在而没有 Attention，Attention 也可以在 Task 尚未整理时存在。

### 4.10 Work Obligation

一个尚未满足、需要 agent、system、user 或 external owner 后续处理的明确条件。
它属于来源 Run 的 Completion Contract，可以由后续 Run 的结构化证据关闭而无需
建立 resume 关系。

**它是描述性记录，不是提醒权威。** Obligation 存在的唯一理由是承载两样 Run 状态
表达不了的东西：解锁条件的文字说明，以及“后续 Run 用证据关闭旧目标”这个跨 Run
能力。它**不决定**某件事是否出现在 Attention 里——那仍由 Run 的执行状态和仍然
存活的控制对象派生（§13.6）。

这条约束是有代价换来的：schema v11 之前，Task 用一个持久化的 `in_progress` 表达
“这件事还没完”，结果是 13 个永不关闭的 open task 和 23 个未认领 Run。**把推断存
起来，它就会和事实漂移。** Obligation 同样是推断（“我们认为还有事没做完”），所以
它不能获得 Task status 曾经拥有的那种权威。

### 4.11 Memory

稳定的个人偏好、习惯和纠正。普通聊天、Task 摘要、运行进度和项目状态不进入
长期 Memory。

## 5. 核心不变量

1. **Run 掌握执行事实。** Task Capsule 不保存 running、waiting、blocked 或 done。
2. **Task 归组可逆。** 自动链接可以修正，用户纠正和 pin 永远优先。
3. **精确恢复不可猜测。** resumes_run_id 只来自显式控制、结构化回边或 Main
   在正常 turn 内选择的精确未完成 Run，并通过网关校验。
4. **相关不等于恢复。** 搜索、观察或引用历史 Task 不写 resumes_run_id。
5. **Run 不依赖 Task 才能创建。** Task Organizer 延迟或失败不能阻止执行。
6. **Task 不授予权限。** Task id、标题、摘要、reference 和 confidence 都不能
   决定 workspace、ExecutionScope、approval 或 tool grant。
7. **前台只有一次 Main 理解。** Organizer 不在 ingress 调用；fast_classifier
   不参与连续性。
8. **活跃输入默认 steer。** user-origin 输入交给活跃 Main；daemon-origin 文本
   不通过自然语言 steer。
9. **跨端共享结构化状态，不共享原始 transcript。**
10. **后台维护只有一条冻结管道。** Task 组织与 Memory 决策共享现有 post-run
    maintenance job，不增加第二个 extractor。
11. **Plan 是辅助，不是权威。** 没有 Plan 也必须能记录、验证和整理工作。
12. **完成以证据为准。** watcher、轮询、人工确认都是策略，不是状态真相。
13. **未解决工作有独立身份，但没有提醒权威。** Work Obligation 可以跨 Run 被
    满足，且不能由 Task 归组或自然语言相似度静默关闭；同时它不是 Attention 的
    必要条件，也不能单独让某件事永久出现在提醒里。Attention 的权威始终是 Run
    状态与存活的控制对象。存下来的推断会漂移，这是 v11 之前的教训。
14. **历史检索完整、默认注入有界。** 没有“最近五条/七天以内才存在”的语义。
15. **迁移不静默丢数据。** 发布用户不能因为所有者允许清空本地历史而被自动清库。

## 6. 前台流程

### 6.1 有 active Run

    user-origin 输入
      -> 持久化到 active Run 的 steer mailbox
      -> Main 在安全检查点读取
           ├─ 补充当前目标：更新理解或 Plan
           ├─ 新增后续工作：增加 Work Unit 或持久化排队项
           ├─ 询问进度：用当前结构化状态回答
           └─ 独立新工作：排队为新的 root Run

Main 可以根据新信息调整执行顺序。即使信息对当前步骤没有帮助，也不能丢弃：
它至少成为当前 Run 的 Work Unit 或新的 durable queue item。

daemon-origin 的 cron、watcher 和恢复通知不能从文本进入此路径；它们使用结构化
origin 和绑定。

### 6.2 没有 active Run

    用户输入
      -> 处理显式命令和结构化 reply
      -> 创建 root Run（此时不要求 Task）
      -> 组装 Active Work Buffer
      -> Main 正常理解
           ├─ 近期上下文足够：直接执行
           ├─ 需要旧历史：work_search
           ├─ 找到候选：work_inspect
           ├─ 只需参考：建立 reference，继续当前 root Run
           └─ 必须恢复未完成执行：请求精确 resume commit

历史搜索命中 Task 只给 Main 参考。普通“继续讨论”“问一下结果”“再做一个类似修改”
都可以新建 root Run 并关联同一 Task，而不认领旧 Run。

### 6.3 精确恢复

精确恢复只在确实需要旧 Run 的 checkpoint、Plan、未完成工具账本或等待状态时发生：

    exact run id / approval id / clarify id / structured reply
      -> person、tenant、状态和未认领校验
      -> 判断执行域与 checkpoint
           ├─ 可在当前解释 Run 安全继续
           │    -> 原子写 resumes_run_id
           │    -> 恢复有界上下文
           └─ 需要不同执行域或完整 checkpoint
                -> 结束解释 Run
                -> 在正确 scope 创建 child Run
                -> child.resumes_run_id = source Run

Task 是否相同不参与 resume 校验。恢复完成后，Task Organizer 可以把两个 Run 的
证据链接到同一 Task，但这是后续语义投影。

### 6.4 跨端

- CLI 正在运行，IM 补充信息：通过 person active Run 进入 steer。
- CLI 已退出但 Run 仍活跃：仍由 daemon active Run 接收 steer。
- Run 已结束，IM 问进度：创建普通 root Run，读取 Active Work Buffer 或 Task；
  不写 resumes_run_id。
- Run 中断，IM 明确要求继续：Main 检索到精确 Run 后请求 resume；必要时切换到
  parent 冻结的 workspace/scope。
- Organizer 尚未完成：Work Spine 和 Run 状态仍然提供近期连续性。

## 7. resumes_run_id 契约

### 7.1 字段语义

    runs.resumes_run_id -> runs.id

- 空值表示这是独立执行尝试，不表示它没有相关 Task。
- 非空表示当前 Run 接管了指定 Run 的未完成执行。
- 每个 child 最多恢复一个 source Run。
- 每个 source Run 最多被一个 child 认领。
- Task Evidence 可以把任意多个相关 Run 聚合到同一 Task，不受这个唯一性限制。

### 7.2 可以写入的来源

- 显式 /resume number|run_id；
- 结构化 approval_id、clarify_id 或 ReplyToRunID；
- watcher/external wait 的结构化 finalization；
- daemon recovery 对精确中断 Run 的恢复；
- Main 在正常 turn 内通过 work_select(action=resume, run_id=...) 请求，并且网关
  验证这是可恢复执行。

以下行为不得写入：

- Task 搜索命中；
- semantic_recall 相似；
- 同标题、同工单号或同 workspace；
- OBSERVE/进度查询；
- Organizer 的 Task 归组；
- 单纯 recency；
- daemon 文本中出现“继续”。

### 7.3 事务校验

写入在一个事务内验证：

1. source Run 存在且属于同 tenant/person；
2. source 状态允许恢复，或属于明确批准的 watcher/recovery 特例；
3. source 尚未被其他 child 认领；
4. child 尚未产生不可重定向的外部效果；
5. 执行域相同，或通过正确 scope 的 transfer child；
6. loop checkpoint 恢复必须在新 Run 开始前完成；
7. approval、clarification 和 watcher 的绑定仍属于 source；
8. 唯一部分索引是跨进程最终护栏。

### 7.4 一次纠正

同域自然语言误选允许一次审计化重指向，但仅在 child 尚未产生 material effect、
未创建审批/澄清/watcher、未发送外部 delivery 且未完成 handoff 时。原事件不删除，
追加 correction 事件。窗口关闭后不能伪装成回滚；系统应停止、说明影响并由用户
选择新路径。

### 7.5 全面命名迁移

实现中一次性迁移以下命名：

| 旧名称 | 新名称 |
| --- | --- |
| parent_run_id | resumes_run_id |
| ParentRunID | ResumesRunID |
| parent run / parent claim | resume source / resume claim |
| idx_task_runs_parent_once | idx_runs_resumes_once |
| ErrParentRunClaimed | ErrResumeSourceClaimed |
| ErrParentRunNotResumable | ErrResumeSourceNotResumable |
| validateParentClaimTx | validateResumeClaimTx |
| parent_run_id event payload | resumes_run_id event payload |

持久 schema 和新事件不双写。升级迁移、冻结旧 maintenance proposal、历史事件读取和
旧 cassette placeholder 可以保留只读兼容；新代码不得继续产生旧字段。

## 8. Task Organizer

### 8.1 输入证据

Organizer 只读取 person-scoped、已冻结的结构化证据：

- 原始 user input 的有界副本；
- Run outcome、done、next steps、files、tests；
- 本 Run 接受的初始输入和 steer 输入，带稳定 input id 与 event cursor；
- Work Unit 的 goal/outcome/verification；
- Task reference 候选；
- 当前 Run 的 workspace logical scope 和 source；
- 本 person 的少量候选 Task Capsule；
- 现有用户纠正、pin、archive 和受保护关系。

它不读取凭证、完整工具输出、系统 prompt、跨端原始 transcript 或其他 person 数据。

### 8.2 输出

每个 Run 的冻结结果包含零个或多个 evidence slices。每个 slice 只允许：

- SKIP：不形成 Task；
- CREATE：创建新的 Task Capsule；
- ATTACH：链接到一个现有 Task；
- RELINK：把自动创建且未受保护的旧链接修正到另一个 Task；
- SUGGEST_MERGE：提出合并候选。

一次 Run 可以产生多个 slices，因此可以被拆成多个 Task。模型同时提供：

- slice summary；
- Task title/objective 建议；
- 关键事实、决定、产出和未解决项；
- 证据引用；
- 简短 reason。

每个 slice 还必须引用提供给模型的 input/work-unit/event cursor 范围。纯自然语言
摘要不能成为无来源的新证据。

Organizer 不输出 workspace、Run 状态、resumes_run_id、approval 或权限决定。

### 8.3 确定性应用

应用层必须验证：

- run/work-unit/task 均属于同 tenant/person；
- 引用 id 来自提供给模型的候选集合；
- candidate ids、摘要 hash、input ids 与 event cursor 范围在 job 中冻结，重放时
  不重新用当前数据库状态悄悄替换；
- 同一 analyzer version + run id 幂等；
- 输出大小、字段数量和 UTF-8 有界；
- summary 不含 secrets 或 raw tool output；
- 用户命名、pin、纠正和手工链接不可被自动覆盖；
- archived Task 不被自动重新打开；
- 自动 merge 只允许未 pin、未命名、全自动来源的 Task；其他情况仅保留 suggestion；
- 任一校验失败时，本次 Task 变更整体不应用，Run 结果保持有效。

### 8.4 Task 摘要更新

当新证据链接到历史 Task 时，Organizer 增量更新：

- objective；
- current digest；
- key decisions；
- constraints；
- outputs and verification；
- unresolved questions；
- next useful action；
- last activity/source。

摘要是可重建投影。Task Evidence 与组织事件是审计事实；摘要损坏时可以从证据重建。

### 8.5 用户纠正

用户可以通过编号或 id：

- rename；
- link/unlink 一次 Run；
- split 一个 Task；
- merge 两个 Task；
- pin/unpin；
- archive/reopen。

用户动作立即生效，并写受保护的 organization event。后续自动维护不得反向覆盖。

### 8.6 调度与降级

- Run finalization 后提交一个现有 post-run maintenance job。
- 连续短 turn 通过 debounce 合批。
- 周期扫描只补偿遗漏和重启，不是唯一触发器。
- 每个 person 保存 organizer high-watermark。
- provider 失败按现有后台退避重试，不阻塞前台。
- 未处理证据继续留在 Work Spine/Run 中；超过上下文预算时使用确定性压缩，但不删除。
- semantic_recall 不可用时，Organizer 和 Main 均可使用本地 FTS 候选。

### 8.7 与 Memory 和 Skill 的关系

同一 post-run 模型结果可以同时携带：

- Task organization decisions；
- preference-only Memory decisions；
- Task reference hints。

确定性 apply 层分别处理，不能跨域授权。Task 借鉴 Skill 的证据、候选、版本和用户
保护思想，但不复用 Skill 的“三次独立验证后发布”门槛：Task 只是可逆历史索引，
Skill 会影响未来执行程序，两者风险不同。

## 9. 上下文组合

### 9.1 默认必须提供

| 切片 | 内容 | 约束 |
| --- | --- | --- |
| Latest input | 当前用户消息 | 唯一权威指令 |
| Channel tail | 当前端点少量最近交互 | 不跨端复制 |
| Active Run card | goal、当前动作、最近证据、等待对象、scope 摘要 | 不含 raw events |
| Unorganized spine | watermark 之后的有界用户/最终回答 | 后台失败时兜底 |
| Task hints | 0–2 个高相关 Task Capsule | 仅参考 |
| Preferences | 与当前请求相关的稳定偏好 | 实际注入后才更新访问时间 |
| Workspace instructions | root-to-leaf 相关规则 | 有界、按优先级 |

### 9.2 仅在精确 resume 时提供

- source Run 的 checkpoint；
- 当前 Plan snapshot；
- 未完成 Work Unit；
- 必需的 paired tool ledger；
- handoff、uncertain effects 和恢复条件；
- source Run 冻结的 execution scope。

### 9.3 按需读取

- 完整 Task 历史；
- 更老的 Work Spine；
- artifact 内容；
- 原始 tool output；
- 详细 Run events；
- 其他 workspace 的状态。

Main 通过 work_search → work_inspect 渐进读取，不在每轮默认注入。

### 9.4 默认禁止

- 全部 Attention 列表；
- 所有未完成 Task；
- 全量跨端 transcript；
- 仅因同 workspace 而相关的历史；
- 完整工具 schema 的文本副本；
- secrets、环境变量和凭证；
- 未经选择的 raw event JSON。

### 9.5 工具 schema 分层

常驻目录只保留高频且通用的最小能力：

- lifecycle：finish_run、update_plan、queue/clarify；
- work history：work_search、work_inspect、work_select；
- workspace：list/search/read、patch、terminal；
- discovery：tool_search；
- 当前安全策略要求的审批能力。

云平台、浏览器、watcher、进程管理、委派、Skill 管理和其他专业能力默认为 deferred。
Main 通过 tool_search 或当前 Skill 激活，激活范围绑定 Work Unit，resume 时从 ledger
恢复。

第一阶段目标不是追求固定 schema 数，而是同时满足：

- 平均 tool schema 占 provider 请求估算 token 低于 20%；
- 常见工作不增加超过一次 tool_search；
- 任务完成率和正确工具选择不下降；
- provider prefix fingerprint 保持稳定，不因每轮候选抖动破坏缓存。

## 10. 通用任务闭环

### 10.1 Completion Contract

每个有真实工作的 Run 应能表达：

- goal；
- acceptance evidence；
- unresolved condition；
- current owner：agent、system、user 或 external；
- attempted strategies；
- next legal strategies；
- uncertain effects；
- deadline/budget（仅在确有来源时）。

**它是投影，不是新的工具参数。** 这八项全部从已有证据算出来：Run outcome、
tool ledger、run_plan_steps、审批／澄清／watcher 行、handoff。**不得**为它新增
`finish_run` 字段，也不得要求 Main 逐项填写。

理由是实测的：现有 `finish_run` 已有 8 个字段（status、summary、done、next_steps、
files、tests、risks、need_approve），而真实 Run 里模型经常只认真填 status 与
summary。再叠一层八字段契约，最可能的结果是字段齐全但内容是套话——那会让完成
判定更不可靠，而不是更可靠。

允许的例外只有一处：当某项确实无法从证据推出、而它对闭环是必需的（例如“只能由
人完成的动作是什么”），才可以让 Main 在**已有**的 `finish_run.next_steps` 里表达，
不新增字段。

### 10.2 Work Obligation

当 Run 结束时仍有必须继续处理的条件，网关持久化 Work Obligation：

- obligation id；
- source run id；
- concise condition；
- owner；
- acceptance evidence contract；
- state：open、resolved、dismissed 或 superseded；
- resolution evidence 和 resolving run id。

审批、澄清和 external wait 继续拥有自己的表，并可引用对应 obligation。后续普通
root Run 如果取得了满足该条件的证据，可以通过结构化 outcome 请求关闭 obligation；
网关验证 evidence 后提交。它不需要写 resumes_run_id，因为它可能只是完成了同一
目标，而没有恢复旧 checkpoint。

Organizer 可以把 obligation 摘要写进 Task Capsule，但不得创建、关闭或转移
obligation。Task 归组错误不能让真实 Attention 消失。

#### 10.2.1 陈旧防护（与 Attention 同一套规则）

`state` 是持久化的推断，所以它必须接受与 Attention 相同的三条约束，否则它会变成
下一个永不关闭的 `in_progress`：

1. **取代。** 同一 source Run 之后出现更新的终态 Run 时，旧 obligation 转
   `superseded`，不需要任何人显式关闭。
2. **证据门槛。** 只有真实留下工作证据的 Run 才能产生 obligation；一次无效果、
   无产出的中断不产生。
3. **时间窗。** 超出窗口且没有任何存活控制对象与之关联的 open obligation 自动转
   `superseded`；真实待答的审批与澄清不受时间窗影响，因为它们由自己的表自证。

**Obligation 不进入 Attention 判定。** Attention 仍然只由 Run 状态与存活控制对象
派生。obligation 的作用是在那些界面上补一句“解锁条件是什么”，以及让后续 Run 能用
证据关闭旧目标。这条边界是本方案与 v11 之前那套 Task 状态的关键区别：一处归组或
判断出错，最坏结果是文案不准，而不是提醒消失或提醒永不消失。

### 10.3 策略选择

当工具或外部等待失败时，系统先记录失败类别和 strategy identity。Main 可以选择
真正不同的策略：

- 修正 cwd、参数、认证或运行环境；
- 使用 provider 原生 wait；
- 一次有界状态查询；
- durable watcher；
- 已管理的后台 process；
- REST、日志、文件或其他只读证据；
- 请求用户完成只能由人完成的步骤；
- 请求用户稍后重新检查。

同一 strategy + target + environment 在没有新证据时不得重复。一个策略耗尽只阻止
该策略，不代表目标无法完成。

### 10.4 状态语义

- waiting_external：SelfMind 仍持有一个 durable owner，未来会自动继续。
- waiting_user：确实需要用户授权、输入、外部操作或接受手工复查。
- blocked：已记录清晰解锁条件，且当前没有安全可行策略。
- verification_partial：执行完成但验收证据不足。
- done：Completion Contract 的必要证据已满足。

“单纯历史 status 不能永久制造提醒”这条要求成立，但它**不通过 obligation** 实现，
而沿用 v11 已经落地并验证过的四条规则：未被 dismiss、未被后续 Run 认领、是所在
分组的最新 Run（更新的终态 Run 取代更早的停驻）、且 `interrupted` 需带工作证据。
这四条把某位用户的未认领 Run 从 23 条降到个位数，是已付过代价的规则，不应换成一个
新的持久化推断（§22.8）。

waiting_external 另有一条硬要求：**watcher 到达任一未观测终态时，其登记 Run 必须
立刻转为 blocked**，不能继续声称“仍有 durable owner 会自动继续”。这一点已在
2026-09-03 落地——在此之前，一个 `blocked_environment` 的 watcher 会让它的 Run 永久
停在 `waiting_external`：watcher 已终态所以没有 monitoring 信号，`waiting_external`
又不在可恢复集合里，于是那份工作不在任何 Attention 集合中。

watcher 注册失败后若没有 durable owner，同理不能标记 waiting_external。系统应换
策略，或清楚地转为 waiting_user/blocked。

### 10.5 平台兼容

策略选择基于 typed capability 和 ExecutionScope，不根据 AWS、GCP、工单号或项目
名写分支。Linux 与 macOS 可以拥有不同可用 capability，但共享状态机和错误类别。

## 11. Plan 语义

### 11.1 新规则

- Main 自己判断 Plan 是否提高执行质量。
- 可以先进行必要发现，再创建 Plan。
- Plan 首次出现时可以包含已有证据支持的 completed 步骤。
- 一个动作满足多个步骤时允许批量完成。
- 可以删除、合并、重排或跳过已证明不需要的步骤。
- 每次 update_plan 仍提交完整 snapshot。
- done 前所有仍展示的步骤必须处于终态。
- 任何 completed 状态必须能关联真实事件或结果，不能补写虚假历史。

### 11.2 删除的强制

实现时删除以下 prompt/UI 假设：

- 第一次 action tool 前必须出现 Plan；
- pending 永远不能直接变为 completed；
- 每次只能推进一个步骤；
- Plan 的 0/N 首屏比真实探索更重要。

UI 可以在首次 Plan 已包含完成步骤时显示“Plan created after discovery”，但不应
伪造早于 Plan 的 plan.updated 事件。

### 11.3 验证重点

衡量 Plan 的指标改为：

- 多步任务完成率；
- 无证据 completed 比例；
- 结束时未解决步骤；
- 中断恢复后 Plan 保真；
- 无效重排和重复步骤；
- 用户是否能理解当前动作。

不再以“首次工具前一定有 Plan”作为质量门禁。

## 12. Module 与接口

### 12.1 RunContinuity Module

负责 resumes_run_id 的全部复杂性：

- 解析显式和结构化 resume source；
- person/state/scope/checkpoint 校验；
- 原子 claim 与一次安全 correction；
- transfer child；
- approval/clarify/watcher/recovery 回边；
- resume context 选择。

Gateway 只提交“尝试恢复 run X”，不自行更新字段或拼接上下文。

### 12.2 TaskHistory Module

负责：

- Task Capsule 创建和读取；
- Task Evidence 链接；
- Organizer delta 的确定性应用；
- user correction；
- search/inspect/list；
- summary rebuild；
- archive/pin。

它不暴露执行状态 setter，也不调用工具或模型。

### 12.3 Attention Module

只从 Run 和存活控制对象派生：

- active Run；
- pending approval / clarification；
- live external owner（pending/running watcher）；
- 满足 v11 现有条件的可恢复 Run：未 dismiss、未被认领、是所在分组的最新 Run、
  且 `interrupted` 带工作证据。

**open Work Obligation 不是这个列表的成员。** 它只为已经在列表里的条目补充解锁
条件文案（§10.2.1、§13.6）。TaskHistory 同理：可以补标题，不能改变 Attention 是否
存在。

这条边界是本 Module 的全部意义所在：Attention 的输入必须全部是自证事实
（正在跑的 Run、真实存在的审批行、真实存活的 watcher、Run 自己的终态），任何
“我们认为还没完”的推断都不得进入。

### 12.4 TurnContextSelector Module

用一个接口构造 Active Work Buffer、Task hints、preferences、workspace instructions
和 exact resume slice。Handler 不直接追加数据库行或 raw event JSON。

### 12.5 Maintenance Organizer

复用现有 PostRunAnalyzer/Apply seam，扩展冻结结果，不再增加一个并行 background
interface。内部可以拆分解析、候选读取和 apply 实现，但对 worker 保持一次 job、
一次版本和一次幂等结果。

### 12.6 接口深度原则

- 调用方不需要理解 Task Evidence、resume claim 或 Attention SQL。
- 测试通过上述 Module 的业务接口验证，不绕过接口断言内部实现。
- 在只有 SQLite 一个真实 adapter 时，不创建仅用于 mock 的仓储 interface。
- 不把这些逻辑重新集中到 gateway 大型 controller/server 文件。

## 13. 目标数据模型

### 13.1 runs

目标字段示意：

    runs (
      id,
      tenant_id,
      person_id,
      workspace_id,
      execution_roots_json,
      channel,
      input_summary,
      resumes_run_id,
      recovery_contract_version,
      status,
      ...
    )

- 不再要求 thread_id/task_id 才能创建 Run。
- resumes_run_id 使用部分唯一索引。
- Run 的子对象优先通过 run_id 所有权关联。

### 13.2 task_capsules

    task_capsules (
      id,
      tenant_id,
      person_id,
      title,
      objective,
      digest_json,
      visibility,
      pinned,
      organizer_version,
      created_at,
      updated_at,
      last_activity_at
    )

digest_json 使用严格版本化结构，至少包含 key facts、decisions、constraints、
outputs、verification、unresolved 和 next action。大型证据不复制进 digest。

Task Capsule 可以跨多个 workspace。workspace 只来自 Task Evidence，用于检索排序
和显示，不成为 Task 所有权或执行授权字段。

### 13.3 task_evidence

    task_evidence (
      id,
      tenant_id,
      person_id,
      task_id,
      run_id,
      work_unit_id,
      slice_key,
      start_event_cursor,
      end_event_cursor,
      relation,
      evidence_summary,
      source,
      protected,
      organizer_version,
      created_at,
      updated_at
    )

- 一个 Run 可以有多个 slice_key，并链接多个 Task。
- source 区分 organizer、user、explicit-reference 和 migration。
- protected 标记用户纠正或 pin 派生的关系。
- 唯一约束防止 maintenance 重放重复写入。

### 13.4 task_organization_events

追加记录 CREATE、ATTACH、RELINK、MERGE、SPLIT、RENAME、PIN、ARCHIVE 和
REBUILD。事件用于审计和重建，不作为执行事件。

### 13.5 task_organization_cursors

保存每个 person 已成功处理的 Work Spine/Run high-watermark。游标只在完整 apply
事务提交后前进；失败不会吞掉证据。

### 13.6 Attention 与 dismissal

增加 person-scoped Work Obligations 与 obligation resolution 记录。

**Attention 的权威不变**：由 active Runs、存活的控制对象（pending 审批、pending
澄清、pending/running watcher）和满足 v11 现有条件的可恢复 Run 派生——未被 dismiss、
未被认领、是所在 Thread 的最新 Run、且 `interrupted` 需带工作证据。Obligation
**不参与**这个判定；它为已经进入 Attention 的条目补充解锁条件文案，并承载跨 Run
的证据关闭能力（§10.2.1）。

这样安排的理由：Run 状态是执行的直接产物，obligation 是对它的推断。让推断决定
提醒，就会重现 v11 之前那类“已经做完却永远在提醒”和“真的没做完却看不到”的成对
故障。

用户隐藏某个提醒时，dismissal 关联精确 run/obligation/control object，不关联 Task。
Task archive 不取消 Run、approval 或外部等待。

### 13.7 公共兼容

- 用户仍使用 task_xxx、/tasks 和 /task。
- 旧 Thread id 在迁移后可复用为 Task Capsule id。
- API 的 task_id 表示语义 Task，可为空；不能再被调用方当作 Run 创建前提。
- HTTP/JSON 新字段使用 resumes_run_id。
- 对冻结历史 payload 的 parent_run_id 只读兼容有明确删除版本，不双写新数据。

## 14. 命令与 UI

### 14.1 分开“历史”和“现在要处理的事”

- /tasks：最近 Task Capsules。
- /tasks search text：完整语义历史。
- /task number|id：Task 摘要和关联 Runs。
- /resume：可精确恢复的 Runs，而不是 Task 列表。
- /status：active Run 或最强 Attention。
- 启动摘要：只显示真实 Attention，不显示普通历史 Task。

编号和稳定 id 都保留。编号绑定端点最近一次可见快照；跨端和重启使用稳定 id。

### 14.2 用户整理

最小命令集合支持 rename、merge、split、link、unlink、pin、archive。低频复杂操作
可以进入 /task detail 的选择界面，不必把所有子命令长期展示在输入提示。

### 14.3 complete 兼容

/task complete 不再表示 Task 完成。迁移期可以：

- 对 Task detail 明示“Task 是历史摘要，没有完成状态”；
- 若用户当前引用的是 Attention Run，引导或兼容为 dismiss exact Run；
- 新文档使用 dismiss/stop/resolve pending object，不继续推广 complete。

## 15. Schema 与数据迁移

目标 schema 版本暂定 v12；实施时若 v11 已公开发布，版本号只前进，不就地修改 v11。

### 15.1 迁移前

1. 停止新 foreground work 并等待安全点。
2. 创建经 fsync 和完整性检查的 control.db 备份。
3. 校验当前 schema 版本受支持。
4. 统计 threads、runs、parent edges、approvals、clarifications、watchers、events、
   artifacts、handoffs、queue、work units 和 memory task sessions。
5. 记录 owner/status 桶，供迁移后比对。

### 15.2 v11 到 v12

一个事务内：

1. 创建 task_capsules、task_evidence、organization events/cursors。
2. 将 listed、pinned、archived 或多 Run 的现有 Thread 迁移为 Task Capsule。
3. 为原 Thread 下每个 Run 写 migration Task Evidence。
4. unlisted 单次 Interaction 不强制创建 Task；Run 和 Work Spine 历史保留。
5. 重建 runs，将 parent_run_id 原值复制到 resumes_run_id。
6. 创建 idx_runs_resumes_once 部分唯一索引。
7. 将现有 pending control objects 与可信 resumable handoff 迁移为 open obligations；
   普通历史 status 不自动制造 obligation。
8. 将 subordinate ownership 收敛到 run_id；无法回填的行进入审计失败，不能静默丢弃。
9. 将 attention dismissal 保留在精确 run/obligation。
10. 删除或重命名旧 threads/关系列，最终 schema 不保留双权威。
11. 写 migration ledger 并提交。

SQLite 不启用外键的事实不能降低校验要求；孤儿和 owner 一致性由显式查询与事务保证。

### 15.3 迁移后校验

- PRAGMA integrity_check；
- Run/approval/clarify/watcher/queue 状态桶不变；
- 每个需要处理的 live control object 都有 obligation 或可解释的直接 Attention 来源；
- resumes edge 数等于原 parent edge 数；
- 无重复 resume claim；
- 无 person/tenant/task evidence 越界；
- events、handoffs、artifacts、work units 无孤儿；
- listed/pinned/archived 历史可检索；
- unlisted Interaction 仍能通过 Run/Work Spine 搜索；
- 旧 Task id 能定位迁移后的 Capsule；
- daemon 重启后 schema/build/version 一致。

### 15.4 测试 fixture

必须包含：

- 最近正式 npm beta 的真实 released fixture；
- 带多个 Thread、多个 Run、parent edge、审批、watcher 和 artifact 的合成 fixture；
- 非空 v11 daily-driver 脱敏 fixture；
- unsupported newer schema 拒绝写入；
- 两个数据库连接并发 claim 同一 resumes_run_id；
- 迁移失败后原库与备份均可恢复。

所有者可以选择 reset 自己的历史来验证空路径，但这不能代替发布升级 fixture。

## 16. 实施批次

批次按“先让执行与恢复正确，再整理语义，最后省上下文”排序。Batch 1 与 2 是本方案
的核心价值，Batch 3 起才引入新的失败模式，所以它们之间有一道证据门（§16.1）。

每个批次给出四件事：**做什么**、**为什么现在做**、**怎么做**、**验收**。

### Batch 0：文档和负例

- **做什么**：本计划成为唯一 active plan，旧 Thread plan 归档；冻结当前
  daily-driver 失败样本与上下文基线；更新领域词汇；建立 schema v12 带数据 fixture。
- **为什么**：后面每个批次都要和一个固定基线比对。基线不冻结，指标就无法证明改善。
- **怎么做**：把 §0.4 的三组实测数字写进基线记录；fixture 见 §15.4。
- **验收**：`docs check` 通过；**没有任何运行行为变化**；`docs/STATUS.md` 不出现
  尚未实现的能力描述。

### Batch 1：精确执行关系改名

- **做什么**：`parent_run_id` 全面迁移为 `resumes_run_id`（§7.5 的完整对照表）；
  RunContinuity Module 收敛校验、claim、correction 和 transfer；移除“同 Task 才能
  resume”的前提；普通 search/reference 不写 resume edge。
- **为什么**：“parent”这个名字让主题关系和执行恢复共用一条路径，这是 v11 没解决
  的根本问题。成本已实测且很低（§0.4），风险主要是遗漏而非行为变化。
- **怎么做**：§7.3 的 8 条事务校验几乎就是现有 `ClaimInteractionContinuation` 已在
  执行的检查，先把它们搬进 Module 再改名，不要一边搬一边改语义。持久 schema 与新
  事件不双写；只保留升级迁移、冻结的旧 maintenance proposal、历史事件读取和旧
  cassette placeholder 的只读兼容。
- **验收**：显式 resume、approval、clarify、watcher、restart 与跨进程唯一认领全绿；
  代码与新数据中不再产生 `parent_run_id`。

### Batch 2：Run 与 Task 解耦

- **做什么**：root Run 不再同步创建 Task/Thread；建立 Task Capsule/Evidence
  schema；Run 子对象按 `run_id` 归属；Attention 完全不依赖 Task 存在；`/resume`
  基于 exact Runs。
- **为什么**：这是唯一能真正消掉“过早归组”的一步。当前实现在 Run 创建之前就要
  确定 Task，于是一次尚未被理解的输入立刻获得了语义容器。
- **怎么做**：先让 Attention 与 `/resume` 不再读 Task（这两处是 v11 刚收敛过的，
  改动面小），再让 Run 创建路径允许 `task_id` 为空，最后建 Capsule/Evidence 表但
  **暂不写入**——写入由 Batch 3 的 Organizer 负责。这样 Batch 2 结束时系统处于一个
  可发布的中间态：有完整执行与恢复，只是还没有自动整理出的工作历史。
- **验收**：没有任何 Task 的 Run 可以完整执行、等待、恢复并正确展示 Attention；
  §0.4 的上下文基线中 `task_runtime` 切片下降且 `estimated_total` 不上升。

### 16.1 证据门（Batch 2 与 Batch 3 之间）

Batch 3 是第一个引入新后台模型调用路径和新失败模式的批次。进入它之前必须先满足：

1. v11 与 Batch 1–2 的行为累计运行满 7 天真实使用；
2. §18.2 连续性指标无回归；
3. 2026-09-02 至 09-04 期间发现的四类问题各自有指标可看且未复发：Plan 出现时机、
   进度动画丢失、watcher 执行绑定缺少 `network:shared`、Attention 语义；
4. §0.4 的上下文基线已按 Batch 2 的结果更新。

**这道门只推迟 Batch 3 及以后，不阻塞 Batch 1–2。** 设它的理由：v11 于 2026-09-02
落地，两天内就暴露了上述四类问题，说明当前行为仍在快速变化；在未稳定的基础上叠加
一个新的后台整理管道，会让故障归因变得不可能。

### Batch 3：后台 Organizer

- **做什么**：扩展同一个 post-run frozen result 与 analyzer version；实现候选检索、
  evidence slices、确定性 apply、cursor 与重试；实现
  CREATE/ATTACH/RELINK/SUGGEST_MERGE；实现用户纠正保护与摘要重建。
- **为什么**：这是“用户不必维护 Task，也有可检索工作历史”的实现体。它必须在后台，
  因为语义归组允许出错，而前台执行不允许。
- **怎么做**：复用现有 post-run maintenance job，**不新增第二个 extractor**
  （§5 不变量 10）。§8.3 的确定性应用是这一批的安全核心：候选 id、摘要 hash、
  input id 与 event cursor 范围在 job 中冻结，重放时不得用当前数据库状态悄悄替换；
  任一校验失败则整次 Task 变更不应用，而 Run 结果保持有效。
- **验收**：一个 Run 可形成零个或多个 Task；**关闭 Organizer 或让它持续失败时，
  前台执行、恢复、Attention 与搜索全部不受影响**（这一条要有专门的降级测试，不能
  只靠推理）。

### Batch 4：上下文和工具目录

- **做什么**：Active Work Buffer 替代默认 Thread attach；exact resume slice 与普通
  Task hints 分离；Task cards 限为 0–2 个，完整历史按需读取；常驻/deferred 工具
  重新分层，保持 Work Unit 激活与 resume 重建。
- **为什么**：实测（§0.4）显示工具 schema 占请求估算 token 的 28–45%，是
  `task_runtime` 的约 6 倍。**省 token 的唯一有效动作是工具目录分层**；Task 卡片
  收敛的价值在于减少误注入，不在省钱。两者不可混为一谈，否则力气会花在便宜的
  那一半上。
- **怎么做**：按 §9.5 分层；先让 `tool_search` 的发现路径可靠，再把专业能力转
  deferred，顺序反了会出现“能力不可达”。激活范围绑定 Work Unit，resume 时从
  ledger 重建。
- **验收**（改为绝对指标，不再用“预算达标”这种无法判定的表述）：
  - tool schema 占 provider 请求估算 token 的**平均值低于 20%**，且与 §0.4 基线
    （28–45%）相比有可测量下降；
  - 常见工作不增加超过一次 `tool_search`；
  - 任务完成率与工具选择正确率不下降；
  - provider prefix fingerprint 稳定度不下降（候选抖动不得破坏缓存）；
  - `semantic_recall` 降级与 Organizer backlog 不阻塞 Main。

### Batch 5：通用完成闭环和 Plan

- **做什么**：**投影** Completion Contract（§10.1），持久化 Work Obligations 与
  resolution evidence（§10.2）；失败 guard 从“禁止继续任务”收窄为“禁止无新证据
  重复同一策略”；watcher 失败后允许其他策略；放松 Plan 首次时点与逐步更新强制，
  保留证据真实性。
- **为什么**：实测（§0.4）显示 Plan 与 watcher 都无法充当完成机制——12–21 次工具
  调用的 Run 可以零 Plan 完成工作，watcher 也会因执行环境而不可用。真正需要持久化
  的是目标、验收证据、责任方、未解决条件与已尝试策略。
- **怎么做**：Completion Contract **必须是投影**，不得新增 `finish_run` 字段或要求
  Main 逐项填写（§10.1 给出了理由与唯一例外）。Work Obligation 按 §10.2.1 接受
  取代、证据门槛与时间窗三条约束，并且**不参与 Attention 判定**（§13.6）。
- **验收**：外部 wait、无 watcher、权限受限、unknown effect 与 late Plan 场景通过；
  **另加一条负例**：一个已由用户在系统外完成的 obligation 不会永久留在 open 状态。

### Batch 6：命令、UI、清理与发布证据

- **做什么**：`/tasks`、`/resume`、`/status` 分离展示；用户 Task correction 操作；
  删除 Thread/current-task/parent 命名与旧 compat 写路径；更新 docs、eval cassettes、
  doctor、daily report 与 npm smoke。
- **为什么**：删除旧写路径是“只有一个事实源”的收尾。留着双轨会在下一次重构时再次
  产生本方案 §2.1 描述的那种混杂。
- **怎么做**：先改展示与用户操作，再删旧路径；删除前确认没有新代码仍在产生旧字段。
- **验收**：完整 `selfcheck`、Linux/macOS、真实 CLI↔IM、restart 与本地安装验证通过。

每个 Batch 必须可以独立构建和验证；不得以“后续 Batch 会修好”为理由提交已知错误
的执行路径。

## 17. 验收矩阵

| 场景 | 预期 |
| --- | --- |
| “你好”直接回答 | 有 Run/Spine，无 Task，无 Attention |
| 一轮完成代码修改 | Run done；Organizer 创建 Task；不需要用户 complete |
| 一个请求包含两个独立目标 | 一个 Run、多 evidence slices、两个 Task |
| 活跃 CLI Run 中 IM 补充条件 | durable steer；Main 调整当前工作；不丢消息 |
| 活跃 Run 中输入独立新工作 | durable queue；当前工作不被错误切换 |
| CLI 完成后 IM 问进展 | 参考 Task/Run 回答；resumes_run_id 为空 |
| 明确继续中断 Run | 新 Run 写 resumes_run_id，恢复 checkpoint/Plan |
| 相似历史但只是新工作 | 可链接同 Task，不能认领旧 Run |
| 新 Run 取得旧目标的完成证据 | 关闭旧 obligation；不伪造 resumes_run_id |
| Organizer 延迟/失败 | Active Work Buffer 仍支持近期跨端理解 |
| semantic_recall 失败 | 本地 search/inspect 可用，前台不降级 |
| 错误自动归组 | 用户 unlink/relink 后永久保护 |
| pinned Task 被建议 merge | 只产生 suggestion，不自动修改 |
| watcher 可用 | durable owner，完成后结构化恢复 |
| watcher 不可用但可查询 | 换 read/status 策略，不重复注册 |
| 只有用户可解除权限 | waiting_user，说明动作和验证方式 |
| 探索三步后才创建 Plan | Plan 可带已有 completed 证据，UI 不伪造历史 |
| 无 Plan 的多轮讨论 | 仍可形成 Task 和 Completion evidence |
| v11 带数据升级 | 备份、迁移、重启、owner/orphan 不变量全过 |
| 两进程同时恢复同一 Run | 只有一个 claim 成功 |
| 用户在系统外完成了某个 obligation | 被更新的终态 Run 取代或超时后自动 superseded，不永久 open |
| obligation 为 open 但无存活控制对象 | Attention 不因它出现条目；解锁条件只在已有条目上作为文案 |
| 关闭 Organizer 后连续使用一天 | 执行、恢复、Attention、搜索全部正常；只是没有新 Task |

## 18. 指标与发布底线

### 18.1 Task 整理质量

- Task CREATE/ATTACH/RELINK/SUGGEST_MERGE 数量；
- 用户纠正率；
- false merge / false split；
- 一个 Task 被后续成功找到的比例；
- Organizer backlog 年龄与重试；
- SKIP 后仍能从 Run/Spine 找到的比例。

不再使用 parent/resume edge 命中率衡量语义理解。

### 18.2 连续性

- active steer 接纳和排队分流；
- exact resume 成功/拒绝/correction；
- 跨端 progress question 无错误 claim；
- restart 后 checkpoint/Plan 恢复；
- stranger/tenant 隔离。

### 18.3 上下文

对照基线是 §0.4 的实测值（`tool_schemas` 10,593–11,022；`history` 6,265–11,295；
`task_runtime` 1,730–1,770；`estimated_total` 24,023–38,497）。每项都要给出与该
基线的对比，而不是只报绝对值。

- provider request 总 token；
- **tool schema token 与占比**（第一优先，门槛见 Batch 4 验收：平均低于 20%）；
- Task hint、Spine、workspace、memory、artifact 各切片；
- tool_search 次数；
- prompt prefix hash 稳定度；
- 截断后关键目标、未解决条件和路径保留率。

### 18.4 完成质量

- done 且证据满足；
- verification_partial；
- waiting_external 是否有 durable owner；
- open obligation 的年龄、owner 和无证据关闭率；
- **被取代／超时自动转 superseded 的 obligation 数**（§10.2.1 是否真的在防陈旧）；
- **obligation 为 open 但 Attention 无对应条目的数量**（应为常态，用于确认
  obligation 没有获得提醒权威）；
- watcher/strategy 失败后是否出现新策略；
- 无新证据重复调用；
- 用户被要求介入的比例和必要性；
- 自动恢复后的最终闭环率。

### 18.5 快速迭代发布底线

不要求独立开发者等待多日才能发布，但每个 beta 必须守住：

- build、test、完整离线 eval；
- 带数据 migration fixture；
- 安装包启动、doctor、daemon restart；
- CLI active steer 与 exact resume；
- 一个跨端结构化场景；
- 一个 Organizer 失败降级场景；
- 一个 watcher 不可用替代策略场景；
- 无已知的数据丢失、权限越界或重复外部效果。

真实 daily-driver 指标用于发现下一轮问题，不成为无法执行的固定等待天数。

## 19. 回退与故障行为

- Organizer 关闭或失败：停止更新 Task，Run/Spine/Attention/搜索仍工作。
- semantic_recall 失败：使用本地 FTS，不阻塞。
- Task 摘要损坏：从 Task Evidence 重建。
- Task 归组错误：用户 correction，不回滚 Run。
- resume 校验失败：保持当前 root Run，不加载旧 checkpoint，不猜测。
- schema 迁移失败：停止启动新 daemon，保留原库和验证过的备份。
- watcher 不可用：换策略；没有自动 owner 时转 waiting_user/blocked。
- Plan 不存在：Completion Contract 和 Run outcome 仍能完成与恢复。

回退不能恢复旧的 Thread 执行权威、parent_run_id 新写入或 ingress classifier。

## 20. 文档影响

实施时需要同步更新：

- CONTEXT.md；
- AGENTS.md 的 Tasks/Context 与 Plan/Completion 不变量；
- docs/STATUS.md；
- docs/work-timeline.md；
- docs/identity-continuity.md；
- docs/context-lifecycle.zh-CN.md；
- docs/coding-agent-foundations.md 及中文翻译；
- docs/tool-safety.md；
- docs/eval-loop.md；
- docs/command-reference.md 及中文翻译；
- docs/adr/0004 的 superseded 状态，并新增一份“语义 Task 与执行恢复分离”的 ADR；
- docs/manifest.yaml 和生成的 docs/README.md。

在对应 Batch 尚未完成前，current domain/status 文档不得提前描述目标能力为已实现。

## 21. 完成条件

只有同时满足以下条件，本计划才能归档：

1. 新代码和 durable 数据不再产生 parent_run_id。
2. Run 可以在没有 Task 的情况下完整执行和恢复。
3. Task 与 Run 是可审计、可修正的多对多证据关系。
4. Organizer 使用唯一后台维护管道，失败不阻塞前台。
5. /tasks 与 /resume 不再混淆工作历史和可恢复执行。
6. Plan 不再被强制在首次工具前出现，completed 仍有真实证据。
7. watcher 失败不会导致无意义重复，也不会伪造自动继续所有权。
8. 后续 Run 可以用证据关闭旧 obligation，而无需伪造 resume。
9. Attention 的输入全部是自证事实；open obligation 既不是它的成员也不是它的必要
   条件，且被取代／超时的 obligation 会自动 superseded（§10.2.1、§22.8）。
10. Completion Contract 是投影：没有为它新增任何 `finish_run` 字段（§22.9）。
11. tool schema 占 provider 请求估算 token 平均低于 20%，且相对 §0.4 基线
    （28–45%）有可测量下降，完成率与工具选择正确率无回归。
12. v11 带数据升级、备份、恢复、孤儿和跨进程 claim 测试通过。
13. 完整 selfcheck、npm smoke、Linux/macOS 和真实 CLI↔IM 证据通过。

## 22. 明确拒绝的替代方案

### 22.1 继续强化 Thread promotion

拒绝。它仍要求先创建 Thread，再从工具、Plan 或状态猜测是不是 Task，无法表达
一个 Run 属于多个语义 Task。

### 22.2 用 LLM 在 ingress 先选择 Task

拒绝。它在上下文加载前判断上下文，增加一次前台调用，并可能影响执行权威。

### 22.3 删除 resumes_run_id，只靠 Task 恢复

拒绝。Task 是可修正语义，无法保护 checkpoint、审批、幂等认领和跨进程并发。

### 22.4 把所有消息写入 Memory

拒绝。工作历史与个人偏好不同，混合会污染召回并增加治理成本。

### 22.5 强制所有长任务使用 watcher

拒绝。不同平台和权限下可用能力不同，正确目标是持久 owner 和完成证据。

### 22.6 强制 Plan 先于所有动作

拒绝。必要探索本身是真实工作，Plan 应提高完成质量而不是满足 UI 顺序。

### 22.7 同时保留 Thread 和 Task Capsule 两套写模型

拒绝。迁移可以临时复制，但最终运行版本只能有一个语义工作历史权威。

### 22.8 让 open obligation 成为 Attention 的权威或必要条件

拒绝，理由是已经付过一次代价。schema v11 之前，Task 用一个持久化的 `in_progress`
表达“这件事还没完”，结果是 13 个永不关闭的 open task 和 23 个未认领 Run；修好它
用掉了取代规则、证据门槛、时间窗和同频道排序四条规则。

Obligation 与那个 `in_progress` 是同一类东西——**存下来的推断**。它可以描述解锁
条件，可以承载跨 Run 的证据关闭，但一旦让它决定“是否提醒”，就会同时产生两种
故障：已经做完的事永远在提醒，真的没做完的事因为 obligation 缺失而看不到。

Attention 的输入必须全部是自证事实。见 §10.2.1、§12.3、§13.6。

### 22.9 为 Completion Contract 新增 finish_run 字段

拒绝。现有 `finish_run` 已有 8 个字段，实测中模型经常只认真填 status 与 summary。
再加八项只会得到字段齐全、内容套话的结果，使完成判定更不可靠。Completion Contract
必须从 Run outcome、tool ledger、plan steps 与控制对象投影得出（§10.1）。
