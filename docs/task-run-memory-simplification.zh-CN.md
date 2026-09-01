# Task–Run 连续性与偏好记忆简化方案

> 生命周期：私有实施参考与迁移记录（不进入公共文档索引）  
> 范围：Phase 1 的任务、运行、跨端接续、上下文选择与个人偏好记忆  
> 说明：本文不是 active plan，不改变当前能力声明；实施进度仍以
> [STATUS.md](STATUS.md) 和唯一 active plan 为准。  
> 修订：2026-08-31 依据代码核对与 5 天真实运行证据修订（见 2.1 节与
> 各阶段前置条件）。
> 当前核对：P0–P3 的主要行为已落地；统一的 `WorkTimeline` 深 Module 提取、
> 各真实 IM 传输的 reply 关联证据与完整 P4 场景仍未完成，不能据本文宣称整体
> Definition of Done 已满足。

## 1. 结论

SelfMind 面向用户只保留两个长期概念：

- **Task**：帮助用户查找、理解和恢复一段工作。
- **Memory**：保存用户明确表达的长期偏好、习惯和纠正。

**Run** 是内部执行记录。它不需要成为新的用户概念，但必须成为连续性、恢复、
审计和上下文选择的事实来源。所谓“工作历史”不是第四种对象，而是一个 Task
下面按时间和父子关系排列的 Runs。

```text
Person
├── Task
│   └── Run
│       └── parent_run_id
└── Memory
```

本方案采用一个深的 **WorkTimeline Module** 隐藏任务解析、精确接续、原子认领、
跨端映射和候选消歧；入口只暴露小的 Interface。Memory 独立为偏好记忆，不再承担
项目知识、任务状态或运行进度。

一句话原则：

> Prompt 决定怎样帮助用户；确定性内核决定哪些事实不能判断错。

## 2. 为什么需要简化

当前实现已经具备 person 级 work spine、Task、Run、handoff、审批、澄清、语义召回
和异步维护，但有多条机制同时尝试回答“这句话属于哪项工作”：

- `current_task` 和临时 Task 预标记；
- continuation cue、`/resume` 和显式 task id；
- Task References 与关键词匹配；
- post-run 的 `KEEP / MOVE / NEW / TITLE / INBOX`；
- `resumed_by_run_id` 与 `task_blockers`；
- Task 级 summary、handoff、events 和 artifacts 上下文。

这些机制分别有合理用途，但组合后会产生三个根本问题：

1. **展示分组可能改变执行语义。** 一次错误 Task 归类可能把另一条工作的完整上下文
   带入当前 Run。
2. **继续关系有多个事实来源。** Run 的反向认领、Task blocker 和自然语言判断可能
   对同一次等待给出不同答案。
3. **记忆承担了太多职责。** 用户偏好、项目约定、任务历史和临时运行事实容易混在
   一次抽取与召回流程中，增加误写和过期风险。

简化目标不是删除历史能力，而是把每类事实只交给一个权威来源。

### 2.1 变更前基线（2026-08-31）

以下是实施前代码（v0.1.0-beta.20）与 2026-08-27 至 08-31 共 5 天真实运行数据，
用于解释迁移动机，不描述本文修订后的当前能力：

- 完整 Task 上下文实际经由三条通道进入模型，后两条不受 attach 模式约束：
  `context_selector.go`（系统提示）、`withResumeContext`（用户消息前置块，
  还会把 `resumed_by_run_id` 盖章到按任务猜测的 run 并 resolve 其 blockers）、
  `withLoopCheckpointResume`（按“任务内最新未完成 checkpoint”整体替换消息
  数组，与父边无关）。
- 运行证据：31 个 Runs 全部落入 1 个 Task（预标记 25、continuation cue 5、
  新建 1）；post-run 标注器 28 次决策全部为 KEEP，未纠正任何明显错误归类；
  `resumed_by_run_id` 实际写入 0 条；14 个 blockers 全部保持 open；个人偏好
  Memory 五天产出为 0（54 次 memory_extract 调用：47 成功、7 失败）。
- approval 恢复目前生成 “Resume task after parked approval …” 文本，再靠
  continuation cue 正则命中；clarify 入站按 person 最老 pending 行认领。二者
  都不是结构化 run 回边——本方案 5.3 第 2 步是新建设，不是收敛。
- `api.MessageRequest` 没有 `ReplyToRunID` / `ApprovalID` / `ClarifyID`
  字段；eval Turn schema 也没有 reply 字段。
- cron 与 watch 走同一 `ProcessMessage`；提示词若命中 continuation cue 会
  steer 用户当前 active Run。这是确定性缺陷，P0 修复。
- 确定性 cue 解析器可识别“开始执行”，但漏掉“开始执行，不需要去确认……”这类
  真实变体；该语料必须进入 eval。
