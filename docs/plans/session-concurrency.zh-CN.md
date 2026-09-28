# 按会话并发执行方案（对齐主流 Agent）

> 生命周期：paused（等待 active 位置；何时切换由项目所有者决定，以 `docs/manifest.yaml` 为准）  
> 日期：2026-09-29  
> 审批：项目所有者（方向与 §0.2 的默认决定，2026-09-29）  
> 复审日期：2026-10-10（与当前 active 计划同日复审）  
> 设计基线：`docs/execution-concurrency-plan.md`（decision / paused）。本计划保留它的终态与不变量，
> 替换它 §12 的推进顺序：先用 session 通道和受管 worktree 达到主流能力；dirty / 非 Git 快照、
> 多仓库组合视图、资源占用声明和跨仓库变更集留在基线里，按需再做。  
> 范围：同一个人的多个 Run 并行执行、同仓库并行写的 worktree 隔离、子代理并行写保护、
> 多任务总览、跨端转交与进度查看。

## 0. 一页读完

### 0.1 结论

今天每人同一时间只有一个 Run 在执行。第二个终端窗口或 IM 聊天的输入，要么插进这个 Run，
要么排队等它结束。Codex、Hermes、pi 都能让同一个人同时跑多个任务，差距在这里。

本计划把准入单位从 person 改成 session：每个终端窗口、每个 IM 聊天各有一条前台通道，同一个人
最多同时运行 N 个 Run（默认 3，可配置）。并发写入默认不共用同一棵目录：同一个 clean Git 仓库
的第二个写 Run 自动进受管 worktree，dirty 或非 Git 的工作区排队并说明原因。跨端同步仍然靠
person 级结构化状态，另外补上跨会话转交和进度查看。

第一个可用里程碑 C1 约 2.5–3.5 周；全部对齐（C1–C4）约 5–6.5 周。

### 0.2 已定的决定

| # | 决定 |
| --- | --- |
| 1 | 准入按 session：每个 session 同时最多一个前台 Run；每人同时运行的 Run 默认最多 3 个，可配置 |
| 2 | 同一 clean Git 仓库的第二个写 Run 自动进受管 worktree 并提示；dirty 或非 Git 工作区排队，并说明原因 |
| 3 | IM 聊天是独立 session，可与 CLI 并行；同一批交付跨会话转交与跨端进度查看 |
| 4 | 子代理并行写保护进首批（C3） |
| 5 | 插话只进本 session 的 Run；跨 session 只能经转交或 `/attach` |
| 6 | 结果回到发起端、审批只在发起端弹面板、desk-first / phone-first 推送规则不变 |
| 7 | worktree 的结果不自动合回用户工作树；合回前预检，冲突逐项列出 |
| 8 | 深水区（§0.4）不在本计划 |

### 0.3 事实依据（可核对）

**主流做法**（2026-09-29 读取的最新浅克隆，只读；行号对应该快照）：

