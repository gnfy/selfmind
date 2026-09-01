# SelfMind 记忆治理机制

> 状态：当前机制。2026-07-11 的缺陷与数据保留为历史基线；后文标注的
> 2026-08-31 P2/P3 修订覆盖早期 task-routing 与 environment-memory 设计。
> 整合来源：记忆系统缺陷诊断、两阶段实验与 Intake Contract。
>
> 本稿不是优先级清单；实施排期以 `docs/STATUS.md` 为准。

---

## 0. 背景：诊断结论与实测基线

### 0.1 缺陷清单（2026-07-11 实测确认）

| # | 缺陷 | 位置 | 严重度 |
|---|------|------|--------|
| D1 | pinned 记忆从不进入 prompt：`buildSystemPrompt` 只读 `user`/`memory` target；唯一消费 pinned 的 `ProfileSynthesizer.MaybeSynthesize` 无任何调用者（死代码） | `internal/kernel/agent.go:1734`、`profile_synthesizer.go` | 高（功能 bug） |
| D2 | 重复确认不强化：写入去重命中后静默丢弃，不刷新 `LastVerifiedAt`/confidence；`RepetitionBoost` 是死代码。被反复证实的事实与一次性过时事实按同样的 90 天半衰期衰减，置信度体系形同虚设 | `post_run_analyzer.go` `duplicatePostRunFact`、`governance.go` | 高 |
| D3 | 历史缺陷：唯一的“是否值得写”判断是 prompt 指令 + ≥160 字符资格门，且无 REINFORCE/SUPERSEDE；现由 v4 偏好限定与确定性 apply 修复 | `post_run_analyzer.go`、`memory_intake.go` | 已修复 |
| D4 | 自动抽取写入不进 learning audit：`storeFacts` 不调 `recordMemoryLearningChange`，`/memory history` 与 undo 覆盖不到自动写入 | `post_run_analyzer.go` | 高 |
| D5 | agent 的 `memory` 工具 add/replace/pin 走 legacy `AddFact`：无 source/scope/confidence 元数据；replace 丢原 ID 与元数据 | `internal/tools/memory.go` | 中 |
| D6 | 存储无界：无归档/上限/合并 apply；衰减只影响选择分数，数据只增不减 | 全局 | 高 |
| D7 | 陈旧 profile 注入：合成器断线但 `latestProfileSummary` 仍每轮注入残留 profile（实测主分区存在 1 条，2026-06 底生成后永不更新） | `agent.go` | 中 |
| D8 | 双相似度实现并存漂移：`memory_view.equivalentMemoryText`（0.55/3-token）与 `consolidation.ConsolidationSimilarity`（0.42/CJK bigram）算法、归一化、停用词均不同 | `memory_view.go`、`consolidation.go` | 中 |
| D9 | 检索/分类对中文不友好：`/memory search` 纯子串匹配，分类关键词英文为主；中英互述永远判不重、搜不到 | `memory_view.go` | 中 |
| D10 | 注入预算固定：`maxFactsEach=20`×2 每轮全量注入，不随模型窗口缩放、无 per-turn 相关性 | `agent.go:1748` | 低 |

### 0.2 实测数据基线（2026-07-11）

- 主分区 `person_3477f062…`：**781 条** facts（2026-06-21 → 07-10），来源构成：
  空 source 299 / `fact_extractor` 293 / `turn_extractor` 189（已废弃抽取器的
  数据残留，全部 0.5 低置信碎片）/ `profile` 1（D7 残留）。
- legacy `default` 分区：**190 条**（2026-06-17 → 06-21，person 分区化之前的
  真实历史数据，source 全空）。06-21 为分区切换日。
- 只读 dry-run（complete-link 0.42）：781 条中 **497 条（64%）落在 166 个候选
  簇**；最大簇 12 条（同一 workspace root 事实的变体）。纯合并上界 ≈ 450 条；
  叠加情景噪声归档后预期收敛至 **120–200 条 active**（与 Codex 预分析 ~190 的
  结论相互印证）。
- `protected = 0`：用户从未 pin 过任何记忆——印证产品目标：治理跑起来之后
  pin 应当趋近于零，而不是要求用户日常运营。
- 288 个分区中 eval 残留分区 facts 均为 0（数据隔离生效），迁移不涉及，但建议
  `selfmind eval clean` 清理目录。

---

## 1. 核心原则

五句话（产品序）：

1. **原始证据不可变，规范记忆可修订。**
2. **模型只提出判断，策略层决定是否生效。**
3. **后台自动治理是主路径。**
4. **人类管理是安全出口。**
5. **遗忘必须立即停止召回。**