- `task_handoffs` 没有 `run_id` 列；生产唯一写入方（run finalize）使用
  `handoff_run_<run_id>` 作为主键，可无损回填正式列。

## 3. 目标与非目标

### 3.1 目标

- 无工单号、无代码仓库、无职业术语时仍能正确创建和继续工作。
- TUI、IM、cron 和 HTTP 共享 Task、Run 和 Memory，原始聊天记录仍留在各自端点。
- 继续一个等待中的工作时，只加载被明确选中的 parent Run 的运行上下文。
- 多个候选并存时不猜测，返回简短候选让用户选择。
- Task 的错误命名、合并或显示排序不改变 Run 的继续关系和执行权限。
- Memory 只保存用户偏好；用户纠正、遗忘、固定和取消固定始终优先。
- 快速迭代时可以分阶段上线，每一阶段均可独立验证和回退。

### 3.2 非目标

- 不增加 SaaS、Runner 或多人协作模型。
- 不共享不同端点的原始 transcript、token delta 或完整工具日志。
- 不用 ingress LLM classifier 判断 Task 或 parent Run。
- 不通过工单号、文件路径、标题相似度、model summary、`work_key` 或
  `current_task` 指针认领一个未完成 Run。
- 不自动语义拆分历史上已经混合的 Task。
- 不在这项工作中重新设计执行权限、审批底线或 workspace scope。
- 不引入独立 `work_history` 表或新的用户命令概念。

## 4. 领域模型和不变量

### 4.1 Person

`person_id` 表示跨端点的同一个人，是 Tasks、Runs 和个人 Memory 的分区键。
`account_id` 仍只表示某个平台绑定，不能替代 `person_id`。

### 4.2 Task

Task 是可逆的工作标签和用户查找入口，而不是上下文边界。

Task 可以被重命名、合并、固定、归档或改变显示优先级。以上操作都不得重写
Run 的 parent 链，也不得扩大 workspace、credentials 或 approval authority。

Task 的状态、摘要和下一步是 Runs 与待处理人机交互的**派生投影**。为查询性能可以
继续缓存到 `tasks` 表，但缓存不是事实来源，必须能够重建。

### 4.3 Run

Run 是一次不可变身份的执行记录。结果、handoff、artifacts、events、验证状态和
待处理人机交互都归属于 Run。

每个非根 Run 最多有一个 `parent_run_id`；每个 parent Run 最多被一个 child Run
认领。唯一继续边为：

```text
child.parent_run_id -> parent.id
```

不得同时维护 `parent.resumed_by_run_id` 作为第二个权威写入方向。现状核对：
`resumed_by_run_id` 只有一个写入点（`withResumeContext` 内的
`MarkTaskRunsResumed`），唯一读取用途是 `soleUnresolvedRun` 的“未被认领”
过滤，真实数据为 0 条；它不是正向 continuation authority。迁移期间它转为
只读兼容。

### 4.4 Memory

Memory 只保存用户的长期偏好、习惯和明确纠正，例如：

- “默认用中文回答。”
- “先给结论，再给证据。”
- “在这个 workspace 中，变更前先给我看方案。”

以下内容不进入个人偏好 Memory：

- 当前任务状态、待办、构建结果和临时故障；
- 文件内容、仓库事实、项目命令和工程约定；
- 工单、订单、课程或家庭事项本身；
- tool output、secret、credential 和未经用户确认的推断。

项目与环境知识继续由 workspace conventions 负责；工作进度由 Run、handoff、
events 和 artifacts 负责。

### 4.5 核心不变量

1. 一个 person 同时最多有一个 executing Run；新工作进入 durable queue，明确继续
   才 steer active Run。
2. 创建 child Run 与认领 parent Run 在同一个事务中完成。
3. `parent_run_id` 唯一索引阻止 crash、重试或多端竞态制造分叉。
4. 没有精确 parent 时，不得加载某个 Task 的完整历史上下文。
5. Task 选择错误最多影响展示，不能影响 workspace、权限或 parent ownership。
6. approval 与 clarify 使用自身结构化记录精确回到 origin Run，不依赖 prose parsing。
7. Memory 召回失败不得阻塞前台 Run；Memory 也不得改变 Task/Run 归属。
8. 所有 person 数据查询必须同时带 tenant 与 person 分区。

## 5. WorkTimeline Module

### 5.1 Module 职责

`WorkTimeline` 是一个深 Module：它把复杂度藏在内部，只让 TUI、IM、cron 和 HTTP
Adapter 提供平台事实。

Module 内部负责：

- active Run 的 steer 或新工作排队；
- approval、clarify 和 reply metadata 的精确映射；
- 显式 Task、`/resume`、同 channel 与 person 全局候选解析；
- 新 Task 与根 Run 创建；
- child Run 创建和 parent 原子认领；
- workspace 继承与执行 scope 输入；
- 多候选消歧；
- Run-scoped context 选择；
- 完成后的 outcome、handoff、artifact 和 Task 投影更新。