| 能力 | Codex（`openai/codex` @30fc6864c） | Hermes（`NousResearch/hermes-agent`） | pi（`earendil-works/pi` v0.87.1） |
| --- | --- | --- | --- |
| 同一个人并行多个顶层任务 | 一个 daemon 内多个线程，每个线程同时一轮，不设总上限（`codex-rs/core/src/thread_manager.rs:396`、`codex-rs/core/src/session/session.rs:91`） | 每个聊天一个 agent，放在 10 线程的池里；`/bg` 另开后台会话；`max_concurrent_sessions` 默认不限（`gateway/run.py:59`、`hermes_cli/config_defaults.py:42`） | 一个进程一个会话，多开进程并行（`packages/coding-agent/src/core/agent-session-runtime.ts:68`） |
| 运行中来了新消息 | 插进当前这一轮；可选持久队列，最多 100 条 | 默认打断；可选插话或排队（最多 32 条） | 插话或追加 |
| 并发写同一仓库 | 受管 worktree，需显式选择（`--worktree`、`/worktree`，或从 `/agents` 新建任务），基于 HEAD 或默认分支，不带未提交改动；不加锁（`codex-rs/worktree/src/lib.rs:62`） | worktree 需显式选择（`hermes -w`、`/worktree`）；进程内按路径加锁，拒绝覆盖过期内容（`tools/file_state.py`） | 无隔离；进程内同一文件的写入排队 |
| 子代理并行 | V1 最多 6 个、深度 1，共用父级目录（`codex-rs/core/src/config/mod.rs:253`） | 最多 10 个、后台运行，共用工作区与容器（`tools/delegate_tool_config.py:17`） | 示例扩展：8 个任务、同时 4 个，共用目录 |
| 多任务界面 | `/agents` 总览：需要输入 / 进行中 / 已完成（`codex-rs/tui/src/app/agent_center/mod.rs:11`） | `/agents` 跨聊天列表，后台完成通知 | 无 |
| 限流与成本 | 无跨线程调度，429 各轮自行处理 | 线程池上限；429 时轮换凭据 | 只靠重试 |

三家都没有 dirty 仓库与非 Git 目录的快照、资源占用声明、跨仓库变更集。worktree 在 Codex 和
Hermes 里都要显式选择，普通会话和子代理默认共用目录。

**SelfMind 现状：**

- `RunCoordinator.active` 以 `person_id` 为单槽（`internal/gateway/httpapi/run_coordinator.go:42`），
  12 个文件、24 处调用依赖它：插话、停止、状态、出队、诊断、摘要、watcher、续接判定、上下文
  选择、控制命令等。
- worker pool 已接入 daemon（`internal/runtime/gateway/runner.go:285`），但 `SELFMIND_WORKERS`
  默认 1，从未在 N>1 下做过真实压测。
- 能写的 Run 按工作区串行，只读的 Run 可以并行（`TaskStrategy.MayWriteWorkspace`，见
  `docs/worker-pool-design.md` §8c）。
- 按服务商的并发上限与 429 共享退避：设计过，没有实现。
- 运行时没有 worktree 实现；只有自修复命令用过 `git worktree add`。
- 批量委派默认最多 5 个子代理并行、一批最多 16 个，共用工作区，没有写冲突保护
  （`internal/app/delegation.go:26`）。
- 执行根在入队时冻结（schema v18 起，插话也保留自己的执行根），沙箱可写范围由它推出，所以
  worktree 可以直接作为 Run 的执行根接入。

### 0.4 明确不做

- 不自动把 worktree 的结果合回用户工作树。
- 不做 dirty 仓库与非 Git 目录的快照视图，不做多仓库组合视图（基线 §5 的深水区）。
- 不做部署、数据库等外部资源的占用声明（基线 §6），不做跨仓库变更集（基线 §9）。
- 不按请求措辞判断只读，只按能力集合；不确定就按写处理。
- 不引入远程 Runner、多节点调度或 SaaS 能力。
- 不为某个模型或服务商写分支逻辑。

## 1. 目标模型

```text
Person
  └─ Session               一个终端窗口或一个 IM 聊天；一条前台通道
      └─ Run               同一 session 同时最多一个前台 Run
          ├─ Logical Workspace   记忆、任务与项目知识的逻辑范围（不变）
          ├─ Execution View      direct（主 checkout）或受管 worktree
          └─ Frozen Roots        入队时冻结的执行根
```

准入规则：

| 情况 | 结果 |
| --- | --- |
| 本 session 没有前台 Run，且未到每人上限 | 立即执行 |
| 本 session 已有前台 Run | 输入插进这个 Run，由 Main 采纳或另排（今天的语义，范围缩到本 session） |
| 已到每人上限 | 持久排队，状态显示"等待空闲通道（N 个运行中）" |
| 只读 Run | 任何工作区都可并行，不占写入资格 |
| 写 Run，目标工作区没有别的写 Run | 在 direct view 执行 |
| 写 Run，目标是 clean Git 仓库，且已有写 Run | 创建受管 worktree 执行，并提示位置与合回方式 |
| 写 Run，目标是 dirty 或非 Git 工作区，且已有写 Run | 排队，并说明"有未提交改动或不是 Git 仓库，不能并行写" |
| worktree 创建失败 | 排队并说明原因，不回退到 direct |
| 服务商达到并发上限或返回 429 | Run 等待并显示原因，按共享退避重试，不判为失败 |

