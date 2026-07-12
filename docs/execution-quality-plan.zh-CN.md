# 执行质量补强方案（提案）

> **状态：提案，未经 owner 批准不开工。** 本文是 2026-07-12 对一份外部差距清单
> 逐项核查后的设计稿。优先级的唯一权威是 `docs/STATUS.md`；本文获批后应把对应
> 条目登记到 STATUS 的 next-work 区，本文只保留设计细节。
> 前置条件：当前工作区四批半未提交改动（记忆治理 P0/P1、激进化 apply、
> /memory 性能修复、eval cassette）先提交落库，再开任何新工作流。

## 0. 差距核查结论（2026-07-12）

| # | 清单项 | 核查结论 |
|---|--------|----------|
| 1 | ContextComposer 是薄包装 | 属实，半刻意（契约形式化而非对象模型）。低优 → W5 |
| 2 | 大工具输出只截断不工件化 | 属实。**最高优** → W1 |
| 3 | Hybrid Recall 只有骨架 | 属实，seam 已留。→ W4 |
| 4 | Memory 治理字段不足 | **已由 canonical 分层基本解决**；access_count/importance/expires_at 刻意不做（见 §8） |
| 5 | Cron 未绑定 TaskID | 属实，但 P3 之后 label 不再管上下文，降级为显示问题。低优 → W6 |
| 6 | Task 高级治理缺失 | 属实（无健康检测、无合并建议器）。→ W3 |
| 7 | 后台学习缺统一调度 | 记忆侧已完成（maintenance_jobs + StartMemoryGovernance）；skill review 未迁。→ W7 |
| 8 | 诊断只有聚合视图 | 部分完成（/diag memory、context.breakdown 已有）。→ W2 |

推荐顺序：**W1 > W2 > W3 > W4 > W5 ≈ W6 ≈ W7**。排序依据是北极星
（跨端连续性 + 执行质量）：W1 直接减少模型重跑与信息丢失，W2 是自诊断能力，
W3 服务任务连续性体验，W4 以后是增量优化。

---

## W1 工具输出工件化（最高优先）—— ✅ 已实施 2026-07-12

> 全部设计点（0–6）已落地并通过单测；STATUS.md Extended tools 行为准。
> 唯一余项：`evalcases/quality/tool-output-artifact.yaml` 的 cassette 因当日
> 两个 provider 配额耗尽未录，配额恢复后录制并设 `require_cassette: true`。

### 现状与证据

`internal/kernel/tool_result.go`：`toolResultModelBytes = 24000`，超限工具输出
对模型只保留 head/tail + "too large"标注；运行期 `Raw` 只活一个 turn，全文
不落盘。后果：模型拿不到中段就重跑同一命令（重复付费+慢），跨 turn / 跨端
`继续` 时全文永久丢失。

### 目标

任何超限工具输出全文持久化为 run artifact，模型可在同一 run 或续跑中**按引用
重读任意片段**，且不破坏三表面契约（raw / model-bounded / user preview）。

### 与 Codex 的对照（2026-07-12 源码核查 `/mnt/d/wwwroot/ai/codex`）

Codex 的做法是四层有界但绝不落盘：采集端 1MiB HeadTailBuffer（中段采集时即
丢，`unified_exec/head_tail_buffer.rs`）；单次结果默认 10k token 且模型可用
`max_output_tokens` 参数按需放大；工具输出进历史时按 TruncationPolicy 再截
一遍（`context_manager/history.rs`）；长输出靠有状态 session 轮询增量读。
它对"中段"的答案是丢弃+重跑/管道过滤。W1 吸收其有界性（下述 0/3 两点），
坚持工件化作为差异点——跨端续跑需要"三天后按引用重读"，codex 单机会话模型
不需要。

### 设计

0. **采集端封顶（借 codex）**：`Raw` 当前无上限，失控命令可吃爆 daemon 内
   存。envelope 层加 2MiB head/tail 采集上限（头尾各半，标注 omitted 字节
   数）；落盘的工件也是这个有界版本，超出部分连工件都不保留。
