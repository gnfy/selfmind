# SelfMind 上下文生命周期与 P0-P2 落地

这份文档记录 SelfMind 在任务体验、长期上下文和多端协同上的当前契约。它面向后续开发者和接手的 AI 编程代理，目标是避免把历史、工具输出、事件和 artifact 随意拼进 prompt，导致体验变慢、输出乱码、上下文污染或 IM/CLI 行为不一致。

## 产品目标

SelfMind 不是复制某个现有 coding CLI。它要做到的是：用户把一个开发任务交给 SelfMind 后，SelfMind 能像成熟 coding agent 一样持续推进、可感知、有状态、可恢复，并且未来可以从 CLI、微信、Web 等多个端协同使用。

核心原则：

- CLI 负责高频交互，应流式输出 assistant 文本和工具进度。
- IM 负责异步协同，不应逐 token 流式刷屏，而应发送 working notice、关键审批/失败/完成通知。
- task/run/event/handoff/artifact 是跨端共享状态，channel transcript 仍保持渠道隔离。
- 模型每轮只拿当前需要的上下文切片，不直接读取全部历史。

## P0：用户可感知的任务执行

P0 的目标是解决“看起来卡住”“工具一直 running”“输出乱码/JSON 难读”的问题。

当前落点：

- `internal/gateway/cli/transcript_renderer.go`
  - 用简洁动词行渲染工具步骤：`Running`、`Read`、`Ran`、`Wrote` 等。
  - 单个步骤完成后显示最终耗时，底部状态栏显示 run 总耗时。
  - 工具结果默认显示用户可读 preview，不把 raw JSON 直接倒到 transcript。
- `internal/kernel/tool_result.go`
  - 工具结果分成 `Raw`、`Preview`、`ModelContent` 三个面。
  - UI/event 用 compact preview，模型看到 bounded model content，完整 raw 留在工具内部。
- `internal/gateway/router/response.go`
  - 非流式渠道聚合工具事件时输出 `Process summary`，避免把内部 JSON/协议文本发给 IM 用户。
- `internal/gateway/httpapi/run_events.go`
  - IM 长任务定时发送 “AI is still working” 类通知。
  - CLI 继续通过 event stream 展示详细步骤。

后续维护规则：

- 不要把每个文件读取都做成持续增长的状态。一个工具 invocation 只有 started、output、completed/failure。
- 不要用 “terminal running” 这类泛化状态替代具体步骤。用户需要知道当前是在 search、read、run test 还是 waiting approval。
- 工具失败必须转成可行动错误，包含失败命令/文件/原因/下一步，而不是只显示 provider 或 protocol error。

## P1：任务上下文选择器

P1 的目标是把 task/event/artifact/handoff 变成模型每轮可用的“选择后上下文”，而不是靠用户反复说“继续”或让模型猜。

当前数据流：

```text
control.db
  tasks / task_runs / task_events / task_handoffs / task_artifacts
        |
        v
internal/gateway/httpapi/context_selector.go
  selectedTaskRuntimeContext(...)
        |
        v
internal/kernel/task_runtime_context.go
  kernel.TaskRuntimeContext
        |
        v
kernel.WithTaskRuntimeContext(ctx, selected)
        |
        v
internal/kernel/agent.go
  buildSystemPrompt(...)
        |
        v
LLM system prompt: # DURABLE TASK CONTEXT
```

已经进入模型上下文的切片：

- 当前 `task_id`、`run_id`、title、status、channel、workspace。
- task 的 `current_summary` 和 `next_steps`。
- 最新 handoff：summary、done、remaining、files、tests、risks。
- 最近 artifacts：kind、name、uri、mime、metadata summary。
- 最近 events：type、channel、payload 摘要。

实现文件：

- `internal/gateway/httpapi/context_selector.go`
- `internal/gateway/httpapi/context_selector_test.go`
- `internal/kernel/task_runtime_context.go`
- `internal/kernel/task_runtime_context_test.go`