工程红线（与 `AGENTS.md` 既有裁决对齐）：

- **相似度只提名，永不决策。** 任何字符串/向量相似度只产生候选，"是否同一
  事实"的语义判断只属于显式配置的便宜模型（`memory_extract` role，绝不回退
  主 coding model）。
- **每个 run 至多一个维护结果，但多个 run 可以共用一次模型调用。** run 终态
  事务只持久化证据；daemon 按 tenant/person/workspace 防抖分组，批量调用一次
  便宜模型，再按 run_id 冻结和应用各自的偏好决策与 Task Reference 候选。
  Task 路由不属于维护分析器。不得新增第二条抽取/决策通路。显式的用户
  memory 命令仍即时执行；最近对话由 work spine 即时承接，不依赖治理完成。
- **pinned 与 `SourceUser` 对自动维护不可变。** 自动流程对其只允许
  KEEP/CONFLICT。
- **一切自动改动可审计、可撤销。** 归档不等于删除；物理删除只发生在 forget
  的隐私保留期之后。
- **不引入知识图谱软件/外部记忆服务。** 关系语义用 SQLite 内的三类"边"
  （supersedes / merged_into(evidence) / conflicts_with）表达；embedding 后置于
  `httpapi.RecallSource` v2 seam，到位后只替换检索层，判断层与策略层不动。

总体数据流：

```
Run 进入终态 ──(FinishRun 事务/恢复 sweep)──▶ maintenance_job
  → 确定性资格门 → 确定性邻居召回 → 一次廉价模型判断(preference+reference hint)
  → 确定性策略校验 → observations / canonical / evidence / events 落库
定时 Consolidator ──▶ 同一"召回→判断→校验"管道（存量整理）
用户 correct/pin/forget ──▶ 直接进策略层（最高权威）
Canonical(active) ──▶ Context 注入与召回
```

---

## 2. 数据模型（P0）

四张新表 + 一张作业表。**只做增量迁移，不改现有 `facts` 表结构**；旧读取路径
保留一个版本，确认稳定后删除。

### 2.1 `memory_observations` — 不可变证据

```
id, tenant_id(person), workspace_id
run_id, analyzer_version
content, normalized_hash          -- sha256(normalizeConsolidationText(content))
target(user|memory), scope, source(user|agent|fact_extractor|legacy|...)
confidence_prior                  -- BaseConfidence(source)
created_at
status: candidate | accepted | retracted | forgotten
```

关键细节：

- observation **一旦写入永不 UPDATE 内容**；状态字段仅表达证据的采信情况。
- **同一事实被不同 run 再次观察到 = 新的 observation**（这正是"重复即证据"
  的载体，驱动 REINFORCE）；**同一 run 的重试不得产生新 observation**——由
  maintenance_jobs 的幂等约束保证（见 2.5），不在 observation 上做唯一约束。
- `normalized_hash` 用于确定性去重网第 1 层（见 3.4）与迁移期跨分区判重。

### 2.2 `canonical_memories` — 当前有效认知（读模型）

```
id, tenant_id(person), workspace_id
content, category, target, scope
status: active | conflicted | superseded | archived | forgotten
pinned(bool), user_confirmed(bool)
confidence, evidence_count, occurrences
last_verified_at, last_accessed_at
valid_from, valid_until           -- 时间有效性（SUPERSEDE 写 valid_until）
superseded_by                     -- canonical id
revision, created_at, updated_at
```

关键细节：

- **元数据继承规则**（MERGE 产生新 canonical 时）：`created_at`/`valid_from`
  取最早成员（保留"从那时起就知道"的时间线）；`last_verified_at` 取最新；
  `confidence = RepetitionBoost(成员最高先验, evidence_count)`；`occurrences`
  求和。
- `category` 由治理流程打标后**存字段**（一次性由 judge 批量打标 + 增量在
  intake 判断时顺带产出），`/memory` 视图退化为确定性读取——同时消灭 D8 的
  双实现漂移与 D9 的英文关键词偏置。
- `last_accessed_at` 在注入/召回命中时更新，但 **context 组装是热路径，禁止
  同步写**——命中记录进内存队列，由后台批量刷库（丢失可接受，仅影响归档
  排序）。

### 2.3 `memory_evidence` — 证据关系

```
memory_id(canonical), observation_id
relation: supports | contradicts | supersedes
created_at
```

一条 canonical 的 `evidence_count` = supports 关系数。`max_evidence_per_memory`
（默认 20）超限时按最旧裁剪 supports 关系（observation 本体保留）。