1. **落盘**：超过 `toolResultModelBytes` 的结果全文写
   `<data_dir>/<person分区>/artifacts/<run_id>/<tool>-<call_seq>.txt`，并写一行
   `control.task_artifacts`（复用现有表：`Kind="tool_output"`、`URI=文件路径`、
   `Metadata={tool, bytes, sha256, truncated_note}`）。写盘失败降级为现状
   （只截断），绝不让工件化失败拖垮工具调用。
2. **模型引用**：head/tail 截断标注改为携带引用：
   `[output truncated: full 812KB saved as artifact art_xxx — use
   tool_output_view(artifact_id, offset, limit) to read more]`。
3. **读取工具**：新增只读工具 `tool_output_view(artifact_id, offset_bytes,
   limit_bytes)`（默认/上限 每次 ≤24KB）。person 分区校验：artifact 必须属于
   该 person 的 task；输出仍走 result envelope。它是纯读工具，允许并行批。
   不复用 `read_file`：artifact 落在 data dir，在 workspace scope 之外，
   `WorkspaceScopeMiddleware` 本就该拦它——不要为此在 scope 上开洞。
4. **保留策略**：每 run 上限（32 个 / 64MB，超出按最旧丢并在标注里说明）；
   随 task 自动归档 sweep 清理超龄工件文件（行保留，URI 标记 expired）；
   `selfmind eval clean` 同步清理 eval 残留工件。
5. **续跑集成**：`withResumeContext` 的 `files_this_task_created_or_changed`
   机制不变；另在 resume 上下文中列出该 task 最近 N 个 `tool_output` 工件的
   id+工具名+大小，让续跑知道有什么可读。
6. **同 turn 历史再截断（借 codex）**：当前每个 24KB 截断版在 turn 的迭代
   窗口里一直占位直到整体压缩。带工件引用的旧工具结果在迭代过去 3 轮后缩到
   4KB + 引用（模型随时可按引用读回，所以这次"再截断"是无损的——这正是
   工件化让 codex 模式变安全的地方）。若实现侵入迭代热路径过深，允许拆出
   随 W5 交付，但设计归属 W1。

### 验收与测试

- 单测：超限→落盘+引用标注；未超限→无工件；落盘失败→降级不报错；
  分区越权 artifact_id → 拒绝。
- eval case（message path 行为变化，按仓库规矩必须加）：turn1 生成 >24KB
  输出，turn2 要求引用中段内容 → 模型通过 `tool_output_view` 取回，不重跑
  原命令。
- `docs/STATUS.md` Extended tools / Run coordinator 行同 PR 更新。

### 不做

- 不做全文进 prompt 的"智能摘要"（等于第二个模型调用，红线）；
- 不做跨 person 工件共享。

估计规模：~400 行 + 测试，1 个工作日。

---

## W2 诊断补齐（/diag context、/diag tasks、compaction/recall 可观测）—— ✅ 已实施 2026-07-12

> 设计点 1–6 全部落地（含 `/diag memory` 的整理进度行，经可选 `PassSummary`
> 能力接口）；"可能卡住"清单作为 W3 健康检测的先导已随 `/diag tasks` 交付。
> W5 的组装时记账未随本包实施（breakdown 仍为组装后估算）。
> STATUS.md Observability 行为准。

### 现状与证据

`/diag` 已含最近一次 `context.breakdown` 单行份额；`/diag memory` 已有治理
统计。**compaction 与 recall 今天都不发结构化事件**（已核实），无法回答
"上下文为什么这么大 / 压缩省了多少 / recall 花了多少"。

### 设计

全部为 pre-agent 控制命令 + 事件读模型，不花模型 token：

1. **`context.compacted` 事件**：`ContextEngine` 压缩落点 emit
   `{before_tokens, after_tokens, summarized_turns, summarizer_role, duration}`。
2. **`recall.selected` 事件**：recall selector 每 turn emit
   `{sources: [{name, hits, elapsed_ms}], slices_attached, expansion_used}`。
   零命中时也发（hits=0），否则无法区分"没触发"和"没命中"。
3. **`/diag context`**：最近一次 breakdown 展开为多行（每 section 一行：
   chars/占比）+ 最近一次 compaction 前后对比 + 当前模型窗口。
4. **`/diag tasks`**：SQL 聚合——open/paused/attention 计数、最老 interrupted
   的 id+age、队列深度、pending approvals/clarifies、W3 健康标记计数。