### 5.2 小 Interface

建议的 Interface 形状如下。名称可在实现时按现有 Go 约定微调，但语义不可拆散到
各个 Adapter：

```go
type WorkTimeline interface {
    BeginTurn(context.Context, BeginTurnRequest) (TurnBinding, error)
    CompleteRun(context.Context, CompleteRunRequest) error
    FindTasks(context.Context, FindTasksRequest) ([]TaskCard, error)
}

type BeginTurnRequest struct {
    TenantID      string
    PersonID      string
    Platform      string
    Channel       string
    Content       string
    WorkspaceID   string
    ExplicitTask  string
    ReplyToRunID  string
    ApprovalID    string
    ClarifyID     string
    ContinueCue   bool
}

type TurnDecision string

const (
    TurnSteerActive   TurnDecision = "steer_active"
    TurnQueueNew      TurnDecision = "queue_new"
    TurnStartRoot     TurnDecision = "start_root"
    TurnContinueChild TurnDecision = "continue_child"
    TurnChooseParent  TurnDecision = "choose_parent"
)

type TurnBinding struct {
    Decision    TurnDecision
    TaskID      string
    RunID       string
    ParentRunID string
    Candidates  []RunCandidate
}
```

`ContinueCue` 来自共享命令/确定性 cue parser，可识别多语言的明确接续表达；它不是
职业关键词分类，也不能猜测模糊语义。Adapter 只传递平台能够证明的
`ReplyToRunID / ApprovalID / ClarifyID`，不得自行查询或绑定 Task。

这三个是需要新建的 wire 字段：当前 `api.MessageRequest`、IM Adapter 元数据、
durable queue payload 和 eval Turn schema 都没有它们。P1 同时补齐这四处，并
删除 approval 恢复的 prose 路径（改为基于 `claimed_by_run_id` CAS 的结构化
origin-run 回边）。

### 5.3 BeginTurn 的确定性顺序

顺序本身是产品契约，所有端点共用：

1. **存在 active Run**
   - 精确 reply 或明确 continuation cue：steer active Run，不创建 Run。
   - 其他输入：作为新工作 durable queue，不污染 active Run。
2. **approval / clarify id 命中**：通过结构化行找到 origin Run，创建或恢复它的
   child Run。
3. **`reply_to_run_id` 命中**：以该 Run 为唯一 parent。
4. **显式 task id 或 `/resume`**：
   - 该 Task 只有一个未被认领的 resumable Run：继续它；
   - 有多个：返回 Run candidates；
   - 没有：在该 Task 下创建 root Run，不伪造 parent。
5. **明确 continuation cue 且同 channel 有待处理 Run**：只有唯一候选时继续。
6. **明确 continuation cue 且 person 全局有待处理 Run**：只有唯一候选时继续。
7. **存在多个候选**：返回 `TurnChooseParent`，不启动模型、不加载完整 Task context。
8. **没有候选**：创建新 Task 和 root Run。

普通新话题不会因为当前 Task、相同标题、相同路径或相同工单样式而自动附着。

实施前差异：第 2 步当时不存在（clarify 按最老 pending 认领、approval 恢复走
prose cue）；第 7 步的候选返回当时也是全新交互（多候选会报错终止本轮）。当前
代码已经用结构化 approval/clarify 回边和确定性 Run candidates 替换这些路径；
其中 gateway restart 会保留 pending clarify，回答与携带 `ClarifyID` 的队列行在同一
事务提交，终态或已被 child 认领的 origin 则使旧问题过期。

### 5.4 候选内容

候选必须简短且不泄漏其他人的数据，至少包含：

- Task title；
- Run 的短 input summary；
- 状态与等待原因；
- 来源端点和更新时间；
- 可复制的短 id 或序号。

选择结果必须落到精确 `run_id`，不能只返回 `task_id`。

## 6. Run-scoped context

当前 `TaskRuntimeContext` 会在 full attach 时读取 Task 最新 handoff、events、artifacts
和 blockers。目标结构改为：Task 只提供显示和 scope 元数据；工作上下文从精确
parent Run 及其派生记录选择。

```text
RuntimeContextBundle
├── Task metadata: id, title, workspace id
├── Parent Run slice
│   ├── outcome / handoff / next steps
│   ├── changed files / artifacts
│   ├── unresolved plan and work units
│   ├── uncertain tools and verification evidence
│   └── bounded relevant events
├── person work-spine tail
├── workspace conventions
└── preference Memory
```

选择规则：

- `parent_run_id` 存在：选择 parent Run 的 bounded slice。
- root Run：不读取 Task 全历史；只使用 work spine tail、workspace conventions 和
  preference Memory。
- 用户仅提到某个 Task：可提供 bounded Task card 作为参考，但不视为 continuation。
- 语义召回结果始终是 advisory，不能成为 parent、workspace 或 permission authority。
- 原始 tool protocol、system prompt 和 token stream 只留在 Run events。