### 2.4 `memory_events` — 审计

记录 create/merge/reinforce/supersede/conflict/correct/archive/forget/restore/
undo，载荷包含：决策来源（intake|consolidator|user）、模型置信度、涉及的
canonical/observation id 集、**被改动前的完整快照**（undo 的依据）。替代并入
现有 learning audit 通道（`/memory history`、undo 走同一模型），修复 D4。

### 2.5 `maintenance_jobs` — 幂等维护作业

```
run_id, analyzer_version
status: pending | running | succeeded | failed
attempts, next_retry_at, result_hash, last_error
UNIQUE(run_id, analyzer_version)
```

关键细节：

- **创建时机挂进现有 run 生命周期，不新建通路**：`FinishRun` 终态事务内创建
  pending job；`MarkInterruptedRuns`（boot sweep + 60s 巡检）为被恢复的 run 创
  建受限 job（只允许 SKIP/ADD，不做 SUPERSEDE——中断 run 的 outcome 不完整，
  不足以判定"替代"）。控制命令（`/help`、`/status` 等）是 pre-agent 的，根本
  不产生 run，无需 analyzer 侧再排除。
- 终态并发用 CAS（`WHERE status='running'` 影响零行则放弃）防重复处理；旧 run
  延迟完成只允许更新自己的 job，不得触碰新 run 的结果。
- `analyzer_version` 语义：算法/prompt 升级后 bump 版本号，可对历史 run 重跑
  维护而不与旧结果冲突（新版本 = 允许新 job）。
- 审批等待中的 run 不学习，等进入终态。

---

## 3. 写入路径：intake（P0，核心解决 D2/D3/D4/D5）

### 3.1 确定性资格门（不花钱）

直接跳过：run 失败且 outcome 无任何有效证据；或输入/结果短到不足以形成候选且
outcome 无结构化字段。当前语言无关下限为：输入至少 6 个 Unicode code points、
结果摘要至少 8 个、合计至少 16 个；因此“以后先给结论”这类紧凑 CJK 偏好不会
再被旧的 160 字符门槛漏掉。门槛只决定是否值得进行异步廉价分析，不判定语义、
不影响前台路由。每 run 最多保留 **3 条 user 决策**。

### 3.2 确定性邻居召回（不花钱）

用 turn 文本（user input + outcome summary）在同 person、同 target+scope 的
active canonical 中检索：

```
neighbors = top-8 by ConsolidationSimilarity(turn文本, canonical.content)
          ∪ 同 target+scope 最近写入的 5 条
```

**"最近 5 条"是跨语言补丁**：CJK bigram 与英文 token 检索互不相交，但跨语言
重复几乎总发生在时间邻近的 run 之间；把最近条目无条件塞进邻居集，让模型（跨
语言无障碍）来判重。embedding 到位后此处整体替换为向量检索，下游不动。

### 3.3 一次模型调用（偏好与引用提示合并，红线）

同一次 post-run 调用只产出 Task Reference 候选与个人偏好决策；它不产出、
不重放任何 Task 路由决定：

```json
{
  "memory_decisions": [
    {"target": "user", "decision": "REINFORCE", "ref": "663fc723", "content": "用户偏好先给结论", "confidence": 0.95, "durability": "durable"},
    {"target": "user", "decision": "SUPERSEDE", "ref": "f2d994ee", "content": "用户现在偏好简短回答", "confidence": 0.98, "durability": "durable"}
  ]
}
```

prompt 附上同 person 的偏好邻居集（带 id），指令要求对每条候选在
SKIP / ADD / REINFORCE:&lt;id&gt; / SUPERSEDE:&lt;id&gt; / CONFLICT:&lt;id&gt;
中裁决；项目事实、仓库约定、命令与运行状态必须 SKIP，且确定性 apply 会再次
拒绝非 `user` target。turn 文本包在数据分隔符里按不可信数据处理。

### 3.4 确定性策略校验与落库（不花钱）

模型不能写库；每个决策过下表（校验思路照抄 `matchOpenLabel`：**模型输出的 id
必须在 3.2 提供的邻居集内**，它永远碰不到没被展示过的记忆）：

