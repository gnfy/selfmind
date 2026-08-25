# 执行并发、Workspace View 与 Runner 演进 — 设计决策

> **状态：Paused decision，未进入当前实现优先级。**
>
> Owner / approver：SelfMind project owner
>
> 下次评审：当前 active plan 得出 verdict 后，最迟 2026-09-12
> 当前唯一活跃优先级仍以 `docs/STATUS.md` 为准。本文不授权实现 SaaS、
> 多租户、远程控制面或独立 Runner；任何实施批次必须先进入活跃计划。

本文定义 SelfMind 从“每人一个 active run”演进到安全多 run 并行时的完整执行模型。
最终目标是：**不同 logical workspace 可以并行；同一个 logical workspace 也可以有多个
run 并行；workspace 是否相同不再决定调度串行。** 文件、Git、部署、数据库等真实资源
是否冲突，由 execution view 与资源声明决定。

本文取代旧版以“一个 workspace 约等于一个 Git repo”为前提的方案。它同时覆盖：

- 单 Git repository；
- 一个 Git monorepo 中的多个 project；
- 根目录不是 Git、下面有多个 sibling repositories；
- 多 repositories 与根目录 loose files 混合；
- dirty Git、submodule、nested repository；
- 完全非 Git 的 workspace。

相关边界分别归属：

- `docs/identity-continuity.md`：person、endpoint、conversation 与 watcher 契约；
- `docs/work-timeline.md`：task、run、work spine 与 continuation；
- `docs/worker-pool-design.md`：本地 Agent worker pool；
- `docs/execution-engine.zh-CN.md`：本地执行信封、环境快照与沙箱；
- `docs/tool-safety.md`：执行 scope、审批、凭据和工具安全；
- 本文：并发所有权、session lane、workspace topology、execution view、资源声明和演进顺序。

---

## 1. 决策摘要

### 1.1 终态定义

> 并发准入按 session，连续性按 person，工作标签按 task，执行隔离按 view，冲突按
> physical resource；workspace 本身永远不是串行锁。

```text
Person / Tenant
  └─ Session                         transcript、交互归属、一个 foreground lane
      └─ Run                         一次可调度、可取消、可审计的执行
          ├─ Logical Workspace       记忆、任务和项目知识的逻辑范围
          ├─ Workspace Baseline      入队时冻结的 topology/context/environment
          ├─ Execution View          工具真实看到的复合文件视图
          └─ Resource Claims         文件、Git ref、部署目标、数据库等物理资源
```

同一个 logical workspace 的并行语义是：

```text
logical workspace ws_1
  ├─ direct view                    主 checkout；一个 broad writer
  ├─ isolated composite view A      run A 独占
  └─ isolated composite view B      run B 独占
```

多个 run 可以同时执行，但不能把“并行”误解为多个执行体同时修改同一棵物理目录。
worker、provider、person/tenant 配额仍可让 run 等待；这些是容量限制，不是 workspace
身份带来的串行。

### 1.2 核心决策

1. `person_id` 是身份、记忆、工作史和配额边界，不是执行锁。
2. `session_id` 是 transcript、实时事件和 foreground run 的交互 lane；每个 session
   同时最多一个 foreground run。
3. `task_id` 是可逆工作标签，不是上下文边界，也永远不作为并发锁。
4. run 在 durable admission 时获得 `run_id`，并冻结 workspace、baseline、context
   cursor、能力集合和 delivery route；drain 不重新猜这些信息。
5. logical workspace、repository/component、project/build unit 和 execution view 是四个
   不同层次。
6. 一个 write run 必须拥有一个 execution view；同一物理 view 同时最多一个 writer。
7. direct view 的执行体未证明退出前，写 claim 不得转让；quarantine 不是释放权限。
8. read-only 来自所有可激活工具的封闭能力集合，不来自请求措辞；不确定即按 write。
9. raw progress 只发给 owner session 和显式 attached observers；final reply 仍遵守
   conversation routing，不因 attach 改变目的地。
10. 跨 repository 的交付是 versioned `WorkspaceChangeSet`，不是假装成一个 branch；
    集成回 direct 串行且承认跨 repo 更新不是天然原子事务。

### 1.3 非目标

当前本地并发阶段不实现：