这样即使历史 Task 曾经混入多个主题，新 Run 也不会得到整张 Task 的不相关上下文。

边界定位与既有机制：

- parent Run slice 是执行连续性的唯一权威。person work spine 保留为有界、
  非权威的近期对话参考（跨 Task/端点的最近轮次窗口），不因本方案删除。因此
  P0 的验收口径是“prompt 中不得出现无关 Run 的 handoff、artifacts、loop
  checkpoint 与完整 events”，而不是“prompt 中完全不存在其他近期文字”。
- loop checkpoint 恢复必须按精确 parent Run 读取；禁止按“Task 内最新未完成
  checkpoint”选择。
- `task_handoffs` 需要正式 `run_id` 列（P1 迁移新增）。回填可按生产主键前缀
  `handoff_run_<run_id>` 无损解析；字符串 ID 本身不作为领域关系使用。
- 语义召回移入 `RuntimeContextBundle` 的独立召回预算，不再挤占 Task 切片预算
  （修复 events 总是最先被截断的问题）。

## 7. Task 的展示、搜索和治理

### 7.1 不再创建独立 Work History

用户查看一个 Task 时，直接展示其 Runs。跨 Task 查找使用 person 级 Task/Run 搜索；
不新增 `work_history` 表，也不复制 outcome 或 handoff。

### 7.2 显示优先级是派生值

为避免每个一次性问答都挤满 `/tasks`，默认列表按以下信号提高优先级：

- 有多个 Runs；
- 有未解决的 approval、clarify、外部等待或 resumable Run；
- 有 artifact、plan、next step 或用户命名；
- 用户固定；
- 最近活跃。

单次完成的普通问答仍可搜索和审计，但默认排在后面。这个规则只影响展示，不改变
保留期和连续性。

### 7.3 去掉自动路由职责

post-run maintenance 不再输出 `KEEP / MOVE / NEW / INBOX` 来决定 Run 属于哪个
Task。未显式指定 Task 的新 root Run 创建新 Task，child Run 继承 parent Task；
Task rename/merge 是显式治理或显示操作。

Task References 不再参与自动路由。现有 reference 可以保留为用户 alias 或搜索提示，
但不能认领 parent Run、加载完整上下文或改变 workspace。

`current_task` 可以保留为 UI convenience projection；它不能成为 continuation authority。

## 8. PreferenceMemory Module 与自定义 Prompt

### 8.1 Memory Interface

Memory 与 WorkTimeline 之间只有窄 Seam：Run 完成后提交经过过滤的用户证据；开始
Run 时按 person/workspace 有界召回。Memory 不读取或修改 Task routing。

```go
type PreferenceMemory interface {
    Propose(context.Context, PreferenceEvidence) ([]PreferenceCandidate, error)
    Apply(context.Context, []PreferenceCandidate) error
    Recall(context.Context, PreferenceRecallRequest) ([]Preference, error)
}
```

实现约束：不新增同形的第二层 Interface。现有缝已经存在——
`PostRunAnalyzer.Analyze`、`PostRunAnalysisApplier.Apply`、
`CanonicalStore.ApplyIntakeWrite`——P3 通过收窄这三层实现上述语义；本节的
Interface 形状只描述语义契约。显式入口先行：新增跨端 `/remember <text>` 与
`/forget <text|ref>` 的确定性路径（现状只有 `/memory pin <text>` 与按 ref 的
forget），再收窄自动候选。

第一阶段只允许以下证据产生自动候选：

- 用户直接表达偏好；
- 用户明确纠正已有偏好；
- 用户明确要求记住、忘记、固定或取消固定。

“多次观察推断习惯”暂缓，直到独立证据计数、矛盾处理、可解释展示和一键撤销都通过
验收。Similarity 只能提出候选，不能授权合并。

### 8.2 Scope

- `global`：跨所有 workspace 的个人偏好。
- `workspace:<logical-id>`：用户明确限定在某个逻辑 workspace 的个人偏好。

仓库命令、构建方式和代码规范不写入 PreferenceMemory；它们由确定性的 convention
discovery 提供。

长期工程约定的去向是仓库约定文件（`AGENTS.md`、`.selfmind.md` 等，经
workspace knowledge 投影）；聊天中的隐式项目事实不自动持久化。若未来需要承接
“用户明确确认的 workspace convention”，作为独立类别另行设计，不混回个人偏好
Memory。P3 停止 `target="memory"` 自动写入时，同时给存量 environment 行定义
显式归档策略。

### 8.3 自定义 Prompt 能控制什么

自定义 Prompt 适合吸收不同用户和不同生活场景的差异：

- 回答语言、语气、详略和输出结构；
- 展示确认步骤的偏好；
- Task title/summary 的措辞；
- 在允许的偏好类别中，哪些内容值得向用户提出记忆建议；
- 用户自己的领域术语和表达习惯。