| 决策 | 生效条件（全部满足） | 落库动作 | 校验失败降级 |
|------|----------------------|----------|--------------|
| SKIP | — | 不写 | — |
| ADD | 过三层去重网（下） | 新 observation(accepted) + 新 canonical(active) + supports 边 | — |
| REINFORCE | ref∈邻居集；confidence≥0.90 | 新 observation + supports 边；canonical 不改文本，`last_verified_at=now`、`occurrences++`、`confidence=RepetitionBoost(...)` | ref 无效→ADD；低置信→仅存 observation(candidate)，canonical 不动 |
| SUPERSEDE | ref∈邻居集；confidence≥0.98；旧条非 pinned/user_confirmed；同 scope | 旧 canonical `status=superseded`、`valid_until=now`、`superseded_by=新id`；新 canonical + supersedes 边 | ref 无效→ADD；**旧条受保护→自动降级 CONFLICT** |
| CONFLICT | ref∈邻居集 | 双方保留 active，新 canonical `status=conflicted` + contradicts 边；进入 `/memory conflicts` | ref 无效→ADD |
| 无法解析 | — | 按 ADD 处理（最坏 = 现状多一条重复，后台合并兜底） | — |

**三层去重网**（ADD 前的确定性拦截；确定性层只处理"同一性"，同义性归模型）：

1. **归一化精确**：`normalized_hash` 相等 → 自动转为对已有 canonical 的
   REINFORCE（无需模型确认——归一化相等就是同一句话）。
2. **包含关系**：≥12 字符且一方包含另一方 → REINFORCE 信息更全的那条。
3. **语义**：邻居集 + 模型的 REINFORCE 决策（3.3）。漏网的交给后台合并。

所有非 SKIP 决策写 `memory_events`。分层可分别度量：第 1/2 层纯函数单测，第
3 层用端到端 eval case（"同一事实换措辞说三遍 → 库里一条、occurrences=3"）。

### 3.5 agent `memory` 工具并轨（修 D5）

工具的 add/replace 不再直写 facts：生成 observation（source=agent）走同一策略
层（agent 主动保存视为已通过资格门，但仍过去重网与保护校验、仍写审计）。
`/memory pin` 语义改为 `pin <id>`（对既有 canonical 置位）+ 保留
`pin <text>`（直接创建 user_confirmed canonical）。replace 保留原 id 与元数据。

---

## 4. 后台整理：consolidation（P1，解决 D6 与存量）

### 4.1 两阶段

**第一阶段（确定性，只提名）**：同 person、target、scope 分区 → complete-link
聚类（簇内任意两条相似度 ≥0.42 才成簇，避免链式误聚）→ 候选簇。现有只读实验
代码（`internal/kernel/memory/consolidation.go`）即此阶段，继续作为门禁复用。
检索信号首版 = 文本相似度 + FTS + 文件路径；embedding/实体后置（本仓库尚无该
基建，接 `RecallSource` seam 后替换）。

**第二阶段（judge，每簇一次调用）**：`memory_extract` role 输入簇成员
（id+文本+来源+时间），输出 KEEP / MERGE / REINFORCE / SUPERSEDE / CONFLICT /
ARCHIVE + canonical 文本 + member_ids + confidence。

### 4.2 apply 规则

- **merge-only 的应用集 = MERGE + REINFORCE + ARCHIVE**（2026-07-12 owner 决
  定：历史库以测试数据为主，整理放开到这三类）。SUPERSEDE 永远只报告不应用——
  intake 面对新证据裁决 supersede；后台 pass 没有新证据，无权贬低旧信念。
- 自动生效条件（同时满足）：同 scope；不涉及 pinned/user_confirmed；无未决
  冲突；输出结构合法且 member_ids 全部属于该簇（`ValidateConsolidationDecision`
  已实现）；confidence 达标：**MERGE ≥0.95（`auto_merge_confidence`）、
  REINFORCE ≥0.90（`auto_reinforce_confidence`）、ARCHIVE ≥0.90
  （`auto_archive_confidence`）**（模型置信未校准，阈值是调参旋钮）；MERGE 的
  canonical 文本通过"无新信息"近似校验；**REINFORCE 的落库文本必须逐字等于某
  个成员原文**（忽略大小写/空白，写入成员原文而非模型措辞——REINFORCE 因此比
  MERGE 更安全，不产生任何模型创作的文本）。
- **shadow 报告即干跑**：shadow 模式对每个判决跑与 merge-only 完全相同的确定
  性闸门，在报告条目上标注 `would_apply` 或拒绝原因而不写库——人工复核的对象
  就是切换模式后的真实写入集。
- **判决 checkpoint 带 judge 版本**（`consolidationJudgeVersion`，app 层常量）：
  修改 judge prompt 或任何 apply 闸门语义时必须提升版本号，旧版本缓存的判决自
  动失效重判，绝不让新闸门消费旧模型的结论。