## 2. 不变量

1. 一个 session 同时最多一个前台 Run；新输入只插进本 session 的 Run。
2. 同一个物理目录同时最多一个写 Run。direct view 与每个 worktree 各算一个目录。
3. 能不能写由 Run 的能力集合决定，不由请求措辞决定；不确定按写。
4. 执行根在入队时冻结。worktree 作为执行根时，逻辑工作区、记忆与任务归属不变。
5. worktree 的结果永不自动合回；合回前预检，冲突逐项列出，不静默覆盖用户的改动。
6. 跨会话转交只能投给本人正在运行的 Run；由目标 Run 的 Main 决定采纳还是另排；两边都有回执。
   守护进程产生的文本不参与转交。
7. 结果路由、审批路由与 desk-first / phone-first 规则不因并发改变；Run 的详细事件仍只发给它的
   session 和显式 attach 的客户端。
8. 服务商并发上限与 429 退避在所有 Run 之间共享；超额只让 Run 等待，不把等待记成失败。
9. 每人上限设为 1 时，行为必须与今天一致，这是回退开关。

## 3. 批次

### C1 按会话并发（2.5–3.5 周）——第一个可用里程碑

| 项 | 改动 | 预计 |
| --- | --- | --- |
| C1.1 | 调度器从 person 单槽改为 (person, session) 通道加每人上限；24 处调用改成按 session 取本通道、按 person 汇总查询 | 3–4 天 |
| C1.2 | 任务的"进行中"从 Run 记录推导，不再依赖 `tasks.active_run_id` 单值 | 1 天 |
| C1.3 | worker pool 默认开启，worker 数与每人上限一致，保留 `SELFMIND_WORKERS` 覆盖；N>1 的真实压测 | 1 天 |
| C1.4 | 按服务商的并发上限与共享 429 退避 | 2 天 |
| C1.5 | 插话限定在本 session；跨会话转交（Main 工具 + 运行时校验 + 双向回执；回复某个 Run 的 IM 通知消息时直接转交）；跨端查看进行中 Run 的进度 | 3–4 天 |
| C1.6 | 呈现：状态行显示运行数；别的 session 的 Run 完成或需要输入时通知；IM 聊天与 CLI 并行 | 1–2 天 |
| C1.7 | eval 支持多 session，补齐跨会话用例；真实压测：每人上限 3，CLI 长任务、微信和 cron 同时运行 | 3–4 天 |

验收：

1. 两个终端窗口在不同工作区同时完成各自的写任务。
2. CLI 跑长任务时，微信发的新任务立即开始并完成，结果回到微信。
3. 同一工作区的第二个写 Run 排队（C2 之前），显示原因；第一个结束后自动开始。
4. 在微信里补充一句给 CLI 正在跑的 Run，经转交生效，两边都有回执；在微信里问进度，直接在微信
   得到回答。
5. `/status` 列出本人所有运行中和排队的 Run。
6. 服务商返回 429 时 Run 等待重试，不判为失败。
7. 压测期间 control.db 没有锁超时，没有事件串到别的 session。
8. 每人上限设为 1 时，现有 eval 用例全部照常通过。

### C2 受管 worktree（1–1.5 周）

- **何时创建**：写 Run 进入一个已有写 Run 的 clean Git 仓库时自动创建；`--worktree` 或
  `/worktree` 可以主动要求。