自定义 Prompt 不能控制：

- identity、tenant/person ownership；
- parent Run 认领与候选消歧；
- workspace、execution scope、credentials 和 approval floor；
- secret、临时状态和 tool output 的记忆禁区；
- forget/correct/pin/unpin 的优先级；
- context source、预算与截断顺序。

优先级固定为：当前用户明确指令 > 用户自定义 Prompt > 已确认 Memory > 默认行为。
低层内容不得覆盖高层内容。

不可控项的测试锚点（P3 落地）：目标与耐久性由确定性 apply 层强制
（`TestIntakeDecisionPolicy`、`TestIntakeDurabilityEnforcement`——对冻结提案
重放同样生效）；瞬态禁区跨三个入口一致（`TestRememberRefusesTransientRunState`、
memory 工具 add、intake episodic 丢弃）；forget/pin/纠正优先
（`TestRememberAndForgetAreCrossEndpointAndDeterministic`、consolidation 的
受保护簇拒改）。提示词工作区的锁定段落校验独立存在，Prompt 内容无法移除
以上任何一层。

## 9. 跨端同步契约

同一 person 跨端共享：

- Tasks 与 Task display projection；
- Runs、`parent_run_id`、outcomes 和 handoffs；
- approvals、clarifies 和 external watches；
- artifact references；
- PreferenceMemory；
- durable queue 与恢复通知状态。

保持端点本地：

- 原始 transcript；
- token delta 和逐步渲染；
- 输入框草稿与 UI 状态；
- 平台原始 payload；
- 未经筛选的工具输出。

IM 平台支持 reply metadata 时，Adapter 应把它映射到 durable `run_id`。平台不支持时，
候选选择和 `/resume` 仍提供通用退路。

## 10. 数据变更与迁移

### 10.1 Schema

向 `task_runs` 添加：

```sql
ALTER TABLE task_runs
ADD COLUMN parent_run_id TEXT NOT NULL DEFAULT '';

CREATE UNIQUE INDEX idx_task_runs_parent_once
ON task_runs(tenant_id, parent_run_id)
WHERE parent_run_id <> '';

ALTER TABLE task_handoffs
ADD COLUMN run_id TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_task_handoffs_run
ON task_handoffs(run_id)
WHERE run_id <> '';
```

唯一索引刻意省略 person_id/task_id：run id 全局唯一，追加它们只会放宽约束。
父子在 tenant、person、Task 上的一致性与“parent 状态可继续、未被认领”由创建
child Run 的同一事务从数据库重新读取并校验（schema 无外键，事务是唯一防线），
不能依赖默认空值绕过校验。原草案中的 `idx_task_runs_task_parent` 属于冗余
索引：认领查询由唯一部分索引承担、任务内列举已有 `idx_task_runs_task_started`，
除非 query plan 证明必要，否则不加。落法遵循 v3 迁移先例：列只进 ordered
migration（v7），不进 baseline。

### 10.2 兼容迁移

1. 按发布升级规范备份并验证 legacy `control.db`。
2. 添加 `parent_run_id` 和索引，fresh DB 与 released upgrade fixture 都测试。
3. 对现有 `parent.resumed_by_run_id = child.id` 的精确关系，回填
   `child.parent_run_id = parent.id`。预期回填近空（真实数据为 0 条）：把
   “大量历史关系进入 audit 桶”写成预期结果而非异常。
4. `task_handoffs.run_id` 按主键前缀 `handoff_run_<run_id>` 回填；无法解析的
   行保持空值并记录 audit finding。
5. 冲突、缺失或跨 person 的历史关系不猜测，只记录 audit finding。
6. 新代码只写 `parent_run_id`；`resumed_by_run_id` 暂时只读兼容。
7. 至少跨一个已发布版本确认无旧写入后，再单独迁移删除旧字段。
8. 维护分析器契约变更（如移除 `task_decision`）必须同时处理版本兼容：
   `MigrateMaintenanceJobsToVersion` 只迁移未完成 job 且原样复制冻结
   payload/proposal。要么提供版本化 proposal decoder 与兼容 Apply，要么在
   切换前显式归档旧版本未完成 job；不得让旧格式 proposal 以新语义重放。

### 10.3 `task_blockers`

`task_blockers` 从 routing、status 和 prompt authority 中移除，但第一阶段不删表。
通用等待状态由 childless resumable Run 派生；approval、clarify 与 external watch 继续
使用各自结构化表。旧 blocker 只供兼容展示和 audit，确认无读取者后再删除。

### 10.4 历史 Task

不对历史 Task 做自动语义拆分或 maintenance replay。提供只读 dry-run audit：

- 无法回填的 resume edge；
- 一个 parent 被多个 child 指向；
- 跨 person / Task 的非法关系；
- 无 owner 的待处理 approval/clarify；
- Task projection 与 Runs 不一致。