- **"无新信息"的确定性近似**（严格校验等于再做一次语义判断，不可行）：
  canonical 长度设上限，且其中出现的文件路径、数字、专有 token 必须在证据集中
  出现过，否则该决策降级 KEEP。列为 shadow 期人工抽查项。
- 低于阈值一律保持原状（KEEP），**不要求用户处理**——宁可留重复，不做没把握
  的合并（重复只是难看，错误合并丢信息）。
- MERGE 落库形态：新 canonical + 成员 canonical `status=archived`（其证据边
  重挂到新 canonical）；元数据按 2.2 继承规则。
- ARCHIVE 用于整簇过期暂态（如调试期"未配置错误消息"类噪声）。
- 全部写 `memory_events` 带快照，undo = 恢复成员 + 移除新 canonical。

### 4.3 调度与可靠性

每 person 至多一个整理作业；`pause_while_run_active: true`（前台 run 优先）；
批量处理（`consolidation_batch_size: 8`）每批独立提交；空响应/EOF/429/503 可
重试，连续失败只记录不 apply；daemon 重启从 checkpoint 续跑（簇 id 是内容哈
希，已裁决簇缓存决策，重跑不重付）；绝不回退主 coding model；统计调用次数、
成本、失败率、整理结果（`/diag memory` 可见）。

整理的 due clock 按 tenant/person 持久化到 control store，而不是依赖进程内 24 小时
ticker。daemon 启动后先给前台 30 秒宽限；若上次成功已经逾期，则补跑一次。tick
碰到活跃 run 或一次可重试失败时，记录 defer/failure 原因并在 10 分钟级短间隔重排，
不会丢弃本次机会后再等完整的 24 小时。成功的空 pass 也会推进 last-success 与
next-due。一次 pass 只处理有界批次；若当前 judge 版本仍有 backlog，则记录
`partial` 而不是伪装成完整成功，不推进 `last_success_at`，并在 4 小时后继续追赶。
默认批量 8 条时，持续空闲条件下最多约 48 个簇/天；只有重算后 backlog 为零的
完整扫描才进入正常 24 小时周期。checkpoint 写入失败或候选/判决读取失败会进入
失败退避，不能吞掉后误报成功。`/diag memory` 同时显示 scheduler 的上次尝试/成功、下次 due、延期原因，
以及 consolidation 报告的生成时间和年龄，避免把陈旧 shadow 报告当成当前状态。

### 4.4 上限治理

上限只约束 **active canonical**，不限制 observation 数量：

```yaml
memory:
  governance:
    enabled: true
    mode: "shadow"                  # shadow → merge-only → full
    max_active_global: 120
    max_active_per_workspace: 200
    max_evidence_per_memory: 20
    archive_after: "180d"           # 基于 last_accessed_at/last_verified_at，非 created_at
    unconfirmed_archive_after: "30d"
    evidence_retention: "365d"
    consolidation_interval: "24h"
    consolidation_batch_size: 8
    auto_merge_confidence: 0.95
    auto_supersede_confidence: 0.98
    auto_reinforce_confidence: 0.90
    auto_archive_confidence: 0.90
    pause_while_run_active: true
```

整理判决固定使用 `memory_extract` 语义角色；专用模型只通过
`models.roles.memory_extract` 覆盖，否则使用 `models.auxiliary`。

当前自动落地边界：`shadow` 只记录判断（含 `would_apply` 干跑标注），
`merge-only` 自动执行门禁通过的 MERGE / REINFORCE / ARCHIVE，
`full` 在此基础上执行全局/每 workspace 上限与按时间归档。
`auto_supersede_confidence` 目前仅为前向兼容保留，后台 consolidation 尚不会自动
执行 SUPERSEDE；run 结束时的 intake SUPERSEDE 仍由独立的确定性门禁控制。

维护执行采用 durable job：终态 run 持久化 replay payload，模型输出先冻结到
`proposal_json`，再执行 task/memory apply。daemon 崩溃或写入失败时重放同一提案，
不会再次调用模型产生另一份结论；同一 run/版本/decision key 的 observation 幂等。
`/memory history` 同时展示显式学习记录和自动治理事件，新的 merge/archive 事件可用
`/memory undo <event-id>` 撤销。旧版本未保存 evidence 归属的 merge 快照会被明确拒绝，
避免不完整恢复。

达到上限：先跑合并，再按归档评分淘汰低价值项，**绝不直接删除**。归档评分综合：
最后访问/确认时间、来源权威度、evidence 数、所属 workspace 是否仍存在、是否与
活跃 task/artifact 关联、是否有未决冲突；pinned/user_confirmed 永不自动归档。