- OIDC、组织账号、计费、seat、work profile 或企业角色；
- PostgreSQL、分布式调度器或多 gateway leader election；
- mTLS、Runner 注册、远程 capability token 或跨节点 artifact transport；
- 自动把两个冲突结果合并回用户工作树；
- 通过自然语言关键词猜 read-only、目标 repository 或资源锁；
- 为临时灰度控制增加长期公共配置项。

---

## 2. 当前实现边界

以下现状必须先迁移，不能直接放开并发：

1. `RunCoordinator` 的 active registry 以 `person_id` 为单槽；steer、stop、drain 和
   status 都依赖该唯一性。
2. `tasks.active_run_id` 是单值。两个 run 关联同一 task 时会互相覆盖任务状态。
3. `tools.executionScopes` 同时注册 person/run key，工具仍可从 `_tenant_id` 回落到
   进程级 person scope。
4. `EnsureWorkspace` 会隐式更新 person current workspace；execution/drain 仍可能回读
   该可变值。
5. durable `control.Event` 已有 channel，但 `api.RunEvent` 丢失该字段；live broker 按
   person 广播，多个 CLI 会串流。
6. queue 已经有 class、priority、not-before、claim token、lease 和 attempt generation；
   真正缺少的是冻结后的 session、baseline、view policy、context cursor 和 run identity。
7. `Workspace` 只有一个 `RepoURL/DefaultBranch`，无法表达 aggregate workspace。
8. `WorkspaceContext` 只有 `ID + Root`，逻辑项目根和物理执行根仍被合并。
9. project profile 支持根或一级多 project，但不拥有 repository topology；根有 manifest
   时不会继续发现子项目。
10. context scanner 从给定路径向上走到 Git/home 边界，不会自动进入多个 child repo
    加载各自指令。
11. `execution_leases` 绑定环境和 credential references，不是互斥资源锁。
12. control store 和 person memory 的串行路径在低并发下成立，但并发开启前需要压测。

run scope、workspace freeze、task active 状态和 session 事件串流是现有正确性缺陷；即使
保持单 run 也值得修复。execution view、自动并发和 Runner 必须等待独立计划。

---

## 3. 四层 Workspace 模型

### 3.1 Logical Workspace

Logical workspace 负责 workspace 级任务、记忆、知识、根目录约定、component topology、
Runner placement 和跨 repositories 的 ChangeSet。它不是 repository、project、view 或
资源锁。现有 `RepoURL/DefaultBranch` 在迁移期只作为单 repo workspace 的兼容展示字段。

### 3.2 Workspace Component

Component 是最小的可独立冻结、隔离和归因单元：

```text
WorkspaceComponent
  ComponentID
  WorkspaceID
  MountID
  RelativePath
  Kind                  git | filesystem
  RepositoryIdentity    Git 才有；不使用绝对路径作为跨节点身份
  ParentComponentID     submodule/nested repo 关系
  ProvenanceFingerprint
  TrustLevel
```

例如：

```text
/work/company-platform                  logical workspace
  ├─ AGENTS.md                          workspace-level guidance
  ├─ Makefile                           loose-files component
  ├─ frontend/.git                     component: repo-frontend
  ├─ backend/.git                      component: repo-backend
  ├─ infra/.git                        component: repo-infra
  └─ local-compose.yaml                loose-files component
```

一个 aggregate workspace 可以包含多个 Git components 和非 Git regions。一个 Git
monorepo 通常只有一个 Git component，但可以包含多个 project units。

### 3.3 Project / Build Unit

Project unit 来自 `go.mod`、`package.json`、`Cargo.toml` 等 manifest，负责 ecosystem、
package manager、verification candidate 和 cwd。Project 不是并发锁。

### 3.4 Execution View

Execution view 是一个 run 实际读取和写入的物理视图：

| Kind | 用途 |
| --- | --- |
| `direct` | 原始主 checkout；一个 broad writer |
| `git_worktree` | clean Git component 的独立工作树 |
| `filesystem_snapshot` | dirty Git、非 Git、loose files 的冻结副本或 COW view |
| `composite` | 多个 component views 按原相对路径组装成一个 workspace root |
| `remote` | 未来由 Runner 解析的容器、volume 或 VPC view |