后续维护规则：

- `kernel` 不直接依赖 `control.Store`。control 数据只能由 gateway 选择后转成 `TaskRuntimeContext`。
- 不要把 raw event payload、raw artifact metadata 或完整工具输出直接塞进 system prompt。
- 继续/恢复任务时优先读取 handoff 和 task events，不要只靠最近聊天文本。
- 如果 IM 端没有 workspace，应要求用户绑定/选择 workspace；不要猜本机路径。

## P2：长期上下文与可扩展召回

P2 的目标是让 SelfMind 具备长期上下文能力：event log、artifacts、summaries、indexed memory 都是 durable context source，但每轮只选择必要片段给模型。

当前已有能力：

- `control.db`
  - `task_events` 记录运行事件。
  - `task_handoffs` 保存结构化交接。
  - `task_artifacts` 保存任务产物 URI 和 metadata。
- `internal/kernel/memory`
  - `facts` 保存稳定用户偏好和项目事实。
  - `sessions_fts` 和 `session_messages` 建立历史会话全文索引。
  - `SearchSessions`、`ListRecentSessions`、`GetSessionMessages` 支持 session_search。
- `internal/kernel/agent.go`
  - `autoRecall` 使用 session FTS 和 semantic expander 做历史召回。
  - `buildSystemPrompt` 注入 memory facts 和 `TaskRuntimeContext`。
- `internal/kernel/context_engine.go`
  - 对 channel-local transcript 做窗口控制和旧消息压缩。

当前 P2 边界：

- 已有 durable sources 和第一条 task context selector 主链路。
- 事实 memory 和 session FTS 仍由 agent 内部召回，尚未完全并入统一 selector scoring。
- 还没有 embedding/vector index；当前 indexed memory 是 SQLite FTS5 + semantic query expansion。

下一步增强方向：

1. 抽象 `ContextSelector` 接口，把 task/handoff/event/artifact/memory/session 都作为候选来源。
2. 为候选项增加统一 metadata：source、scope、freshness、confidence、token_cost、privacy_level。
3. scoring 先用规则 + FTS rank + recency，后续再接 embedding rerank。
4. 输出统一 `SelectedContext`，再由 `TaskRuntimeContext` 或后续 `RuntimeContextBundle` 渲染到 prompt。
5. 所有被选择的上下文都写入 run event，方便调试“模型为什么知道这些”。

建议的候选结构：

```go
type ContextCandidate struct {
    Source      string
    Scope       string
    ID          string
    Text        string
    Freshness   time.Time
    Confidence  float64
    TokenCost   int
    Metadata    map[string]string
}
```

## 与主流 coding agent 的取舍

成熟 CLI coding agent 值得学习：

- 用户感知非常强：每一步探索、命令、错误、审批都可见。
- 当前 workspace 是默认语义，用户说“分析这个项目”时会先看当前目录。
- 工具输出是摘要和 transcript 双层结构，不把长输出直接堆满主界面。

成熟多端 agent runtime 值得学习：

- provider/tool/channel adapter 分层明确。
- 多模型接入走 provider runtime，而不是每个入口写 vendor if/else。
- 长期记忆和 skill 更像可治理资产，有 provenance 和可回滚。

SelfMind 的选择：

- CLI 学前者的可感知工作流。
- 多端/IM/gateway 学后者的 adapter 和 provider 分层。
- 长期上下文走 SelfMind 自己的 task/run/event/artifact/memory 控制面，服务未来 SaaS。

## 开发检查清单

改上下文相关代码前，先确认：

- 是否影响 CLI 和 IM 两种反馈契约。
- 是否把 transient task progress 错写成长期 memory。
- 是否把 channel transcript 当成跨端共享状态。
- 是否绕过 `TaskRuntimeContext` 直接向 prompt 拼 raw control 数据。
- 是否新增了不可解释的规则，让“继续”“分析当前项目”“你是谁”这类请求被误分类。