---

## 5. 读路径：召回、注入与人类界面（P0 一部分 + P2）

### 5.1 注入（修 D1/D7/D10）

- **pinned/user_confirmed 的 active canonical 无条件置顶注入**，不衰减、不参与
  截断（数量天然极少）。——修 D1，这是"人类安全出口"能闭环的前提。
- 其余 canonical 有两条互补读路径：
  1. 小型静态兜底块按 `ScoreFact`（EffectiveConfidence × scopeRelevance，
     复用 `governance.go`）保留高权威全局偏好；
  2. 查询型召回源注册到 `RecallEngine`，复用 `semantic_recall` 查询扩展、
     跨来源预算和去重，以 CJK/词法签名从 person 分区的 active canonical
     中检索与本轮相关的长尾记忆。只允许 global 与当前 logical workspace
     scope；pinned 不参加检索竞争，因为它已无条件注入。
- 查询型召回当前为有界词法 v1（最多扫描 2000 条、最终跨来源最多选择 3 个
  slice）；embedding 仍是后续可注册的 `RecallSource`，不改变选择器形状。
- `status=forgotten/archived/superseded` 一律不注入；`conflicted` 降权 ×0.5
  参与查询；超过 `valid_until` 或尚未达到 `valid_from` 的 canonical 不进入
  静态注入、查询召回或访问打点。
- **删除 `latestProfileSummary` 注入路径 + 清理 profile target 残留（实测 1
  条）**；画像信号由 pinned + 高分 canonical 承担（与 AGENTS.md"不做 profile
  synthesis call"裁决一致）。

### 5.2 `/memory` 视图与命令

默认视图只展示简洁认知（确定性读取 `category` 字段 + 计数），冲突置顶：

```
Conflicts needing your confirmation   1
Communication preferences             6
Development and tools                 9
Projects and workspaces              12
```

管理命令为安全出口：`search <query>`、`show <id>`、`explain <id>`（显示
canonical、支持/矛盾证据、来源、最后确认时间、为何被召回）、`correct <id>
<text>`、`pin <id>`、`forget <id>`、`conflicts`、`raw`、`history`、`undo`；
`/diag memory` 显示治理统计。search 升级 FTS + CJK bigram（修 D9）。

实现约束（2026-07-26）：`last_accessed_at` 只更新真正通过静态或跨来源召回预算
选择并注入 prompt 的 canonical id；候选扫描不算使用。同一 canonical 若已被
查询型召回选中，会从静态兜底块排除，避免当轮重复注入。Agent 原生工具调用透传
`_workspace_id`，因此
项目/环境记忆按 workspace 落库。`pin <id>`/`unpin <id>` 已接 canonical 保护位，
pin 同时确认用户权威；unpin 仅取消无条件注入，不抹掉用户确认。

写入边界（2026-08-08）：可归属于具体工单、build 或 run 的“当前状态”是
task card、handoff 与工作脊柱的职责，即使维护模型将其标为 `durable`，确定性
intake 也必须丢弃，不能进入长期 canonical。带业务前缀的运行态（例如
`CI_PENDING_APPROVAL`）同样识别。召回命中后的访问打点使用脱离前台取消信号的
短事务；只有已进入 prompt 预算的 canonical 才会被 touch。

### 5.3 四类知识职责（2026-08-15；2026-08-31 简化 P2/P3 修订）

长期连续性不能由一个“万能 memory 表”承担，当前实现明确分为四类：

| 类型 | 负责内容 | 写入/更新方式 | 是否参与任务路由 |
|---|---|---|---|
| Work Spine / handoff / parent Run 切片 | run、证据、当前状态、下一步 | 每轮确定性落库 | 否 |
| Task Reference | 任务的名称、编号、实体别名、描述性地址 | post-run 维护提案 + 用户确认激活；用户可直接增删 | 否（P2 起纯别名/搜索提示，只参与召回） |
| Canonical memory | 用户明确表达的个人偏好、习惯与纠正（P3 起仅此一类） | 显式 `/remember`/`/forget` 为主，post-run 自动候选为辅（仅偏好证据），observation/canonical 治理 | 否，只参与召回 |
| Workspace knowledge | `AGENTS.md`、`.selfmind.md` 等项目规则与程序性知识 | 授权扫描器生成路径/hash/mtime/章节/有界摘录；文件变更整份替换 | 否，只参与当前 workspace 召回 |