物理路径属于执行节点。Gateway/control store 持久化 `view_handle`、mounts、hash 和状态，
不把本机绝对 root 当作跨节点权威事实。

---

## 4. Workspace Topology 与 Baseline

### 4.1 发现规则

Topology discovery 必须确定性、只读、语言无关且 symlink-safe：

1. 显式注册 components 优先；
2. workspace root 自身是 Git repo 时先建立一个 Git component；
3. root 不是 Git 时，在 AllowedRoots 内发现 `.git` directory 或 worktree/submodule 的
   `.git` file；
4. 识别 common Git directory，使相关 worktrees 共享 repository identity；
5. 不跨越 AllowedRoots，不跟随越界 symlink；
6. 跳过 `.selfmind`、VCS metadata、dependency、build 和 cache 目录；
7. 记录 submodule/nested repository 关系，不把 child repo 重复算进 parent region；
8. 扫描有深度、数量和 wall-clock 上界，并返回 omission count 与
   `topology_incomplete`，不能静默截断。

提示词发现可以有预算上限，执行隔离不能把截断的 inventory 当成完整 topology。发现
不完整时，write run 只能使用完整 workspace snapshot、明确收窄到完整注册 components，
或 fail closed。绝不能隔离前几个 repositories，剩余路径继续指向原 checkout。

### 4.2 入队时冻结 Baseline

单值 `base_commit/base_dirty` 不足以表达 aggregate workspace：

```text
WorkspaceBaseline
  BaselineID
  WorkspaceID
  TopologyVersion / TopologyHash
  ContextCursor / CapturedAt
  Components[]
    ComponentID / RelativePath / Kind / RepositoryIdentity
    HeadOID / Branch / StatusFingerprint          Git
    WorkingTreeSnapshotID / SubmoduleState        dirty Git
    TreeHash / SnapshotID                         filesystem
```

规则：

- clean Git 冻结本地精确 `HEAD OID`；
- fresh-remote 策略必须在接受任务前解析成具体 OID，不能 drain 时再追 tip；
- dirty Git 冻结 `HEAD OID + working tree snapshot`，不能只有 dirty bool；
- 非 Git/loose files 冻结 tree hash 与 snapshot reference；
- baseline 捕获不完整时，run 不得伪装成已稳定接收；
- run 中新 clone 的 repo 是该 view 的 topology change，完成时进入 ChangeSet。

### 4.3 Context 快照

每个 run 同时冻结 person work spine high-watermark：

- 使用本 session 的 channel-local transcript；
- 使用 person spine 截至 `context_cursor` 的已完成记录；
- 不读取 sibling run 的中间事件；
- sibling 完成后原子追加 slim spine entry，只有后续新 run 可见；
- foreground prompt cache namespace 使用 session/prefix identity，不把共享 task 作为唯一
  namespace。

---

## 5. Composite Execution View

### 5.1 形态映射

| Workspace 形态 | View 计划 |
| --- | --- |
| 单 clean Git repo | 一个 Git worktree |
| 单 dirty Git repo | worktree + frozen overlay，或 filesystem snapshot |
| Git monorepo、多 projects | 一个 repo worktree，多个 project profiles |
| root 非 Git、多个 clean sibling repos | composite：每 repo 一个 worktree，root loose files 一个 snapshot |
| 多 repo、部分 dirty | clean repo 用 worktree；dirty repo 用 snapshot/overlay |
| 完全非 Git | 整体 filesystem snapshot |
| submodule | 父子 components 分别冻结；父记录 gitlink，子保留自身状态 |
| topology 不完整 | 完整 workspace snapshot，或 fail closed |

aggregate isolated view 必须保留原相对目录布局：

```text
<view_handle>/root/
  ├─ Makefile                   snapshot mount
  ├─ frontend/                 worktree mount
  ├─ backend/                  worktree mount
  ├─ infra/                    worktree mount
  └─ local-compose.yaml        snapshot mount
```

### 5.2 View 选择

策略来自能力和资源状态，不来自关键词：

1. 封闭 read-only tool set：对稳定 direct 获取 shared-read；direct 正在写时用冻结 view
   或等待。