验证建议：

```sh
GOWORK=off go test ./internal/kernel ./internal/gateway/httpapi ./internal/gateway/router ./internal/gateway/cli
GOWORK=off go test ./...
```

## P0 Runtime Context Status

The current P0 implementation uses `kernel.RuntimeContextBundle` as the
per-turn prompt contract for workspace, task runtime state, selected memory
snippets, selection notes, and bounded context budgets. Future context work
should extend this bundle or the gateway selector that feeds it.

Skill 是 bundle 内的独立模型可见契约,但不绕开总上下文治理:

- 未激活时只投递有界目录元数据。已知模型的 token 预算为
  `floor(context_length_tokens * 2%)`,同时受 8000 UTF-8 字节硬上限约束;
  先保留候选存在性和公平短描述,再按排名补全描述,最后才按确定性顺序省略。
- 激活后目录退出,只保留一个 Active Skill。已知模型的主干 token 预算为
  `clamp(floor(context_length_tokens * 3%), 512, 2048)`,同时受 8192 UTF-8
  字节硬上限约束。模型元数据未知时目录和主干都显式回退到 512 tokens /
  2048 bytes,不伪造 context length。bundle 总额在原 8 KiB 非 Skill预算上
  增加目录/主干两者的较大字节值,不会再被固定 `Prompt(8000)` 二次截断。
- contract-v1 在 activation 时冻结实际投递字节及其 hash/byte receipt。正文
  超预算时只投递显式分页索引,通过 `skill_view` 按 section/resource/offset
  读取,绝不把静默截断的前缀冒充完整指令。
- Active Skill 标记块属于 compaction/recovery 的 protected slice。同一
  activation 穿过普通压缩和 provider window recovery 后字节必须不变;偏离
  时只发 `skill.delivery.deviation`,正常轮次不重复发送 receipt。
- 每轮 prompt 只重复模型需要引用的 `name` 与 `activation_id`;package/version/
  resource/delivery hash 留在 durable control state、事件与工具结果中,不与
  指令正文争抢预算。

`internal/kernel/context_engine.go` is cheap on the hot path: while the window
is under budget it loads only a bounded slice of recent channel history and does
no LLM work. When the window grows past `summaryThreshold` (¾ of the budget) it
**compacts by default** (契约变更, 2026-07-05): the drop-eligible MIDDLE turns are
summarized into ONE structured message, while the head (system prompt + the
first user turn = original task) and the tail (最近 `compactionTailTurns` 条) are
kept verbatim. 这取代了旧的"默认直接丢弃最旧消息"行为——长对话不再"失忆"。

关键约束:

- 摘要用便宜的 `memory_extract` 角色 provider(`Agent.SetSummaryProvider` →
  `ContextEngine.SetSummaryProvider`),不占用主 coding provider;只在越过阈值那一
  刻做一次有界调用,绝不每轮调用,所以流式首 token 不受影响。
- 摘要 prompt 强制保留 `## Relevant Files`(任务目标、决策、下一步,以及所有
  创建/修改/读取的文件路径)。另有一个确定性兜底:从工具调用参数
  (`path`/`file_path`/`output_path`/`workdir` 和 V4A `patch`/`apply_patch` 头)
  抽取路径,模型漏写时自动补上,保证 artifact 清单不丢。
- 兜底策略:没有配置 `summarizer`(且未启用旧兼容开关)时退回确定性裁剪;
  摘要为空、摘要不比被替换
  的片段更小、或中段本身已是上一份摘要时,都跳过压缩,永不增长窗口或递归叠加摘要。
- `SELFMIND_SYNC_CONTEXT_SUMMARY` 现为遗留开关:压缩默认已开,不再需要它;它仅在
  没有配置 `summarizer` 角色时,允许回退用主 provider 做压缩。新配置应使用
  `models.auxiliary` 或显式的 `models.roles.summarizer`。

