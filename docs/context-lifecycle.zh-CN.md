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

`internal/kernel/context_engine.go` is intentionally cheap on the default hot
path: it loads only a bounded slice of recent channel history and performs
deterministic trimming. Synchronous LLM summarization is disabled by default
because it delays the first visible token and makes the CLI feel stuck. It can
be enabled only for diagnostics with `SELFMIND_SYNC_CONTEXT_SUMMARY=1`.

Streaming feedback now relies on larger event buffers and critical-event
delivery backpressure. CLI/TUI should show tool and assistant progress live;
IM channels should keep token streams collapsed and use working notices plus
final summaries.