2. foreground write 且 direct 空闲：可用 direct，保持单 CLI 体验。
3. direct 已占、background/async 或显式隔离：创建 isolated composite view。
4. worktree 只用于可证明可重建的 component；dirty/non-Git 使用 snapshot provider。
5. 显式隔离 materialization 失败：fail closed，不静默回退 direct。
6. read-only run 不得原地升级 write/execute；需要升级时结束并重新 admission。
7. unknown/external/deferred tool 若可能写入或执行，保守按 write。

### 5.3 Direct View

第一版保持 direct workspace 一个 broad writer。未来可增加 component-scoped direct
writers，但必须同时满足 target components 已冻结、`WritableRoots` 已缩窄、terminal
无法写其它 component、不运行 root integration command，且 resource claims 完整。

final goal 不依赖这个优化；同 workspace 并行通过 isolated views 已经成立。

### 5.4 Read/Write Roots

现有 `WorkspaceRoot + AllowedRoots` 演进为：

```text
LogicalWorkspaceID / ExecutionViewID / ViewRoot
ReadableRoots / WritableRoots / ComponentMounts / RunnerID
```

- isolated writable roots 只指向 view；原 checkout 默认不暴露；
- attachments 是 read-only roots；
- component-scoped direct 只能写已声明 components；
- logical workspace context 服务记忆/task/project knowledge；execution view context 服务
  cwd、文件和工具，二者不能继续共用一个 Root。

### 5.5 Snapshot 安全与成本

Snapshot provider 必须处理 tracked/untracked/ignored/binary/sparse files、symlink、mount
boundary、大型 dependency/build/cache、Linux reflink/overlay、macOS clone/copy fallback、
disk quota、progress、GC 与 crash recovery。

默认不复制密钥或 operator credential state。凭据继续通过 EnvironmentLease、credential
profiles 和节点内 ProcessMaterial 提供；大型或敏感 ignored state 需要显式 profile，不能
因“复制 workspace”进入 artifact、control.db 或 prompt。

---

## 6. Resource Claims、取消与 Quarantine

### 6.1 与 EnvironmentLease 分离

EnvironmentLease 绑定运行环境；互斥使用独立的 ResourceClaim：

```text
ResourceClaim
  ResourceKey / HolderRunID / Mode(shared|exclusive)
  Generation/Fence / HolderInstanceID
  State(waiting|held|quarantined|released)
  HeartbeatAt / ExpiresAt
```

一资源可有多个 shared holders，不能用 `resource_key PRIMARY KEY` 单行表达全部 claim。
Gateway 负责调度声明；执行节点负责物理 enforcement。

### 6.2 Key 空间

```text
tenant:<t>/runner:<r>/view:<view_id>
tenant:<t>/runner:<r>/direct-component:<workspace_id>:<component_id>
tenant:<t>/gitstore:<repository_identity>
tenant:<t>/gitref:<repository_identity>:<branch>
tenant:<t>/workspace-integrate:<workspace_id>
tenant:<t>/deploy:<environment>
tenant:<t>/db:<cluster>
```

isolated file writes通常只持自己的 view；worktree lifecycle/fetch 短时持 gitstore；ref
mutation 持 gitref；ChangeSet apply 持 workspace-integrate 与目标 direct components；
deploy/database 等外部副作用独立于文件 view。

### 6.3 多资源与等待

资源集合必须作为一个 admission unit 原子获取，或具备完整 rollback，不能各持一半互等。
等待期间不占 SQLite write transaction，可以被 stop 取消，并持续发
`waiting_resource`，包含持有者 run/session 的安全摘要。

### 6.4 取消与 Quarantine

1. worker 只有在执行 goroutine/进程真正退出后才归还。
2. direct holder 未证明死亡前，写 claim 不释放、不转让。
3. isolated view 卡死时标记 quarantined；不能删除或复用，但可创建新 view。
4. git/external claims 按真实副作用处理，不能因文件 view 可丢弃就释放 deploy/db 权限。
5. fence 不能阻止仍活着的本地 goroutine 写盘；context propagation、child process group
   termination 和未来可杀死 worker process 是前置。
6. status/doctor 展示卡住的 run、view、claim、持续时间和受影响队列。

---

## 7. Session、Run Registry 与事件面

### 7.1 身份层次

```text
Person               跨 endpoint 连续
Account              已绑定的平台身份
Session              transcript、交互 lane、run owner
Client Instance      当前在线连接，可 attach session/run
Run                  执行与控制的精确目标
```