audit 不修改 Memory、不移动 Run、不删除 artifact。

## 11. 代码落点

实施时优先替换现有路径，不在旧逻辑外再叠一层：

| Module / Seam | 主要位置 | 变更方向 |
| --- | --- | --- |
| Control store | `internal/control/store.go`, `task_labels.go` | schema migration、parent 原子认领、派生查询 |
| WorkTimeline | `internal/gateway/httpapi/run_coordinator*.go` | 收敛 `resolveTask`、continue resolver、StartRun 和 completion |
| Context selector | `internal/gateway/httpapi/context_selector.go` | 从 Task-full 改为 parent-Run slice |
| Runtime context | `internal/kernel/task_runtime_context.go` | 承载 typed Run context；`kernel` 不依赖 gateway/control |
| Post-run maintenance | `internal/gateway/httpapi/run_labeler.go`, `maintenance_worker.go`, `internal/app/post_run_analyzer.go` | 移除 Task 路由决策，只保留合规的 PreferenceMemory 决策 |
| Task search/view | `internal/control/task_*.go`, `internal/gateway/httpapi/task_view*.go` | 派生状态、显示优先级、Run timeline |
| Adapter metadata | `internal/gateway/cli`, `weixin`（活跃入口） | 只传 reply/approval/clarify/run facts |
| Legacy 路径 | `internal/gateway/router`, `telegram`, `wechat`, `channel`, `internal/kernel/task/manager.go` | 拆分而非整删：`RunAgentWithEvents` 等模型执行能力仍被 `run_coordinator` 使用，先提取到正确 Module；删除死的 raw `Handle`/`Bridge`/旧 TaskManager 与遗留 `current_task` 表接线 |
| Eval | `evalcases/timeline`, memory eval cases | 通过生产消息路径验证跨端和消歧 |

如果 `run_coordinator` 继续增长，应把 WorkTimeline 实现提取到聚焦文件；不要把规则复制
到每个 Adapter，也不要让 `kernel` 依赖具体 store。

## 12. 分阶段实施

### P0：先切断错误上下文

范围：

- 在创建 Run 之前先做只读 parent 解析（复用 `soleUnresolvedRun` 谓词）；
- 三条注入通道同时按精确 parent 门控：selector（系统提示）、resume context
  （用户消息）、loop checkpoint（消息数组）；没有精确 parent 时都禁止
  Task-full 注入；
- 有精确 parent 时，handoff/events/artifacts/plan/checkpoint 全部按该 parent
  Run 读取（P0 内 handoff 经 `handoff_run_<id>` 主键读取，正式列留给 P1）；
- root Run 只获得 Task 展示元数据、workspace conventions、偏好 Memory 和有界
  work-spine tail；
- 同一 Task 内 continuation cue 命中多个未完成 Runs 时，返回确定性候选列表，
  不启动模型（person 全局候选留给 P1 的 BeginTurn）；
- cron/watch/background origin 永远不能通过文本 cue steer 用户 active Run；
- 语义召回移入独立 bundle 预算；
- `resumed_by_run_id` 认领从 resume context 组装中拆出，作为 run 创建后的
  显式步骤（继续作为 P1 回填的证据来源）；
- 保持现有 Task 展示和 schema 不变。

可观察完成条件：两个未完成 Runs 位于同一 Task 时，模糊“继续”返回确定性候选且
不调用模型；任一新 Run 的 prompt（系统提示、用户消息、恢复的消息数组）不含
无关 Run 的 handoff、artifacts、checkpoint 或完整 events；cron 提示词含 cue
词不 steer 用户 run。

回退：恢复 selector 与 resume/checkpoint 门控，未发生数据迁移。

### P1：建立唯一 parent edge 和 WorkTimeline Interface

范围：

- v7 migration：`parent_run_id`、唯一部分索引、`task_handoffs.run_id` 与
  回填、upgrade fixture；
- `BeginTurn` 的确定性解析和原子 child 创建（事务内重新校验 parent 的
  tenant/person/Task 一致与可续、未被认领状态）；
- 新 wire 字段：`ReplyToRunID`/`ApprovalID`/`ClarifyID` 贯通
  `api.MessageRequest`、IM Adapter、durable queue payload 和 eval Turn
  schema；
- approval/clarify 结构化回边：基于 `claimed_by_run_id` CAS 精确回到 origin
  Run，删除 “Resume task after parked approval …” prose 路径与最老 pending
  认领；
- 旧 `resumed_by_run_id` 回填与只读兼容。

可观察完成条件：并发两次继续同一 parent 时，只有一次成功创建 child；另一次得到
已认领结果，不产生分叉。并发测试至少覆盖两个独立 SQLite 连接（daemon 单连接
串行化不构成证据），跨进程写入方（CLI maintenance、eval runner）在清单内。

回退：新字段保留但停止新写入；旧读取仍可工作。不得回滚数据库版本后继续写。