- **怎么创建**：`git worktree add --detach` 到 SelfMind 数据目录下的受管位置，基于当前 HEAD，
  并为 Run 建一个命名分支（如 `selfmind/<run 简称>`）。未提交的改动不带过去，提示里会说明。
- **怎么执行**：Run 的主执行根指向 worktree，沙箱可写范围随之指向 worktree，逻辑工作区不变。
- **怎么交付**：Run 结束时，结果是 worktree 里的分支和 diff。`/apply` 先 `git apply --check`
  预检，再用 `--3way` 合回；冲突逐项列出；不合回也可以保留分支。
- **生命周期**：`/worktree list`、`/worktree remove`；有未合回或未提交内容的 worktree 不自动删除；
  合回后清理。
- **失败**：创建失败时排队并说明原因，不回退到 direct。

验收：同一 clean 仓库的两个写任务并行完成，各自产出 diff，`/apply` 合回且测试通过；dirty 仓库
排队并提示；worktree 里有未合回内容时，删除会被拒绝并提示。

### C3 子代理并行写保护（3–5 天）

- 批量委派里，只读子代理照常并行；能写的子代理默认依次执行，显式要求时各自进 worktree。
- 同一视图内的文件写入加过期检查：写入前核对内容，文件已被别的 Run 或子代理改过就拒绝覆盖，
  并告诉模型重新读取。
- 能执行终端命令的子代理无法追踪写入范围，一律按能写处理。

验收：两个子代理改同一个文件不会互相覆盖；只读子代理的并行度不变；过期写入被拒绝后，模型能
重新读取并完成。

### C4 多任务总览（3–5 天）

- `/agents` 总览：按"需要输入 / 进行中 / 排队 / 已完成"列出本人所有 session 与 IM 的 Run，可以
  直接观察（`/attach`）或停止。
- IM 端提供精简列表。
- 跨会话审批沿用 `/approvals` 与 `/approve`；可选增加 `/approve all`。

验收：三个 Run 并行时，总览的状态与各 session 一致；从总览停止一个 Run，只停这一个。

## 4. 完成后的对齐情况

| 能力 | 主流 | 本计划完成后 |
| --- | --- | --- |
| 同一个人并行多个任务 | Codex、Hermes 不设上限；pi 多开进程 | 每人默认 3 个，可配置 |
| 并发写同一仓库 | worktree 需显式选择，默认共用目录 | clean Git 仓库自动 worktree；dirty / 非 Git 排队 |
| 子代理并行写 | 共用目录，基本没有保护（Hermes 有进程内路径锁） | 只读并行，写入串行或进 worktree，加过期写入检查 |
| 多任务界面 | Codex 的 `/agents` 总览最完整 | `/agents` 总览 + IM 精简列表 |
| 限流与成本 | 三家都没有跨任务调度 | 按服务商共享并发上限与 429 退避 |
| 持久与恢复 | Codex 重启后续跑一轮；Hermes 子代理不持久 | 沿用持久队列、运行登记与恢复通知 |

## 5. 多端同步

跨端共享的是 person 级结构化状态，不是对话记录。并行之后，身份、记忆、工作记录、结果路由、
审批推送和 `/attach` 观看都不变，变的只有执行层。

| 场景 | 今天 | 计划完成后 |
| --- | --- | --- |
| CLI 在跑，微信发新任务 | 插进 CLI 的 Run，或排队 | 微信立即开跑，结果回到微信 |
| CLI 在跑，微信问进度 | 插进 CLI 的 Run，回答出现在 CLI，微信只收到回执 | 微信读取 CLI Run 的计划与最近进展，直接在微信回答 |
| CLI 在跑，微信补一句给这个任务 | 自动插进去 | 经转交进入 CLI 的 Run，两边都有回执 |
| CLI 任务已结束，微信说"继续" | 新 Run 借助工作记录接着做 | 不变 |
| 两端改同一个工作区 | 串行 | clean Git 仓库自动 worktree，否则排队 |
| 离开电脑时，CLI 任务要审批或已完成 | 推到常用 IM | 不变 |