CLI UUID channel 和 IM chat channel 可作为迁移期 session source，但长期 SessionID 必须由
Gateway 在认证后的 tenant/person/account 下解析或创建。Channel 保留为 delivery locator。

### 7.2 Durable Run Registry

```text
byRunID
foregroundBySession
runsByPerson
```

- admission 时创建 run id 与 queued run；queue 只引用 run；
- 每 session 一个 foreground run，可有多个 queued requests；
- person/tenant 只做 active 配额；
- task 可关联多个 run，`task_runs` 是运行事实源；
- task status 从 runs 派生，单值 `active_run_id` 不再决定生命周期；
- finalization 先存 per-run outcome/handoff，再由 deterministic reducer 更新 task view。

### 7.3 Event Envelope 与 Audience

```text
tenant_id / person_id / session_id / run_id
audience = session | run | person
event_id / cursor / live_seq / item_id / call_id
durability / payload_version
```

- assistant/tool/token/plan：owner session + 显式 attached observers；
- 生命周期和后台完成摘要：person audience，但不得带 transcript/prose 明文；
- final reply：遵循 conversation routing；attach 不改变 delivery owner；
- replay 按 audience 授权过滤；
- 不支持 session protocol 的旧客户端在并发模式下只得人级摘要。

Presence 扩展到 session/client instance。显式共享同一 session 的客户端可共同看到面板；
其它 session 不被抢焦点。

### 7.4 精确控制与 Approval

- “继续”只 steer 当前 session 的唯一 active run；没有则按正常 ingress/queue；
- `/stop` 默认停当前 session；`/stop <run_id>` 精确跨 session；
- `/attach <run_id>` 只增加 observation/steering，不移动 final reply；
- approve/resume 唯一候选可省略 id，多候选必须选择；
- 控制命令保持 model-free。

Approval 不按 person 无条件广播，也不固定 owner session。Gateway 根据 origin、presence
和 desk-first/phone-first policy 选择交互 target；其它 CLI sessions 只显示摘要。任一已授权
endpoint 可用 approval id 接管，resolution 向所有已展示 surfaces 发幂等撤下事件。

---

## 8. Component-aware Context、Memory 与 Verification

### 8.1 Project Instructions

```text
operator agent.md
  → workspace root AGENTS.md
      → component/repo AGENTS.md
          → deeper path AGENTS.md
```

workspace 根指令作用全部 components；repo 指令只影响对应路径；deeper 按 root-to-leaf
覆盖。所有 repository instructions 仍是不可信数据，一个 repo 不能污染另一个。

为避免把几十个 repo 全塞进 prompt，引入 component activation：

- 明确受信路径、read/search/cwd 的实际证据触发激活；
- 激活后在下一模型边界注入 component guidance/profile；
- active set 在 work unit 内单调增长；
- write 未加载 guidance 的 component 返回 `component_context_required`，不得先写后补；
- 大 workspace 只注入 topology summary、active components 和 omission count。

该机制依赖路径/工具证据，不新增自然语言 repository classifier。

### 8.2 Project Profile 与验证

Project discovery 按 `workspace → components → projects` 组织，每个 verification candidate
带 cwd：

```text
backend/api       go test ./...
frontend          pnpm run test
infra             terraform validate
workspace-root    make integration-test
```

只从 manifest、lockfile 和 declared scripts 生成；验证 touched projects；root integration
只有明确证据时运行；预算按 active component 分配并报告 omission；aggregate root 自身有
manifest 时不能遮住 child repos。

### 8.3 Memory 与 Work Spine

workspace 仍是项目/环境记忆的主要 scope，但允许 component qualifier：workspace-global
存跨 repo 约定，workspace+component 存 repo 特定事实。run 只注入 active components 的
局部记忆；touched paths、artifacts、handoff 和 spine entry 同时记录 component ids。

---

## 9. WorkspaceChangeSet 与集成

### 9.1 交付模型

```text
WorkspaceChangeSet
  ChangeSetID / RunID / WorkspaceID / BaselineID / TopologyHash
  ComponentChanges[]
    ComponentID / Kind / BaseRevision|TreeHash
    Branch|Commit|Patch|Artifact
    ChangedPaths / Verification
  RootFilesPatch / IntegrationOrder / Conflicts / Risks
```