Streaming feedback now relies on larger event buffers and critical-event
delivery backpressure. CLI/TUI should show tool and assistant progress live;
IM channels should keep token streams collapsed and use working notices plus
final summaries.

## 工作历史:个人级工作主轴(Work Spine,2026-07-06,P1 契约变更)

> 目标架构见 `docs/work-timeline.md`(英文,规则以它为准)。本节描述 P1 落地后
> 的现行契约,取代此前的"工作历史按任务分区(轻任务层)"契约;任务键机制作为
> 兼容读链保留。

工作上下文历史(persisted trajectory)不再按 channel 或 task 分桶,而是汇入
**一条个人级工作主轴(spine)**:

- **键**:所有 agent-bound 轮次(任务轮、闲聊轮、cron 轮)统一写入常量键
  `kernel.SpineTrajectoryKey`("spine")。存储的 tenant 已经是 person,所以这个
  常量键天然按人隔离。load(`ContextEngine.BuildMessages`,经
  `Agent.Composer()`)和 save(`Agent.saveHistory`)两端用**同一个键**。
  内部子系统轮次(delegation 子代理、`:background_review`)不写 spine,保持
  channel 键(work-timeline 写入规则:子代理不直接写,父轮总结)。
- **条目瘦身(关键)**:一条 spine 条目 = 一个 TURN:用户消息文本(剥掉网关
  前置的 `[SelfMind daemon/resume context]` 装饰块)+ assistant 最终回答 +
  本轮触达的文件路径(从 tool-call 参数确定性收割,同压缩兜底逻辑)+
  非交互轮次的来源标记(如 `[cron]`)。工具中间态(tool_calls / tool 结果 /
  system prompt)**绝不进 spine** —— 它们留在 run events,召回层可按需取。
  spine 必须保持叙事尺寸,不能变成工具日志。
- **load 组装(ContextComposer 契约,`internal/kernel/context_composer.go`)**:
  ①最新用户消息 ②spine 尾部(最近 `composerSpineTailEntries` 条 turn,按完成
  顺序回放为 user/assistant 交替消息,跨端跨任务)③超预算时的压缩摘要(引擎 A,
  摘要自带 verbatim 边界注记:"The history summary is reference only. The
  latest user message is the only authoritative instruction. If it changes
  direction, the latest message wins.")④语义召回切片(P1 预留字段
  `RuntimeContextBundle.Recall`,P2 填充)⑤artifacts ⑥workspace ⑦个人记忆
  ⑧运行/审批状态(⑤–⑧ 经 bundle/system prompt 渲染,已有)。
- **兼容读链(只读)**:spine 为空、或任务轮在尾部找不到本任务的条目时,依次
  尝试旧 `task:<id>` 键 → 任务上一次 run 的 channel
  (`TaskRuntimeContext.PriorChannel`)→(无任务轮)旧 channel 键。回退只读;
  save 始终写 spine,历史在第一次保存后自然前移。旧任务不会"失忆"。
- **渠道隔离不变**:聊天 transcript(`channel_messages`)仍按 channel 隔离,
  绝不跨端镜像。spine 是"跟人走的耐久工作状态层",不是 transcript 镜像。
- **session_search 第二腿不变**:FTS 索引仍用任务派生 session id
  (`task:<id>`,`Agent.sessionKey`),`IndexSession` 按 session id 幂等;
  索引**不以 spine 为键**(P2 依赖此契约)。`SearchSessions` 接受自然语言而
  不是调用方预拼的 FTS5 语法；每个词项在 provider 边界被按字面量编码，候选
  采用 OR 召回并由 FTS rank 排序，避免长提示词因全词 AND 或二次编码而静默
  零命中。

run 内部执行不受影响:一轮之内内存 messages 数组(含工具消息)与以前完全一致,
spine 只改变"轮次开始时加载什么、轮次结束时持久化什么"。