取舍：并行的 Run 互相看不到对方的中间对话，只能看到结构化进度与已完成的结果；同一件事可能在
两端各开一次。新 Run 的上下文会带上本人正在运行的 Run，由 Main 选择转交、查看还是独立做。

## 6. 风险与回退

| 风险 | 影响 | 应对 |
| --- | --- | --- |
| 服务商限流（qwen 的 TPM / QPM） | 并行时频繁 429 | C1.4 共享上限与退避；等待不判失败 |
| token 花费成倍增加 | 成本上升 | 按 Run 显示用量；每人上限可调 |
| control.db 是 SQLite 单写入者 | 并发高时锁等待 | C1.7 压测；锁等待指标进入 daily report |
| 插话语义变化 | 跨端补充不再自动进入别的 Run | 转交 + 回执；回复 IM 通知消息时确定转交 |
| 转交误判 | 该独立做的被转交，或反过来 | 目标 Run 的 Main 再判断一次；回执说明去向 |
| worktree 的使用习惯 | 改动在另一个目录，需要合回 | 提示位置与 `/apply`；总览列出未合回的结果 |
| git 边界情况（子模块、LFS、hooks） | worktree 创建失败或内容不完整 | 失败即排队；含子模块的仓库先不自动开 worktree |
| 磁盘占用 | worktree 累积 | 合回后清理；`/worktree list` 显示占用 |
| 并发缺陷 | 状态错乱 | race 测试、注入故障的 Go 测试、真实压测 |

回退：每人上限设为 1，恢复今天的串行行为；worktree 自动创建可以关闭，退回排队；转交可以关闭，
只保留 `/attach`。每一项都能单独回退。

## 7. 验证

每个批次的工程底线：build、test、race 测试、完整离线 eval、docs check、打包安装、daemon 重启。

必须能证明的行为，除 C1–C4 的验收条目外：

1. 每人上限为 1 时，现有 eval 用例全部照常通过。
2. 多 session eval 覆盖并行执行、session 内插话、跨会话转交、跨端进度查看、审批只在发起端弹面板。
3. 用真实模型（qwen3.8-flash）为转交与进度查看各录一个 eval 用例并提交录制。
4. 真实压测记录并行数、排队时间、429 次数、SQLite 等待与每个 Run 的 token 用量。

## 8. 文档、规则与 schema 影响

- `AGENTS.md`：把"每人一个 active run"的不变量改成按 session 准入加每人上限。`AGENTS.md` 只剩约
  200 字节余量，改之前要先把其他细节移到领域文档。
- `docs/STATUS.md`、`docs/identity-continuity.md`（插话与转交）、`docs/work-timeline.md`、
  `docs/worker-pool-design.md`（顺带更正"只接在本地 CLI 路径"的过时说法）、`docs/tool-safety.md`
  （子代理并行写）、`docs/command-reference.md` 及中文翻译（`/worktree`、`/apply`、`/agents`）、
  `docs/tui-terminal-first-hybrid.md`。
- `docs/execution-concurrency-plan.md`：记录本计划替换其 §12 的推进顺序，终态与不变量保留。
- schema：worktree 登记与 session 通道状态可能需要新表或新列，按版本迁移，迁移前备份，并带历史
  版本的升级测试。
- 对应批次完成前，现状文档不得把目标能力写成已实现。

## 9. 工期

| 批次 | 内容 | 预计 |
| --- | --- | --- |
| C1 | 按会话并发、限流、转交与进度查看、多 session eval、压测 | 2.5–3.5 周 |
| C2 | 受管 worktree 与 `/apply` | 1–1.5 周 |
| C3 | 子代理并行写保护 | 3–5 天 |
| C4 | 多任务总览 | 3–5 天 |
| 合计 | | 5–6.5 周 |

对比设计基线原来的推进顺序：到 P4 才开放并发（约 5–8 周，初始上限 2），P2–P5 全部完成约 2–3 个月。