submodule 先产生 child commit，再更新 parent gitlink，最后验证父 workspace。新 clone、
component move/remove 或 topology change 也显式列出。

### 9.2 Merge/Apply

跨 repos 没有天然原子事务。第一版只交付 ChangeSet，不自动写 direct。未来显式 apply：

1. 获取 workspace-integrate 与全部 direct claims；
2. 校验所有 component revision/tree hash；
3. 在临时 integration composite view 预演全部变更；
4. 运行 component 和 root integration verification；
5. 全部通过后按 deterministic order 更新 direct；
6. 每一步写 durable journal；
7. 失败时报告 applied/pending，不 silent reset 用户工作树。

自动回滚必须先有可验证 checkpoint，删除/覆盖类操作仍走正常授权。

---

## 10. Scheduler、配额与性能

### 10.1 工作分类

| 类型 | Agent worker | 语义 |
| --- | --- | --- |
| Foreground run | 是 | 当前 session 等待，最高工作优先级 |
| Watch finalization | 是 | 短、幂等，低于交互前台 |
| User background/async | 是 | 有限并发，不占前台保留槽 |
| Cron | 是 | 按 priority/not-before 调度 |
| External watcher | 否 | 独立 poller，完成后提交 finalization |
| Maintenance | 独立 job | 不抢 Agent 前台槽 |

approval/steering 是控制消息，直达 run，不作为 worker job 排队。

### 10.2 公平性与容量

- `per_session_foreground=1` 是交互不变量；
- person/tenant/provider/runner limits 是策略配额；
- scheduler 在 session lanes 间 round-robin；
- 至少保留一个 foreground slot；
- provider 并发上限、429 backoff 与 route isolation 是开并发前置；
- 等待原因使用 `waiting_worker/resource/provider` 和未来 `waiting_runner`。

### 10.3 Worker、Cache 与 Storage

Agent worker 继续一次一个 conversation。正确性和 provider prompt cache 不依赖固定 worker；
session affinity 只能是度量后的 warm-state 优化，忙时必须回落。

默认并发大于 1 前：control store 保持单 writer但评估 bounded read pool；wait/heartbeat
不长持 write transaction；memory provider 评估 per-tenant actor/shard；记录 DB wait、event
volume、view bytes、materialization time。不能凭推测宣布 SQLite 能承载固定并发数，用
`max_active=2` soak 建立基线。

---

## 11. Gateway 与未来 Runner

当前仍是一个 `selfmind` daemon。类型需要 Runner-ready，但不预建远程协议：

```text
Gateway/control plane
  identity/session/run/queue/baseline refs/approval/scheduler
  resource claims/event cursor/ChangeSet metadata

Execution implementation / future Runner
  physical paths/view materialization/Git/terminal/patch/sandbox
  environment snapshot/credential material/process lifecycle
```

Gateway 持 versioned handles，不持 Runner 绝对路径或 credential bytes。源码和 snapshot
默认留在执行节点；模型只得到任务所需有界切片。

演进触发器独立：

1. 本地并发：本文 P0–P5，单 daemon、SQLite、本地执行；
2. 远程 Gateway：强认证、TLS、API/event version、stream resume；
3. Runner：同一 binary 的 runner 角色、设备身份、mTLS、短期 token、heartbeat、
   lease/revoke、offline queue、artifact integrity；
4. SaaS control plane：独立策略批准后才加入 tenant identity、PostgreSQL/object store、
   OIDC、配额和多租户隔离；
5. Enterprise governance：只由真实试点触发。

Aggregate workspace 在本地和第一版 Runner 阶段绑定一个执行节点。跨多个 Runner 组成一个
workspace 会引入分布式 snapshot/transaction，不在当前范围。

---

## 12. 分阶段实施

P4 前 `max_active_per_person` 保持 1；先证明隔离，再打开并发。

### P0 — 现有正确性，不改变并发行为

- execution authority run-keyed，删除 person fallback；
- tools 从可信 invocation scope 精确解析 scope；
- `EnsureWorkspace` 只注册，不隐式切 current workspace；
- admission 冻结 workspace/session，execution/drain 不回读 person current workspace；
- admission 创建 durable run id；task active 从 runs 派生；
- blocking/network/process/write tools 完成 cancellation；
- plan/process registry/ledger 做 run-scoped fail-closed audit。