2026-08-31 的偏好记忆简化之后，`target="memory"` 的环境/项目事实**不再自动
写入**个人记忆：分析器只判定用户明确表达的偏好
（确定性 apply 层跳过一切非 user 目标的决策，冻结旧提案重放同样被跳过并计入
`environment_target_retired` disposition）；memory 工具的 `add` 拒绝
environment 目标并指向仓库约定文件。存量 environment 行继续可读、可被
`selfmind maintenance memory-archive-environment [--apply]` 可逆归档
（pinned/用户确认行不动）。

Task Reference 的自动激活已冻结（P2）：run 支持只能到 `candidate`（召回
信号），仅用户确认激活；同一 value 出现多个确认绑定进入 `conflicted` 弃权。
标题、摘要、recall 结果和模型生成文本都不算证据。引用不再路由消息、不加载
任务上下文、不改变 current task、workspace、权限或生命周期。
用户可通过 `/task <id> references`、`reference add|remove` 查看和裁决。

Workspace knowledge 不复制整份文档到 canonical memory，也不调用额外模型。
它复用项目上下文扫描的授权边界，并作为独立 `RecallSource` 与 task/session/
canonical 共同竞争固定预算；删除或 hash 变化会使旧章节失效。

`selfmind report daily` 是个人版的最小记忆观察基线：它按来源报告 recall 候选、
实际注入和最终输出词法重叠，并汇总 `ADD`、`REINFORCE`、`SUPERSEDE`、
`CONFLICT`、瞬态丢弃等 disposition。词法重叠只是诊断信号，不证明因果；当
maintenance 存在 failed/blocked 作业时，报告必须明确标记写入基线不完整。

### 5.4 纠正与遗忘语义（P2）

| 操作 | 语义 |
|------|------|
| correct | 创建 SourceUser observation；旧 canonical `superseded`，新 canonical `user_confirmed` |
| pin | 置位 pinned：禁止自动合并/覆盖/归档，注入置顶 |
| forget | canonical+关联 observation 立即 `forgotten`（**当轮起从注入/召回/search 排除**）；按 `evidence_retention`/隐私策略延迟物理删除；不可 restore |
| archive | 保留证据，默认不进上下文；可 restore |
| conflict 裁决 | 用户在 `/memory conflicts` 选择胜者 → 胜者升级 user_confirmed（获得 pin 级保护，无需 pin 仪式），败者 superseded |
| restore | 仅恢复 archived → active |

CONFLICT 机制是"减少人工管理"的关键反转：系统只在真正矛盾且无法自决时**主动
提问一次**，而不是让用户巡逻整张列表。

---

## 6. 迁移方案

1. 新表增量建；现有 `facts` 表不动，旧读取路径保留一个版本。
2. **迁移范围 = `default` 分区（190 条，2026-06-17→06-21 的真实历史）+
   `person_3477f062…`（781 条）**，两者都导入为 legacy observations（source
   保留原值，空 source 记 `legacy`）；default 的导入让 consolidation 跨来源去
   重，保留更早的 `created_at` 时间线。eval 残留分区 facts 为 0，不迁移；顺手
   `selfmind eval clean`。
3. 初始 canonical 一对一生成（`normalized_hash` 相同的合为一条并累计
   evidence），暂不做语义合并。
4. `turn_extractor` 来源的 189 条标记为优先归档候选（已废弃抽取器的低置信碎
   片），作为 shadow 期人工抽查的首批样本。profile 残留 1 条直接删除。
5. shadow 模式持续运行：judge 决策只写 `memory_events(mode=shadow)`，不 apply；
   人工抽查 **≥100 个候选组**。
6. 自动合并抽样准确率 **≥99%** 后，只开放高置信 MERGE → 验证召回不下降 →
   开 REINFORCE/SUPERSEDE → 最后开自动归档。
7. 存量一次性清理：对迁移后的全量跑 judge 出报告，owner 审核后 apply（预期
   active canonical 收敛至 120–200）。

---

## 7. 实施顺序与验收标准

### 7.1 顺序

```
① 证据/规范分层（新表 + 迁移导入）
② Run 终态幂等（maintenance_jobs 挂 FinishRun/恢复 sweep；CAS）
③ 单次 PostRun 决策（与 labeler 合并一次调用；三层去重网；REINFORCE 复活；
   memory 工具并轨；—— 同批修 D1 pinned 注入 + D7 profile 清理 + D4 审计）
④ shadow 整理（judge 接线，只报告不写）
⑤ 高置信自动合并（99% 门 + 存量清理）
⑥ 上限与归档
⑦ 人类安全出口（conflicts/explain/forget 全语义）
⑧ Hybrid Recall 优化（FTS/CJK；embedding 接 RecallSource seam 后置）
```

