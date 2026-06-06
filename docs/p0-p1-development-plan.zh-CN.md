# SelfMind P0/P1 开发落地说明

这份文档记录本轮围绕“日常可用”和“越用越懂”的第一批代码改造，方便后续开发继续沿同一架构推进。

## P0：多终端任务连续性

目标路径：

```text
公司 CLI 发起任务
  -> gateway 创建 task/run
  -> agent 在 workspace 内执行
  -> IM 用 /status 查看进度
  -> IM 用 /stop 取消
  -> 回家 CLI 继续查看同一个 task
```

已落地：

- agent 事件流改为 per-run event sink：gateway 通过 `kernel.WithEventChannel(ctx, ch)` 为每次 run 注入事件通道。
- gateway 不再临时替换共享的 `Agent.EventChannel`；该字段只作为本地 TUI 兼容 fallback。
- `api.RunOutcome` 作为结构化任务结果，包含 `status`、`summary`、`done`、`next_steps`、`files`、`tests`、`risks`、`need_approve`。
- run 完成后，gateway 根据 outcome 写入 task status、task handoff 和 `run.finished` event。
- `/status` 输出从普通文本摘要升级为任务卡片：当前状态、active run、summary、done、tests、files、next、risks。

关键文件：

- `internal/kernel/event_context.go`
- `internal/gateway/router/events.go`
- `internal/gateway/httpapi/outcome.go`
- `internal/gateway/httpapi/server.go`
- `internal/gateway/httpapi/run_events.go`

仍需继续：

- 官方 Feishu/WeChat/QQ adapter 的验签、解密、发送器。
- IM 原生审批按钮和 `/approve <id>` fallback。
- 真正 worker pool 或每 run 独立 agent 实例。目前 `Agent` 仍有 `runMu`，并行能力还没有完全打开。

## P1：学习可见性与审计

目标：

```text
memory/skill 发生变化
  -> 写入统一 learning audit
  -> 用户能看见学到了什么
  -> 后续可以撤销、恢复、对比
```

已落地：

- 新增 tenant 级学习审计：`~/.selfmind/<tenant>/learning/learning-log.jsonl`。
- skill 变更会写入 `~/.selfmind/<tenant>/learning/skills/<skill-name>/<change-id>.json`。
- memory 的 add/replace/remove 会写入 learning audit。
- `skill_manage(action=history, name=...)` 可以查看某个 skill 的变更历史。
- gateway 会把 `learning.review` 进一步归类为 `learning.memory.saved`、`learning.skill.created`、`learning.skill.updated`、`learning.skipped` 等事件类型。

关键文件：

- `internal/tools/learning_audit.go`
- `internal/tools/skill_manage.go`
- `internal/tools/memory.go`
- `internal/gateway/httpapi/run_events.go`

仍需继续：

- `/memory history`、`/memory undo <id>`。
- `/skills undo <change_id>`。
- TUI/IM 显示短学习事件，例如 `memory saved`、`skill updated`。
- background review 的异步结果需要更稳定地桥接到 task events。

## 后续约束

- 不要把 run 执行逻辑继续堆进 `internal/gateway/httpapi/server.go`。下一步应继续抽出 run coordinator 或 worker service。
- 不要让 IM adapter 拥有 task/run/workspace/memory/skill 状态。adapter 只负责平台协议。
- 不要让 channel 状态卡自己解析 assistant 原文。状态卡应读取 `RunOutcome`、handoff 和 task events。
- 不要为 memory、skill、review 分别造历史文件。统一写 learning audit。

## 验证

本轮已通过：

```sh
GOWORK=off go test ./...
```