5. **`/diag memory`** 增补最近一次 consolidation pass 摘要（judged/applied/
   rejected + 报告路径），数据来自 memory_events，不新增状态。
6. 命令元数据登记 `internal/gateway/command`（触发 drift-guard 测试更新）。

### 验收与测试

- httpapi 单测：每个子命令的渲染 + 事件读取；kernel 单测：两个新事件的载荷。
- 压缩/召回行为本身不变，无需新 eval case；`/diag` 输出变化更新 STATUS 观测行。

估计规模：~300 行 + 测试，1 个工作日。

---

## W3 Task 健康检测 + 重复合并建议（只建议，不自动）—— ✅ 已实施 2026-07-12

> 设计点 1–3 落地：健康标记随 W2 的 `/diag tasks` 交付（卡片徽标改为
> `/tasks` 的 possible-duplicate 行）；建议器挂 6h 治理 sweep（同 workspace、
> ≥0.8、幂等事件）；`/task <src> merge <dst>` 单事务迁移全部历史并归档源。
> 设计点 4（统计扫描聚合化）核查后未发现全表扫问题，未动。
> eval case 未加：合并是确定性 pre-agent 控制命令，单测覆盖（先例：/queue drop）。
> STATUS.md Task governance 行为准。

### 现状与证据

`/tasks` 卡片只有 attention（pending approval/question/blocked）与 paused 徽标；
自动治理只有 6h 归档 sweep。没有"卡了很久"的主动信号，没有重复 task 的
合并通道（post-run labeler 的 MOVE 只管单 run 归属）。

### 设计

1. **健康标记（读模型，不改状态机）**：`/tasks` 与 `/diag tasks` 计算三类
   标记——`stalled`（interrupted/blocked 超过 48h 无新 run）、`dormant`
   （in_progress 超过 7d 无活动）、`stuck-approval`（pending approval/clarify
   超过 24h）。纯 SQL（`last_activity_at` 已有），阈值进 config（`tasks.health_*`）。
   显示为卡片徽标，绝不自动改 task 状态——stuck-run 不变式仍归 recovery sweep 管。
2. **重复合并建议器（确定性，零模型调用）**：nightly sweep（挂现有 6h 治理
   sweep）对 person 的 OPEN task 卡片两两算 `SimilaritySignature` 相似度
   （复用记忆侧刚建的签名基建；title + 卡片摘要），≥0.8 的对写一条
   `task.duplicate_suggested` 事件（幂等：同 pair 只建议一次）。`/tasks` 在
   卡片上显示 `possible duplicate of #n`。
3. **显式合并命令**：`/task merge <src> <dst>`——批量 `ReassignRun`（事务已
   有）迁移 src 的 runs/events/artifacts 到 dst，src 归档（不删除），写
   `task.merged` 事件。仅用户显式触发；建议器永不自动执行（清单与仓库原则
   一致：治理是 post-run 且可逆的，合并建议必须过人）。
4. **统计扫描核查**：把 `/tasks` 附属计数改为带 WHERE 的 SQL 聚合（若核查
   证实仍全表扫）。

### 验收与测试

- control 单测：merge 事务（runs/events/artifacts/handoffs 全迁、current_task
  重指、src 归档可 /resume 复活）；建议器幂等；健康标记阈值边界。
- eval case：`/task merge` 后 `继续` 在 dst 上下文续跑。

估计规模：~500 行 + 测试，1.5 个工作日。

---

## W4 Recall v2（artifact 源 + 融合排序；embedding 后置）

### 现状与证据

`httpapi/recall.go`：`RecallSource` seam + taskcard + session FTS +
`semantic_recall` 角色查询扩展。无 artifact 源、无 embedding、排序只有词法分。

### 设计（两步走，embedding 明确后置）

1. **v1.5（无新依赖，先做）**：
   - 新增 `artifactRecallSource`：对 `task_artifacts` 的 name/metadata 做词法
     匹配（W1 落地后工件才有料，故排在 W1 后）；
   - **融合排序**：`score = lexical × recency_decay(last_activity) ×
     authority_boost(pinned task / user_confirmed 记忆 / 有 handoff 的 task) `。
     不引入 access_count 字段——用 task `last_activity_at` 与记忆
     `last_accessed_at` 现有信号；
   - 预算契约不变：≤3 slices、每源限额、超时降级、绝不阻塞 turn。