每步按仓库规则：行为变化同 PR 更新 `docs/STATUS.md` 行 + 增补 `evalcases/`。

### 7.2 验收标准

- pinned / user_confirmed 记忆被自动修改：**0 次**。
- 跨 workspace 自动合并：**0 次**。
- 高置信自动合并抽样准确率：**≥99%**。
- forget 后进入 prompt 或召回：**0 次**（含 forget 当轮）。
- 同一 run 重试不产生重复 observation；daemon 重启后维护任务不重不丢。
- 后台维护对前台请求 P95 延迟影响 <1%。
- 长期 active canonical 稳定在配置区间（120–200）。
- 记忆上下文 token 降低 ≥30% 且召回质量不下降（day-in-the-life scorecard）。
- 用户 correct 后，旧事实不再影响后续推理。
- pin 的新增频率趋近于零（产品目标：治理生效后用户无需日常 pin）。

### 7.3 必备 eval case（`evalcases/`）

- pin/user_confirmed 事实下一轮对模型可见（修 D1 的回归锁）。
- 同一事实换措辞说三遍 → 一条 canonical、occurrences=3、confidence 上升。
- 配置变更类 run → SUPERSEDE：旧条不再注入、可 explain 追溯。
- 受保护条目遭遇 SUPERSEDE → 自动降级 CONFLICT，双方保留。
- forget 后立即提问相关内容 → 不出现在回答依据中。
- `/memory history` 可见自动写入并可 undo。

---

## 8. 与现有代码的对接点

| 现有位置 | 变化 |
|----------|------|
| `internal/app/post_run_analyzer.go` | wire 格式扩展为 memory_decisions；三层去重网；策略校验表 |
| `internal/gateway/httpapi/run_labeler.go` | prompt 合并邻居集；仍是同一次调用 |
| `internal/gateway/httpapi/run_recovery.go` / `control.Store.FinishRun` | 终态事务创建 maintenance_job；恢复 sweep 创建受限 job |
| `internal/kernel/memory/consolidation.go` | Stage-1 检索复用；`ValidateConsolidationDecision` 进 apply 前置 |
| `internal/kernel/memory/governance.go` | `RepetitionBoost` 接入 REINFORCE；ScoreFact 继续用于注入排序 |
| `internal/kernel/agent.go:1734` | pinned 置顶注入；动态预算；删 profile 注入 |
| `internal/kernel/profile_synthesizer.go` | 删除（死代码） |
| `internal/tools/memory.go` / `memory_view.go` | 工具并轨策略层；视图改读 category 字段；search 升 FTS |
| `internal/tools/learning_audit.go` | 与 `memory_events` 合流（history/undo 单一通道） |

## 9. 明确不做的事

- 不加第二个抽取/决策模型调用（并入唯一 maintenance 调用）。
- 不引入图谱数据库/外部记忆服务；关系语义 = SQLite 三类边。
- 不在写入时追求 100% 判重精度（intake 挡明显重复，彻底干净靠后台）。
- 不物理删除证据（forget 隐私保留期后除外）。
- 不把 embedding 做成前置依赖（接 seam 后置替换检索层）。
- 不要求用户日常整理（pin/forget 是安全阀，CONFLICT 是唯一主动触点）。

---

## 10. Shadow 校准与晋级

`shadow` 是默认且无写入风险的治理模式。每次整理结束后，SelfMind 在
`<data-dir>/memory/reports/` 写入同一份结果的两种视图：

- `shadow-<person>.json`：机器可读的完整判决、候选组和动作明细；
- `shadow-<person>.md`：供 owner 抽查的可读报告。

报告至少包含：整理前 active 数、候选组数、本轮实际判决数、拒绝数、
`would_apply` 数、预计整理后 active 数，以及每项 MERGE/REINFORCE/ARCHIVE
建议。`/diag memory` 只显示最新一轮摘要，不把明细塞进日常终端。

晋级必须是显式配置变更：

1. 保持 `shadow`，抽查真实历史中的候选与拒绝原因；
2. 准确率满足验收线后改为 `merge-only`，只应用高置信、可逆动作；
3. 观察稳定库存后再设置 global/workspace cap，并考虑启用 `full`。

系统不得根据一次报告自动晋级。报告缺失、模型未配置、候选不足或校验
失败都应保持当前模式，前台 run 始终优先于后台整理。