验收：单槽行为不变；`-race` scope 不交叉；queue 永远在接受时 workspace 执行；旧库升级
和恢复通过。

### P1 — Session 事件与控制面，仍不并发

- SessionID/ClientInstanceID；event audience、channel preservation、session broker；
- owner/observer/summary 路由；approval target policy；
- steer/stop/approve/attach 精确 run；old-client negotiation；
- foreground prompt cache namespace session-safe。

验收：两个 CLI 不串 transcript；B 的“继续”不进 A；approval 只在策略 target 弹面板；
attach 可观察但不移动 final reply。

### P2 — Workspace Topology，仍不并发

- components/projects/topology version；
- clean/dirty/non-Git/submodule baseline；root loose-files region；
- component-aware AGENTS/Profile/memory；touched component evidence；
- shadow 生成 view plan、snapshot size 和 omission diagnostics，仍在 direct 执行。

验收：单 repo、monorepo、多 sibling repos、mixed root、submodule、dirty/non-Git fixtures
均得到确定 topology；计划与实际 touched paths 一致，遗漏显式可见。

### P3 — Composite View，仍不并发

- view handles、component mounts、Readable/WritableRoots；
- worktree、dirty/non-Git snapshot、composite layout；
- lifecycle/recovery/quarantine；WorkspaceChangeSet；默认不 auto apply。

验收：isolated run 完成真实多 repo 修改，原 workspace checksum 不变；materialization
failure 不写 direct。

### P4 — Resource Claims 与灰度并发

- shared/exclusive claims 与原子多资源 acquisition；
- run registry 三层索引、per-session foreground、fair scheduler；
- foreground reservation、provider limits；
- 首次只开放 `max_active=2`、本地 CLI、显式并发、禁止 auto integration；
- 顺序开放：不同 workspace → 同 workspace 不同 clean components → 同 repo worktrees →
  mixed snapshot views；direct broad writer 仍一个。

验收：同 person 两 sessions 在不同/相同 workspace 都能并行；deploy/db key 不并发；
isolated 卡死不影响其它 view；direct 卡死不转让 claim。

### P5 — 集成、跨平台与默认化

- integration view/journal；cross-repo preflight/verification/apply；
- Linux/macOS snapshot providers；large/ignored/binary/symlink policies；
- disk quota、GC、orphan cleanup；IM/cron/background 并发；
- 成本、DB contention、429、cache 指标达到 reviewed gate 后才考虑默认并发大于 1。

到 P5 才正式宣称：logical workspace identity 不再串行 runs，单/多 Git、dirty/non-Git
workspace 均有安全并行路径。

远程 Gateway、Runner、SaaS、Enterprise 不是 P5 自动后续，必须各自触发和批准。

---

## 13. 风险、灰度与回退

整体一次性实施风险高；分阶段并在 P4 前保持单 run，可将每批风险控制在中等范围。

| 领域 | 风险 | 主要失败 |
| --- | --- | --- |
| run scope/workspace freeze | 中 | 旧任务恢复失败、scope missing |
| session event/control | 中高 | transcript 泄漏、steer/stop 错目标 |
| task multi-run lifecycle | 高 | phantom running、summary 覆盖 |
| topology/context | 中高 | 漏 repo、指令或验证串污染 |
| composite view | 很高 | 写回原目录、漏文件、symlink escape |
| dirty/non-Git snapshot | 很高 | 密钥复制、磁盘爆炸、不可复现 |
| claims/quarantine | 很高 | 双 writer、永久锁、僵尸副作用 |
| cross-repo integration | 很高 | 半完成更新、错误回滚 |
| remote Runner/multi-tenant | 极高 | 凭据、设备身份、重放、越权 |

内部回退门：`max_active_per_person=1`、`direct_only`、显式 components、dirty/non-Git
排队、只产 ChangeSet、旧客户端仅 person summary。这些是 rollout controls，不承诺为长期
公共配置。

证据顺序：deterministic correctness → topology/view shadow → isolated single run → clean Git
灰度 → mixed/dirty 灰度 → auto apply 最后。单元/replay 通过不等于可发布，必须有真实 soak。