2. **v2（embedding，等基建）**：`Runtime` 增加可选 embeddings 能力探测；有
   能力的 provider 生成记忆/卡片向量（SQLite 存 blob，纯 Go 余弦），实现为
   又一个 `RecallSource`。**不把 embedding 做成任何功能的前置依赖**（治理
   文档 §9 原话）。

### 验收与测试

- recall 单测：融合分排序稳定性、artifact 源命中、降级路径。
- 观测：依赖 W2 的 `recall.selected` 事件验证命中率变化。

估计规模：v1.5 ~250 行，0.5–1 个工作日；v2 另议。

---

## W5 ContextComposer 组装时记账（低优）—— ✅ 已实施 2026-07-12

> buildSystemPrompt 逐段记账（类别+token+stable/volatile），breakdown 事件改用
> 精确记账并新增 stable/volatile 字段，`/diag context` 显示缓存前缀边界。

薄包装是刻意取舍，不引入插件化 slice 框架。唯一值得做的增量：Compose 从
"组装后估算"改为**组装时记账**——每个切片在拼接时记录
`{slice, source, chars, stable|volatile}`，`context.breakdown` 事件直接用
记账值（准确），并为 P1-3 的稳定前缀提供权威边界。行为不变，只是观测从
估算变精确。~150 行。与 W2 的 `/diag context` 天然配套，可并入 W2 一起做。

## W6 Cron 绑定 TaskID（低优，小改动）—— ✅ 已实施 2026-07-12

> `cron_jobs.task_id` 幂等迁移；首次成功执行回写学到的 task；后续触发作为显式
> attach 证据；label 归档自动清除重学。

`cron_jobs` 加可空 `task_id`；执行时若非空且 label 未归档 → 作为显式
attach 证据（等价 caller task_id）；首次执行由 labeler TITLE 命名后回写
task_id。日报类任务从此稳定归入同一 label。注意这只是显示/归档整洁问题
（P3 后 label 不管上下文），做的价值是 `/tasks` 视图干净。~120 行。

## W7 skill review 迁入周期维护器（低优）—— ✅ 已实施 2026-07-12

> daemon 内 SpawnReview 改为入队幂等 maintenance_job（payload 哈希键、版本
> 命名空间 100），维护 worker 每 pass ≤2 个 CAS 认领执行、失败 10 分钟重试、
> 5 次封顶；无入队接线的环境（eval/测试）保持原地执行路径。

`background_review` 触发从即时 goroutine 改为 `maintenance_jobs` 同款
durable job（去重 UNIQUE 键、CAS 认领、限流、有界重试），挂进 daemon 周期
维护循环。收益是崩溃不丢/不重跑审查任务。记忆侧模式已验证，照抄。~200 行。

---

## 8. 明确不做（对差距清单的否定项）

- **access_count 字段**：`last_accessed_at` + 半衰已覆盖时效信号；计数会把
  高频旧习惯永久钉住，与"最近确认优先"冲突。
- **importance / expires_at**：置信度 × 时间衰减 + archive_after 已表达同一
  语义，双轨字段必然漂移。
- **candidate 状态的记忆**：intake 是单次决策直接落库（SKIP 即不落），
  引入 candidate 队列等于把治理变成用户作业，违背"静默自组织"原则。
- **task 自动合并**：只建议、只显式执行（W3）。
- **为 recall/工件化增加第二个每 turn 模型调用**：红线不动。
- **ContextSlice 插件框架**：过度工程，W5 的记账即够。

## 9. 执行顺序与依赖

```
提交现有工作 ──► W1 工具工件化 ──► W4-v1.5（artifact 源依赖 W1）
                └► W2 诊断（W5 记账可并入） ──► W3 task 健康/合并建议
W6 / W7 / W4-v2：空档期插入，无依赖压力
```

每个工作流独立 PR：行为变化同 PR 更新 `docs/STATUS.md` 行 + eval case
（W1、W3 必须；W2 观测类可豁免）。全部完成后，这份文档的内容并入各域文档，
本文归档删除（不留过期规划文档，仓库规矩）。