### P2：简化 Task

范围（内部顺序是硬约束，逆序会产生 Task 永不终结、Reference 只升不降、
一次性问答挤满列表等回归）：

1. 所有 Task status/summary 写入收敛到现有派生 reducer
   （`resolveFinalTaskStatusTx`），去掉自由写 setter；
2. 移除 `PreserveTaskLifecycle` 的延迟终态提交（现状把预标记 task 的终态提交
   推迟到标注器 KEEP 路径）；
3. 默认列表 visibility/显示优先级投影落地；
4. 冻结 Task Reference 自动促升（其负证据回路依赖将被删除的 MOVE 结果）；
5. 移除 `KEEP / MOVE / NEW / INBOX` 路由与 INBOX 机制（含配置、`/diag`、治理
   统计等挂点），同步 bump `postRunAnalyzerVersion` 并按 10.2 第 8 条处理版本
   兼容；未显式指定 Task 的 root Run 创建 Task，child Run 继承 Task；
6. `current_task` 从 continuation authority 降级为 UI convenience
   projection，References 降级为 alias/search hint（同时修改 `attachPolicy`：
   `reference_continuation` 不得再给 full context 与 UpdateCurrentTask）。

投影重建走 CLI dry-run/apply 通道（沿用 `selfmind maintenance task-audit`
先例）；迁移不变量校验禁止在 schema migration 中改变 `tasks.status` 桶计数。

可观察完成条件：错误 rename/merge 或相似标题不能改变后续 parent Run；一次性问答
不会挤占默认 Task 列表，但仍可搜索。

回退：保留原表列，切回旧展示投影；不能恢复双向 parent 写入。

### P3：收窄 Memory 并接入自定义 Prompt

范围（内部顺序）：

1. 先建显式确定性入口：跨端 `/remember <text>`、`/forget <text|ref>`（保留
   现有 `/memory` 治理子命令）；
2. 自动候选收窄为明确偏好与纠正证据，通过收窄现有
   Analyze/Apply/ApplyIntakeWrite 缝实现，不新增同形 Interface；
3. project/run/transient 的 SKIP 落在确定性 apply 层（prompt 层约束在冻结
   proposal 重放时无效，已有生产事故先例）；
4. 维护分析器版本兼容处理（见 10.2 第 8 条）；
5. 停止 `target="memory"` 自动写入并定义存量 environment 行的归档策略；
6. 固定 Prompt 可控项与不可控项。

可观察完成条件：用户在一个端点明确偏好后，另一端点可召回；构建状态和临时任务
不会被写为偏好。

回退：停止自动候选，保留显式 Memory 读写和既有审计记录。

### P4：兼容清理

范围：

- dry-run audit 后清理无读取者的 legacy 字段和 blocker authority；
- 拆分而非整删 `router.Gateway`：提取 `RunAgentWithEvents` 等仍被使用的模型
  执行能力，删除死的 raw `Handle`/`Bridge`/旧 TaskManager 接线与
  `internal/kernel/task/manager.go` 的遗留 `current_task` 表；
- 清理已证明的死代码：`buildRunLabelPrompt`、`kernel.FactExtractor`/
  `TurnExtractor` 及其仍可设置的配置键；
- 删除被新 Interface 覆盖的测试，而不是并存两套断言（run_labeler /
  post_run_analyzer / memory_intake / maintenance_batch 相关约 60 个测试）；
- 更新 `work-timeline.md`、`memory-governance.zh-CN.md`、`STATUS.md` 和公开命令文档。

可观察完成条件：代码与文档中只剩一个 parent authority；全量 selfcheck 与 legacy
upgrade fixture 通过。

## 13. 验收矩阵

| 场景 | 期望 |
| --- | --- |
| 同 channel：“先准备，等我确认” → “开始” | 新 Run 精确指向等待 Run；只加载该 parent context |
| TUI 准备 → Telegram 回复确认 → TUI 查看 | 同一 person、Task 和 parent chain；结果可跨端查找 |
| 同一 Task 有两个待处理 Runs → “继续” | 不调用模型执行；返回两个 Run candidates |
| 等待期间输入无关新话题 | durable queue 新 root Task，不 steer/attach 等待 Run |
| reply metadata 指向旧 Run | 精确继续该 Run，不受 `current_task` 影响 |
| 重命名或合并 Task 后继续 | parent chain 不变，workspace/approval authority 不扩大 |
| 中文、英文或无职业关键词的 continuation cue | 使用同一确定性规则；不要求工单号 |
| 学习、写作、购物、家庭事务和软件开发 | 领域词只影响 Prompt 表达，不影响 routing 规则 |
| 用户说“以后先给结论” | 形成可审计偏好候选，并可跨端召回 |
| Run 中出现“build failed” | 不写入 PreferenceMemory |
| 两端并发认领同一 parent | unique index 保证只有一个 child |
| crash 发生在 child 创建前后 | 重试幂等；不丢 parent，也不产生双 child |
| Memory provider 失败 | foreground Run 正常完成，maintenance 可观察地失败/重试 |
| cron/watch 提示词含 continuation cue | 不 steer 用户 active Run；按 origin 隔离 |
| “开始执行，不需要去确认线上是否执行过了” | 与“开始执行”走同一确定性接续结果（真实语料回归） |