---

## 14. 验收矩阵

### 14.1 身份与事件

- 两个 CLI transcript 不串；B 的“继续”不 steer A；
- `/stop` 默认不停止另 session；attach 不移动 final delivery；
- approval 按 notify policy 升级；老客户端不收并发 raw stream。

### 14.2 Workspace 与 Baseline

- queue 后仍使用接受时 workspace/topology/base/context；
- root 非 Git、多个 child repos 完整发现；root manifest 不遮 child repos；
- monorepo 不错误拆锁；submodule 父子状态冻结；
- topology cap/inaccessible/symlink 显式 fail closed。

### 14.3 View 与文件安全

- 同 repo 两 worktrees 并行不覆盖；aggregate composite views 保持相对布局；
- dirty/non-Git snapshot 可复现；isolated 后原 workspace checksum 不变；
- attachments/credentials/越界 symlink 不进入 writable view；
- materialization/GC crash 后无静默复用。

### 14.4 Resource 与取消

- 多资源 acquisition 无 partial deadlock；waiting_resource 可见、可 stop；
- isolated quarantine 不影响其它 views；direct holder 未死不转让；
- deploy/db claims 不因 view 可丢弃而释放；所有工具真实停止。

### 14.5 Task、Context 与交付

- 同 task 多 runs 不覆盖 active/summary/handoff；
- sibling 中间内容不进当前 prompt；component instructions/memory 不串；
- verification cwd 与 touched projects 正确；
- multi-repo ChangeSet 完整，integration conflict 无未说明半完成状态。

### 14.6 持久化与性能

- released control.db fixture 可升级、备份、恢复并拒绝新 schema；
- restart 后 queue/baseline/view/claim/ChangeSet 一致；
- `-race` 覆盖同 person/同 workspace/不同 views；
- 双-lane deterministic harness 证明 stream/queue/steer/approval；
- `max_active=2` 数小时真实 coding soak；
- DB wait、view disk、429、每成功 run 成本和 cache telemetry 有可信基线。

---

## 15. 跨代码库硬不变量

能力实际落地时再同步进入 `AGENTS.md`；此前本文是设计归属，不能把未实现行为写成事实。

1. 每 session 同时最多一个 foreground run；person/tenant 不做串行。
2. task 永不作为并发锁或上下文边界。
3. run 的 workspace、topology、baseline、context、delivery 在 admission 时冻结。
4. workspace 不是 repo；component 不是 project；project 不是资源锁。
5. write run 必须有 view；同一物理 view 同时最多一个 writer。
6. 同 logical workspace 多 runs 通过独立 views 并行。
7. direct holder 未退出不转让；quarantined view 不复用、不删除。
8. read-only 来自封闭能力；unknown/deferred escalation 按 write，运行中不升级锁。
9. isolated view 不写原 workspace；物理 root 由执行节点解析。
10. component instructions、memory、verification 只在本作用域生效。
11. raw events 携带 session/run/audience，只投 owner/authorized observers。
12. steer/stop/approve 解析唯一 run；多候选时询问，不猜。
13. 跨 repo 交付用 versioned ChangeSet；集成串行、校验基线、失败可见。
14. 文件 view 与 deploy/db/network 分别声明；worktree 不是完整隔离证明。
15. Gateway 管调度权限，执行实现/Runner 管物理 view、进程和凭据；一个代码库、一个
    `selfmind` binary。

---

## 16. 文档与计划治理

- 本文保持 `decision / paused`，不与当前 active plan 竞争。
- P0 的单 run 正确性缺陷可由维护者明确并入当前 active plan；P1 以后必须有 owner verdict
  和新的 active implementation plan。
- durable schema 变更遵守版本迁移、旧库备份、released fixture 和 newer-schema reject。
- 用户可见 message-path 变化添加 production-path eval；调度、topology、snapshot、迁移
  和 crash mechanics 使用 Go/race/fixture 测试。
- capability 边界变化时更新 `docs/STATUS.md`；机制变化更新本文及 owning domain doc；
  `docs/README.md` 只通过 `selfmind docs index` 生成。
- 本文不记录逐日实施日志。长期证据与 rollout verdict 进入获批的 active plan。