## 14. 测试与发布证据

### 14.1 Go tests

- 通过 `WorkTimeline` Interface 测试 `BeginTurn / CompleteRun / FindTasks`，避免断言
  内部调用顺序。
- Control store 使用本地 SQLite，覆盖事务竞态、person 隔离、projection rebuild 和
  schema migration。
- released legacy database fixture 覆盖备份、回填、重新打开和 unsupported-newer
  schema 拒绝写入。
- Adapter tests 只验证平台 metadata 的规范化，不复制 routing tests。
- 替换已失效的 pre-label/labeler tests，不保留互相矛盾的旧测试层。

### 14.2 Eval cases

重复出现的消息路径缺陷必须进入 `evalcases/`，并走生产入口：

- 同端精确继续（断言精确 parent run，而不止 `require_same_task`）；
- 跨端 reply（依赖 P1 的 eval Turn reply 字段）；
- 多候选消歧（确定性候选、不调用模型，`model_required: false`）；
- 新话题不误续（替换现有钉死 pre-label 行为的用例与 Go 测试）；
- 无工单/无职业术语；
- 中英文明确 cue（含真实未命中语料，如“开始执行，不需要去确认……”）；
- 偏好跨端召回与临时事实拒写（`assert_state on: memory` 原语已存在）。

Model-backed case 必须有 replay cassette；纯确定性 case 声明
`model_required: false`。Mock success 不能作为发布证据。

### 14.3 Gate

每个阶段至少执行：

```text
selfmind selfcheck --fast
```

合入或发布前执行完整：

```text
selfmind selfcheck
```

涉及消息路径时检查对应 eval；涉及 store 时检查 legacy upgrade fixture。CI 只承担本机
不能证明的证据，不重复运行没有新增价值的同一 gate。

## 15. 观测、上线与回退

只记录结构化、无敏感正文的指标：

- `turn_binding_decision` 各类型数量；
- candidate count 与用户选择率；
- parent claim conflict / duplicate prevention；
- root Run 比例与默认列表可见率；
- Run context source 和预算使用；
- Memory candidate 的 ADD/SKIP/CONFLICT 与 provider failure；
- legacy edge 回填、冲突和未解析数量。

建议使用逐阶段 feature switch，只控制新旧读取路径，不允许双写两个 parent authority。
每阶段先 shadow 比较确定性决策，再对 daily-driver 开启，最后删除旧路径。回退应优先
停用新读取或自动维护；数据库新增字段保持向前兼容，不做破坏性降级。

## 16. Definition of Done

只有同时满足以下条件，方案才算完成：

- 用户长期概念只有 Task 与 Memory；Run 作为 Task 内历史呈现。
- `child.parent_run_id` 是唯一 continuation authority。
- 所有端点通过同一个 WorkTimeline Interface 解析新建、排队、steer、继续和消歧。
- 无精确 parent 时不会注入 full Task context（系统提示、resume 用户消息与
  loop checkpoint 三条通道一致）。
- cron/watch/background origin 不能通过文本 cue steer 用户 Run。
- Task routing 不再依赖 post-run LLM labeler、Task References 或职业标识。
- Task 状态和显示优先级可从 Runs 与结构化等待记录重建。
- Memory 只存用户偏好，project/run/transient facts 被拒绝。
- 自定义 Prompt 的可控项和安全不可控项有测试。
- legacy DB 升级、crash window、并发认领、person 隔离和跨端场景都有证据。
- `selfmind selfcheck`、文档契约和相关 eval 全部通过。

## 17. 与现有文档的关系

- 产品连续性不变量见 [identity-continuity.md](identity-continuity.md)。
- 当前 Task/Run/recall 机制见 [work-timeline.md](work-timeline.md)。
- Context 预算与 prompt 路径见
  [context-lifecycle.zh-CN.md](context-lifecycle.zh-CN.md)。
- Memory 写入、冲突、衰减与撤销机制见
  [memory-governance.zh-CN.md](memory-governance.zh-CN.md)。
- 包归属和依赖约束见 [architecture-constraints.md](architecture-constraints.md)。

本文保留设计取舍、迁移顺序和未完成的 Definition of Done；当前能力始终以
[STATUS.md](STATUS.md) 与对应领域文档为准。实现变更必须同步更新
[work-timeline.md](work-timeline.md) 的 continuation/context 小节与
[context-lifecycle.zh-CN.md](context-lifecycle.zh-CN.md) 的 selector 切片清单。
