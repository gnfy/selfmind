# 执行引擎设计与实施方案（Execution Engine）

> **文档状态：已实施（步骤 0a–10 全部落地）。** 本文档描述本地执行引擎的机制
> 与实施顺序；实现与文档同步。
> 活跃优先级仍以 `docs/STATUS.md` 为唯一事实源；安全边界的规则归属
> `docs/tool-safety.md`。本文档不引入远程协议，也不新增独立 Runner。
>
> 语言：本文档为 zh-CN 唯一版本（同 `docs/context-lifecycle.zh-CN.md`、
> `docs/memory-governance.zh-CN.md` 的惯例）。
>
> 证据基线：2026-07-28 的真实运行数据与 6 个可复现实验，见 §2 与附录 A。

## 1. 目标与范围

本轮**只修当前个人版的执行引擎**。不拆独立 Runner，不引入远程协议。修完后的
执行引擎可以整体搬进未来的 Runner（§12）。

核心目标：

1. 同一 run 使用稳定环境。
2. gcloud、GKE、Agent CLI 首次执行尽量成功。
3. 临时文件与必要状态可跨命令延续。
4. 不修改宿主机凭据。
5. 密钥不写入数据库、事件、进程 argv、日志或模型上下文。
6. 沙箱失败可分类处理，不盲目切 host，不重复试同一条命令。

非目标：设备身份/mTLS、短期能力令牌协议、Runner 注册与心跳、`waiting_runner`
状态、artifact 跨节点传输、多租户隔离后端（container/gVisor/microVM）。

## 2. 证据基线（2026-07-28）

### 2.1 运行数据（来源：`~/.selfmind/data/control.db` 的 `task_events` 与 `tool_ledger`）

| 项 | 数值 |
|---|---|
exec 调用（terminal / verify / execute_code） | 293（isolated 207，host 83，无沙箱事件 3） |
失败 | 43（isolated 28，host 12，其他 3） |
输出含 `read-only file system` | 76 行 / 26 个调用 |
输出含 `configuration directory may not be writable` | 42 行 / 24 个调用 |
输出含 `unable to create private file` | 32 行 / 18 个调用 |
被拒写路径集中于 | `~/.config/gcloud/credentials.db`（64 行）、`~/.config/gcloud/logs`（42 行）、`~/.cache/helm/repository` |
host 模式调用按小时 | 12点 3 → 15点 4 → 16点 7 → 17点 3 → **18点 39 → 19点 27** |
审批 | requested 12 / approved 12 |
当日新增 person 级永久 host 执行授权 | **10 条**：`set`、`git`、`aws`、`python3`、`kubectl`、`gh`、`for`、`argocd`、`gcloud`、`find` |
引用 `/tmp` 的 exec 调用 | 29（其中 isolated 20） |

两条最关键的观测：

**（1）耐久 watch 被同一缺陷静默打断。** 19:57 创建的 `watch_external`
（`gcloud builds describe` × 6）`status=failed`，`last_output` 全为
`CHECK_ERROR`；而同一 workspace 下 `aws codebuild batch-get-builds` 的 watch
全部 `succeeded`。差异原因：aws CLI 只读 `~/.aws` 不写盘，gcloud 必须写
`credentials.db`。`RunSandboxedShell`（`internal/tools/exec_sandbox.go:288`）
没有 host 逃逸口，模型无法自救。

**（2）错误提示把唯一正确方向堵死。** 沙箱拒写的真实错误文本是：

```
error_class: auth; hint: Authentication or credentials failed; fix auth state
first instead of repeating the same call. Host execution is not an
authentication or setup fix.
```

共 14 个调用收到这个误导。原因见 §3 的 R4。

**（3）跨调用 `/tmp` 断链的实例**（写在前一调用、后一调用读取失败）：
`/tmp/cwtest-yw-test-isolation-kubeconfig`（→ `connection refused
localhost:8080`）、`/tmp/cwops-jake.hu.initial-password`（→ `password file
missing`）、`/tmp/cwai-jake.hu.initial-password`、`/tmp/cw3-ecr-cloudtrail.json`、
`/tmp/jake-hu-multi-sre-result.json`。

### 2.2 实验结论（可复现命令见附录 A）

| 编号 | 结论 |
|---|---|
E1 | `~/.config/gcloud` 共 **231 MB**，其中 `logs/` 占 231 MB，真正需要的其余部分约 60 KB。**全量复制不可行。** |
E2 | 最小选择性复制 **49,779 字节** + `CLOUDSDK_CONFIG` 重定向后：`gcloud auth list` ✅、`gcloud auth print-access-token` ✅、GKE `kubectl get ns` ✅ **首次成功**；宿主 `~/.config/gcloud` checksum **UNCHANGED**。 |
E3 | `--bind <lease>/tmp /tmp` 替代 `--tmpfs /tmp` 后，命令 B 能读到命令 A 写入的文件；目录 0700 不影响 gcloud/kubectl。 |
E4 | 同一 source 绑两次（真实路径 + `/tmp`）后 inode 一致（336547），`$SELFMIND_RUN_TMP` 在两种模式下是字面同一路径。**但 lease 目录若位于 `/tmp` 之下，`/tmp` 绑定会遮蔽真实路径**（实测报 `Directory nonexistent`）。 |
E5 | 软链到真实状态目录**无效**（仍报 read-only，软链穿透到 ro-bind）；rw-bind 真实目录有效；`bwrap 0.9.0` **没有** `--overlay-src`/`--tmp-overlay`（`Unknown option`），overlay 方案排除。 |
E6 | 只读 `GOCACHE` 下 `touch` 被拒，但 `go build`（含 `-gcflags=-N` 强制 cache miss）**exit=0**。Go 降级但不失败，gcloud 硬失败——两类状态目录风险不同。 |

### 2.3 环境漂移的证据

daemon（PID 38583，19:50 启动）继承了拉起它的 shell 的环境与 cwd：

- PATH 含 `/run/user/1000/fnm_multishells/1473_1785211899779/bin`，而宿主机此刻
  并存 **9 个** `fnm_multishells/<pid>_<ts>` 目录（该 shell 退出后路径失效）；
- daemon cwd = `/mnt/d/wwwroot/workspace/cw-sre-base-prod/tools/aws`。

## 3. 根因

| 编号 | 根因 | 代码位置 |
|---|---|---|
**R1** | 沙箱可写视图取自 `cwd` 而非 `ExecutionScope`。后果：`AllowedRoots` 其余 root 不可写；cwd 为子目录时 workspace 根下 `.tmp/` 只读；`$HOME` 下所有工具状态目录一律只读，而 gcloud/helm/kubectl-auth-plugin **即使只做读操作也必须写状态目录**。 | `builtin.go:476` → `exec_sandbox.go:259-265` |
**R2** | `/tmp` 每次调用新建 tmpfs，且 host 模式看到真实 `/tmp`。没有 run 级 scratch，`TMPDIR` 未重定向。 | `sandbox/sandbox.go:105` |
**R3** | 子进程环境是 daemon `os.Environ()` 的实时快照：没有声明式策略、没有 per-workspace 声明、含会话级易失路径、cwd 漂移；lease 只是事后记录，并未控制执行。 | `process_env.go:55` |
**R4** | 错误分类无沙箱上下文：`auth` 规则含裸 `credential` 子串且排在 `permission` 之前，`credentials.db` 因此被判成 auth；isolated 分支对 auth 追加"host 不是修复手段"。 | `tool_errors.go:39-58`、`215-228` |
**R5** | 逃逸口只有 all-or-nothing 的 `sandbox=host`；可复用授权键取脚本首个 token，因此 `set -euo pipefail` 落成 `command:set`。 | `middleware.go:598-613` |
**R6** | 授权账本只有写入与存在性检查，没有列出、撤销与期限，用户看不到落了什么。 | `control/approval_grants.go:44`（INSERT）、`:64`（SELECT 1） |

## 4. 设计原则：三层分工

对齐 codex 的分层（源码实测）：

| 层 | 内容 | codex 的位置 | 本方案的位置 |
|---|---|---|---|
**L1 内置行为数据** | 安全命令清单、禁止授权前缀、平台默认只读根、core 环境变量白名单、沙箱拒绝关键词、受保护元数据名 | Rust 源码常量：`exec_policy.rs:53` `BANNED_PREFIX_SUGGESTIONS`（约 100 条）、`bwrap.rs:44` `LINUX_PLATFORM_DEFAULT_READ_ROOTS`、`shell_environment.rs` `UNIX_CORE_ENV_VARS`、`denial.rs:14` `SANDBOX_DENIED_KEYWORDS` | **Go 源码数据表**：已有 `middleware.go:636` `hardlineProtectedRoots`、`:802` `dangerousBinaries`、`tool_errors.go:29` `errorClassRules`；本轮新增 `envprofiles.Catalog`、`bannedGrantPrograms` |
**L2 用户批准的规则** | 模型建议 → 用户确认 → 落成可读、可列出、可撤销的规则 | `~/.codex/rules/*.rules` 独立文件目录，**不在 config.toml** | `execution_capability_grants`（完整）与 `approval_grants`（**本轮补齐管理面**，§11） |
**L3 用户配置** | 少量策略开关 | config.toml 三个键：`sandbox_mode`、`sandbox_workspace_write`、`shell_environment_policy` | `config.yaml` 的 `exec_sandbox` 三个键：`enabled` / `required` / `allow_network`。**本轮零新增键** |

### 跨层规则（硬约束）

1. **L1 是 L2 的下限。** L1 的 `bannedGrantPrograms` 与"复杂 shell 结构性排除"
   共同决定哪些命令**永远不能**产生可复用授权，L2 无法突破。
   （codex 用两种手段实现同一件事：禁止模型建议这些前缀 + 含重定向/替换/
   变量赋值/通配的命令结构上不参与规则匹配。）
2. **L1 不可配置。** profile catalog、禁止授权表、拒绝关键词都是行为定义，
   随二进制发版、经 PR 评审、由构建期测试校验，不进 `config.yaml`。
3. **L2 必须可见可撤销且有期限。** 用户批准的东西，用户必须能看到、能列出、
   能撤销；host-escape 类授权不得永久。
4. **L3 键数不增长。** 由测试锁定（§13）。

理由：`config.yaml` 已有 20 个顶层键（5.3 KB）；把 profile 目录塞进去会让
"配置"与"行为"混淆，并引入插值小语言与运行时解析失败面。仓库已有 Go 数据表
先例（四张），也有 `go:embed` 先例（`skill_catalog.go:16`，但那是**用户资产**，
性质不同）。

## 5. 目标结构

### 5.1 执行链

```
ExecutionRequest
  → EnvironmentResolver     （按 lease 取 snapshot）
  → ToolEnvironmentProfile  （catalog 匹配 + 叠加）
  → SandboxPlanner          （→ SandboxPlan + ProcessMaterial）
  → SandboxBackend          （bwrap | seatbelt | host）
  → ProcessRunner
  → FailureClassifier
  → RecoveryPolicy          （至多一次）
  → ExecutionResult
```

### 5.1.1 后端强度不等价（macOS seatbelt）

平台由 `IsolationBackendForPlatform` 选择：Linux 用 bubblewrap，macOS 用
`/usr/bin/sandbox-exec`，其余平台没有隔离后端、退回审批受控的 host 执行。三处
判断（有效模式、`ContainmentAssessment.Enforced`、host-escape 归类）都问后端的
`Available()`，不再各自写 `GOOS == "linux"`。

两个后端**实施同一份 `sandbox.Policy`，但强度不同**。seatbelt 是权限过滤器，
不是命名空间，因此有三条必须记录的差异：

| 契约 | bubblewrap | seatbelt |
| --- | --- | --- |
| 工作区外只读 | `--ro-bind / /` | `(allow file-read*)` + 默认拒写 |
| 写限定在声明根 | `--bind-try` 每根 | `(allow file-write* (subpath (param …)))` 每根 |
| 关闭出网 | `--unshare-net` | `(deny default)` 下不发放 network 规则 |
| PID / IPC / UTS 命名空间 | 有 | **无** |
| `OverlayMounts` / `SynthesizedDirs` | 有：私有、带种子的覆盖层，宿主不被写 | **无**：改为声明子路径原地可写（`HostStateWrites`，§9.4），表达不了的计划被拒绝 |
| `$SELFMIND_RUN_TMP` 与 `/tmp` | 同一目录（双重绑定） | **不同**；`/tmp` 在可写根之外 |

后两条有具体后果：

- 需要挂载的状态（catalog 里只有 `aws` 的 SSO token cache 与 STS cache 用
  `MapRWAt`，其余 profile 靠环境变量重定向）在 macOS 上不再被整体拒绝，而是按
  所有者决定 D3 翻译为宿主真实路径上的逐子路径写授权（§9.4）。这**弱于**
  Linux：被写的是宿主真实缓存。准备阶段向后端询问能否挂载（`MountsToolState`），
  不判断平台名。子路径表达不了的需求（解析到声明根之外、不是目录、会覆盖只读
  配置、没有声明根）仍被拒绝并说明具体原因，绝不丢弃需求或改到 host 执行。
- `TMPDIR` 已指向 run scratch（`executionenv/scratch.go`），所以 `mktemp` 一类仍在
  可写视图内；依赖字面 `/tmp` 的命令在 macOS 沙箱里会被拒写。

### 5.1.2 边界在审批前确定，执行固定于已批准的边界（2026-10-07）

执行边界在任何能力询问（`credential:read`、`network:shared`）、smart 判断模型
和人工审批之前确定一次（`annotateEffectiveSandboxMode`），所有审批读取它，执行
固定于它。确定时不只看平台有没有沙箱，还看后端能否兑现**这次调用的计划**：

- 后端兑现不了时，`auto` 调用以 host 执行呈现，判断模型与人看到的是 host
  边界及原因；显式 `isolated`、操作者的 `exec_sandbox.required` 与受管 Git 执行
  视图所需的隔离则在审批前以类型化的 `not_dispatched` 失败拒绝，不为不会被执行的
  沙箱边界请求任何审批。
- 并行容量不增加隔离或人工审批要求。host 调用按冻结的当前模式决策：smart
  每次使用廉价角色判断，不复用 host 决定；full-auto 经过硬底线后执行；on-request
  沿用覆盖当前身份、目标与环境的精确审批或 standing policy。外部效果占用检查
  在执行通道之前完成，未知效果仍需观察。实际 host 命令独占本人在该 daemon
  Runtime 中的执行通道；其他 terminal、文件写与 patch 等待，模型调用继续。
  长等待只有保存精确未派发调用的 checkpoint 并原子落下续接队列后才能释放
  worker。排队的 host 保留先后顺序，等待审批、澄清或另一外部效果时让出通道
  意图；续接仍重新通过权限与效果检查。显式 host 后台命令仅启动时独占，并提示
  启动后进程仍可能影响其他 Run；普通命令保持前台执行。
- 可行性检查假定待定的 `credential:read` 询问会被批准；凭据被扣留时需要的
  原地写是其子集，因此结论不会因拒绝授权而变坏。
- 在原地写入凭据状态的后端上，containment 摘要附带
  `host_credential_writes=<路径>;<路径>`，判断模型与人看到同一边界；路径集合变化
  会改变审批指纹，需重新询问。可复用的批准键——记住的命令类、提供的规则、阶段
  批准的声明效果——都绑定写入集合的摘要（`|host_writes=…`），精确 run 键与判断
  决定键以及停放审批的续接授权经 containment 摘要同样绑定：在不需要原地写时
  记住的类或规则，配置改为需要写入后不再放行该命令；以同一写入集合记住的则继续
  复用。启动审查保留类本身仍过底线的绑定键，`/approvals grants` 注明它只在写
  同样的缓存时有效。
- 阶段批准（`request_permissions`）为每条声明命令生成的键，在生成时即绑定该
  命令获得凭据访问后会做的原地写入，命令标签写明这些路径，询问理由以它们开头；
  写入出现之前批准的阶段包不会放行它们。不受信任工作区里，正是 `credential:read`
  询问开启了这些写入，而它放行的受控安全观测之后不再询问第二次；因此该询问同样
  以写入事实开头，它留下的 run 授权也绑定写入集合。
- 各界面都会截断审批理由（IM 120 字符，终端面板两行），因此理由以边界事实开头：
  `writes credential caches in place: ~/.aws/sso/cache, ~/.aws/cli/cache` 或
  `runs on the host: <原因>`，截断不会丢掉它。external-effect 声明边界自己的
  一次性询问（针对另一个 Run 的"遗留不确定"效应）放行的是同一次确切调用，因此
  同样以该事实开头并携带 containment 摘要，停放的批准所留下的授权通过该摘要绑定
  写入集合。在进程启动前就被拒绝的后台启动会报告"未启动"，其占用的声明随之释放，
  而不是被留成不确定。

执行使用已批准的边界（`ExecutionRequest.Boundary`）。批准——人、判断模型、
已存或 run 内授权、停放审批续接时的一次性能力——假定的是隔离，而派发时后端
兑现不了（配置变化、状态路径被换到根外）时，调用不派发，模型收到类型化的
`sandbox_boundary_unavailable` 或 `sandbox_plan_unsupported`，并明说没有在 host
上运行。容量一同样如此；durable watcher 轮询没有轮询时的审批，同样不降级。
以 host 批准的边界在 host 执行。可选隔离在初始可行性检查中因沙箱关闭、
无可用后端或后端无法兑现这次计划而解析为 host，发生在审批之前；已批准的
隔离边界不会在派发时改成 host。

已批准的边界包含它的原地凭据写入：派发准备时，若计划的写入不是放行这次调用的
决定所覆盖集合的子集（宿主配置在审批与派发之间改变了），在创建任何目录之前即
拒绝（`approved_host_writes_changed`，`not_dispatched`）。durable watcher 轮询
没有自己的放行决定，因此受注册时审批所覆盖的写入集合约束：注册把它记录在
watch 的执行绑定上（`host_state_writes`，位于既有绑定 JSON 中，无 schema 变更）。
计划写入更多的轮询同样在创建任何目录之前被拒绝，watch 以环境失败停放；没有
记录写入的绑定（包括绑定出现之前的旧行）不覆盖任何写入。后台启动按约定就是
host 执行，遵循同一规则：要求隔离的策略拒绝它。watcher 只有在其无人值守的轮询
（按注册时冻结在 watch 上的策略解析 `auto`，该策略只会被 daemon 当前策略收紧、
绝不放宽，轮询没有自己的审批）将运行于注册所批准的同一边界时才注册；否则什么都
不运行并拒绝注册（`watch_boundary_mismatch`），不会出现注册成功却永远无法轮询、
或在批准之外轮询的 watcher。这两项约束互不依赖对方的输入保持不变：注册之后关闭
沙箱不会把轮询移到 host（host 上没有可用来约束写入集合的原地计划）；更严格的配置
把在 host 上注册的 watch 收紧进隔离时，也不会授予它原地写入——它的注册没有覆盖
任何写入，这样的轮询被拒绝，直到重新注册。

这是有意的行为变化：此前 `exec_sandbox.go` 在后端拒绝计划时把 `auto` 调用静默
改为 host 执行。2026-10-06 的安全复核（WP-F2，高危）复现：macOS、默认
`exec_sandbox`（启用、非 required）、smart 模式、未受信工作区且宿主有 AWS SSO
配置时，`python3 tool.py` 被当作 contained 直接放行，seatbelt 拒绝挂载式 AWS
状态后在宿主执行。现在它在 seatbelt 中运行（凭据被扣留：宿主缓存保持只读），
任何路径都不会再静默落到宿主。

路径以 `sandbox-exec -D` 参数传入，不插值进 policy 文本，因此路径无法终止字符串
字面量并注入规则；非绝对或未规范化的路径一律拒绝渲染（fail closed），调用不派发，
而不是丢掉一个可写根后产生难以定位的权限失败，也不改到 host 执行。原地写的状态
子路径（`STATE_WRITE_n`）同样以参数传入，且不能是根目录 `/`。

2026-10-05 兼容修复：解释器版本查询和明确的非执行语法检查只收集自身依赖，不自动引入未知脚本可能调用的全部工具；这项优化不改变只读证明或审批权限。前置 `VAR=value` 赋值只改变程序看到的环境、不改变运行哪个程序，`TZ=UTC date`、`CGO_ENABLED=0 go build` 与不带赋值的命令准备完全一致；程序按 shell 自身的解析、经与安全底线相同的识别得出（无法解析时才退回按字段切分），值含路径或空格（`KUBECONFIG=/x kubectl`、`FOO="a b" aws`）也不会挪动程序词；快照无法解析的内联选择变量（`AWS_PROFILE=x aws …`、`env -u AWS_PROFILE …`，`-c` 脚本内同样）作为配置覆盖保留该工具的保守状态要求，但不引入主机库存。环境 profile 的挂载状态可由有界配置条件决定，明确的参数和快照变量选择配置段。未知脚本、动态选择、配置覆盖或无法可靠读取的配置保留保守要求。静态认证继续使用只读配置；确实需要写缓存的认证仍要求挂载，macOS 无法兑现时明确拒绝，不静默丢弃、不增加 host 逃逸。准备缓存纳入最终挂载要求。模型收到的是当前 Run 冻结的执行策略；显式隔离或操作者 required 的拒绝不能解释为另一个任务占了槽位。

### 5.1.3 受管 Git 视图的 daemon 侧 Git 不进入执行链

`/views`、`/apply`、视图归档与清理由 daemon 在宿主上直接运行 Git，不经过上面的
沙箱后端；而视图及其 `.git` 对所属 Run 可写。因此视图里可写的状态不得决定宿主
Git 执行什么（实现见 `internal/executionenv/git_view*.go`）：

- 每条视图命令都把 `GIT_DIR`、`GIT_COMMON_DIR` 和工作树固定到该视图，并在命令行
  关闭 `core.fsmonitor`、钩子和 untracked cache。daemon 侧的每条 Git 命令（无论对
  视图还是源仓库）都不读 commit-graph、multi-pack-index 和可达性 bitmap，这是纵深
  防御：凡可能导致丢失工作的判断都不依赖 Git 读取它们，要么由 daemon 对所依赖的提交
  和树计算哈希，要么只读视图自身的对象目录（见下文）。只有视图的有效性检查
  （`merge-base`，误判通过只会让校验遍历列出更多对象）和一个仅供展示的计数还会读取
  它们。`.git` 不是普通目录，或含 `commondir`、`gitdir`、`config.worktree`、
  `info/grafts`、`shallow`，或 alternates 不等于准入时的源对象库时，视图不可用但
  保留文件。
- `.git/config` 只能含创建时写入的被动键；include 和一切可指定程序或改写 Git
  去向的键都使视图不可用。
- 读取检出与索引（status、被忽略和被隐藏的条目）使用 daemon 写入的临时 Git
  目录，只带校验过的被动键，因此校验之后改写 `.git/config` 也定义不出过滤器；
  status 不进入子模块，diff 带 `--no-ext-diff --no-textconv`；仍有 Run 可能写入
  视图时，`/views` 只读已提交内容，`/apply` 拒绝。
- status 按 Git 读到的样子比较索引与 HEAD 的树，不计算哈希；视图自身不存的树（视图
  里的 gc 会去掉源仓库已有的对象）来自源仓库。因此 status 判为干净的索引还必须恰好
  记录 HEAD 的树：对索引副本运行 `write-tree --missing-ok`（不读任何树对象），结果
  必须等于 daemon 读出并校验哈希的 HEAD 提交中的树，否则检查失败；源仓库写入者植入
  的树因此无法对归档和清理隐藏已暂存的改动。大小仍相符、或索引条目没有 stat 数据
  （`read-tree` 之后，或与该链接同一秒写入）的符号链接，Git 也以同样方式读取其 blob
  来判定。因此 daemon 从检出中读出每个符号链接的目标，按 blob 计算哈希，与索引条目
  的 id 不同的就算作未提交的工作，不从任何一个对象库读取 blob。普通文件不需要这项检查：
  Git 直接对文件本身计算哈希，`text=auto` 下它为该文件读取的唯一 blob 只决定是否规范化
  CRLF 行尾。
- 清理证明只看视图自身存储的对象（松散与打包），要求它们都能在不计 reflog 时
  从已交付 HEAD 到达；经 alternates 借用的源仓库对象不属于视图。这次遍历只读视图
  自身的对象：daemon 建一个只链接视图松散对象目录与 pack 目录、没有 alternates 和
  其他 info 文件的对象目录。经 alternates，Git 还会读到源仓库，写入者可以在那里植入
  一个父提交通向已交付 HEAD 丢下的工作的提交。遍历遇到视图自身未存的提交（视图 gc
  之后）就在那里停止，这只会让归档被保留。视图自身的对象库属于它的 Run：在那里伪造
  对象的 Run 能对这项检查隐藏它自己不可达的工作，就像它也能直接删掉那些工作一样。

源仓库的 `.git` 对首个直接写入的 Run 可写，且 daemon 侧 Git 工作期间也可能被改。
确切的保证是：对源仓库的 daemon 侧 Git 不从源配置读取程序、hook 或传输，即使配置
在筛查之后被改。源命令分四类：

- 身份探测（`--show-toplevel`、`--git-common-dir`）按本人 Git 的方式解析，用来发现
  拓扑变化；基线冻结只接受公共目录就是自身 `.git`、且其中没有 `commondir`/`gitdir`
  文件的检出。
- 配置、对象与索引读取（config、cat-file、ls-files）、交付的对象导入（index-pack）
  以及新建引用的源侧复核（rev-parse）都没有 hook 事件：固定 `GIT_DIR` 与
  `GIT_COMMON_DIR` 到准入的 `.git`，置空 `GIT_ALLOW_PROTOCOL`，设
  `GIT_NO_LAZY_FETCH`，忽略 replace 引用，在命令行覆盖钩子路径与 fsmonitor；
  每条命令先拒绝事后多出 `commondir`/`gitdir` 的源 `.git`。
- 引用的读取与创建改走 daemon 写的 Git 目录。固定目录只让配置和对象留在准入的
  `.git`，Git 仍会顺着其中的 `commondir` 文件解析引用，检查之后植入的文件会让固定
  目录的引用读取答出另一个仓库的结果，例如把未交付说成已交付。daemon 目录没有
  `commondir`：它链接源的 `refs`、`packed-refs`、`reftable`，使用源对象目录，配置由
  daemon 写入，只重复按 Git 自己方式读取的源仓库格式（仓库自身的 config 文件，不跟随
  include）：`files` 或 `reftable` 引用存储；其他引用存储在写入任何东西之前拒绝，
  基线冻结暂不接受 SHA-256 仓库。那里完全不读源配置，所以 `update-ref` 不会运行配置式
  `hook.<name>` reference-transaction hook（`core.hooksPath=/dev/null` 挡不住它）。
  建引用（`/apply` 分支与清理保留引用）是对“引用不存在”的比较交换，已指向同一提交的
  引用视为先前相同的创建；不写 reflog。只有在该引用经 daemon 目录、也经按源自身配置的
  固定读取都解析到预期提交后才报告成功，写到源仓库 Git 看不到之处的引用因此是拒绝，
  而不是交付。清理还会在受保护的移动内、检出离开恢复路径之前再复核一次，失败则保留
  归档。
- 基线冻结的 status 经只带被动比较键的 daemon 目录读取，筛查后再加的 filter 或
  fsmonitor 不会运行。

基线冻结遵循同样的规则：两次身份探测之后，配置筛查、子模块检查和比较键读取
（config、ls-files）固定目录，HEAD 经 daemon 引用目录读取，status 经 daemon 写的
配置读取。`/apply` 不用传输：在视图内构建恰好包含 daemon 校验遍历（见下文）所列
对象的 pack，经两个 Git 进程之间的管道流入源对象库的 `index-pack --strict`，daemon
从不持有整个 pack；取消或任一侧失败都会
停止两侧，并各自报告自己的错误。基线冻结、建视图、交付与清理前按展开后的源配置
筛查，拒绝配置式 hook、alternate-refs 程序、pack 协议程序、重定向工作树和可执行
内容驱动，并拒绝通过 alternates/http-alternates 借用对象的源仓库；
`core.sshCommand`、`credential.*`、`url.*.insteadOf` 等开发者常见键不拦截，靠移除
传输化解。

引用不等于已交付的工作，对象文件存在也不等于：能写源仓库的 Run 可以把视图的提交和
树对象复制过去，在每个 blob 的路径放空文件或别的对象，再让分支指向该工作。只有源仓库
持有工作 HEAD 相对基线新增的每个对象、且内容哈希等于其 id，并且源仓库自己的 Git 仍能
解析该分支时，工作才算已交付、才可在清理时受保护。每个列出的对象都经 daemon 目录用
`cat-file --batch` 从源仓库读回并由 daemon 计算哈希（整个过程复用同一个缓冲区），
忽略 replace 引用，并在读取前后都拒绝经 alternates 借用的对象库。Git 读取时不计算
哈希，同一个对象也未必只从一个副本读取：当某个 pack（最新或最近用过的）和松散文件都
持有同一 id 时，Git 2.54 按 pack 副本报出 blob 的大小、却从松散副本输出其内容，而提交
和树则整个从 pack 副本读取。所以列出对象的每个副本都必须通过哈希：
与打包副本并存的松散文件、每个索引里列出该 id 的 pack 中的副本（经只含该 pack 的
对象目录读取），以及源仓库有 multi-pack-index 时它解析出的副本，因为本人的 Git 默认
经由该索引读取。否则，今天没被 Git 读到的替代对象会等到重新打包时才生效。`/views`、
归档与清理只在该证明成立时把分支算作已交付。

什么算工作、什么算已在基线中，由 daemon 自己运行的遍历决定。Git 读对象时不计算
哈希，所以 Run 写进自己视图的 pack，只要其索引把基线历史中某个提交的 id 指向别的
内容，就能让工作提交看起来属于那段历史；无论怎样配置，这里都不能信任 Git 自己的
遍历。daemon 经 `cat-file --batch` 读取视图或归档的每个提交和树，只接受哈希等于其
id 的内容；像 `rev-list` 限定范围那样，从工作 HEAD 和基线两侧按时间从新到旧遍历
提交，只有当一条经过校验的父提交链从基线通向某个提交时，才把它算作基线。顺序只影响
遍历读多远，从不排除工作。工作提交按父提交在前的顺序处理，每个工作提交贡献它在某路径
上持有、而它的任何父提交在同一路径都不持有的树和 blob；再次遇到的树不再重读，因为其下
的一切当时已列出，或由更早处理的父提交或基线持有。比 `rev-list --objects` 多列是安全
的，因为源仓库本来就必须持有这些对象；有测试在生成的历史（合并、重命名、模式与类型
变化、符号链接、gitlink、回退、移回、时钟偏差）上把遍历与 Git 自己的可达性比较，要求
遍历从不少列。每个树模式条目都按树读取，工作树中同一 id 既作为目录又作为文件出现时
拒绝。grafts、shallow 文件、replace 引用、commit-graph 以及 HEAD 之外的引用都不参与；
对象哈希不符的视图无法列出，归档、`/apply` 与清理因此拒绝它。一次遍历最多持有
100000 个尚未确认属于基线的提交，更多的工作按失败关闭处理。一个提交最多可列 1024 个
父提交，远超 Git 写出的任何 octopus 合并（重复列出的父提交只保留一份，存放在恰好与
所保留父提交个数相同的切片里）；一棵树最多位于其提交根树之下 2048 层目录，
即 Git 自己的默认 `core.maxTreeDepth`；遍历用显式栈读树。它不保留任何提交的文本，每个
id 只持有一份。必须保留的内容（读过的提交，以及列出、命名或引入过的 id）和正在遍历的树
按 Go 实际分配的大小计入 512 MiB 的预算，一棵树在构建前就计入；遍历完的树及其名称
索引在 64 MiB 内缓存，被丢弃后需要时重新读取并校验哈希。遍历自身的工作计入 2^28 步：
每读一个对象 256 步，外加每 16 字节 1 步（缓存丢弃后重读的树再次计入）；每解析一个树
条目 4 步；每个与父提交的树比较的条目，每个父提交 1 步，名称每 64 字节再加 1 步；已引入
或已列出的条目不再比较。遍历过程中也会检查自己的 context，截止或取消时按中断结束。
超过任一界限时，遍历拒绝并指明是哪个界限。遍历在应为提交或树之处遇到
blob，或遇到超过 64 MiB 的对象，都立即拒绝并放弃该次读取：Git 按一个副本报出 blob 的
大小，却可能输出另一个副本，等下去只会在截止时间结束，并被误读为中断。这里不信任视图自身的对象
库，包括建视图时复制进去的基线历史；视图里的 `git gc` 或重新打包都不改变结果。源仓库
的写入者只能经由“工作与源仓库共有、而视图从未自己存储”的对象影响清单，这些对象同样
经过校验。

在负载较高的机器上实测（macOS，Go 1.27）：遍历结束后存活堆从不超过计入量的 1.03 倍，
即每种情形都在八分之一以内；包含 Go 尚未释放的垃圾在内的峰值为计入量的 1.1 至 2.1 倍，
反复读取同样的 1 MiB 树时为 3.2 倍。花完 2^28 步消耗 daemon 4 秒到 40 秒的 CPU，视这些
步花在什么上而定：比较或读取 1 MiB 的树约 5 秒，带 1,024 个父提交行的提交 16 秒，重复
读取一百万棵很小的树 36 秒（墙钟 90 秒），单父提交 40 秒（由 99,000 个外推）。在 1,024 个
父提交下、根树为 64 MiB 且重复同一目录条目的 head 在 0.8 秒内完成，那些父提交都没有的
不同名称在 6 秒后被拒绝。各自都把同一个父提交列 1,024 次的 30,000 个提交的工作在 8 秒内
列出（计入 14 MiB，存活 11 MiB），100,000 个则在 21 秒（CPU 16 秒）后被拒绝；300 个父
提交的根树从 1,000 个目录指向 300 棵互不相同的 1 MiB 树，而 head 有 1,000 棵互不相同的
子树，6 秒后被拒绝；2,000 个共用 1,023 个父提交（200 KiB 的树）的提交 9 秒后被拒绝；
12,000 个共用 100 个父提交（很小的树）且不缓存任何树的提交 90 秒（CPU 36 秒）后被拒绝。
诚实的工作绰绰有余：在 8,000 个文件的目录里各改一个文件的 300 次提交用掉 7% 的步数、
计入 66 MiB（1.1 秒）；500 次提交共 152,000 个对象用掉 2%（0.7 秒）；为归类日期早于基线
的工作而读取 300,000 个提交的基线历史用掉 30%、计入 123 MiB（墙钟 55 秒，CPU 13 秒）。

`/apply` 与清理每次都先从视图或其归档导入再证明：pack-objects 恰好打包所列对象。
`index-pack --strict` 会把收到的每个对象与源仓库已有的松散副本比对，但 Git 2.54
不与打包副本比对，所以导入先按上面的方式检查源仓库已有的每个工作对象副本，发现替代
对象就拒绝，而不是用更新的 pack 把它遮住。重复的 `/apply` 以及交付之后的清理因此会
在源仓库留下重复的 pack，直到 Git 重新打包。已清理视图的归档已不在，同一校验遍历
改为读取源仓库自己的对象。证明、遍历、导入或引用检查超时、被取消时如实报告“未完成”，
而不当作缺少工作或引用已变；Git 命令失败时带上 Git 自己的信息。从基线可达的对象
属于源仓库自己的历史，不再重复证明。

`/apply` 与 `/views prune` 还会等待仍可能改动源 checkout 的对象并点名：正在准入或
运行、对该 checkout（或其上级目录）有可写绑定的本人 Run，可在那里续接的停放 Run，以及
这类 Run 启动后仍在运行的后台进程（或早先 daemon 记录在该 checkout 中的后台进程）。

剩余风险：

- 在 macOS 上 seatbelt 没有 PID 命名空间，未登记为后台进程、从沙箱前台命令逃逸的
  子进程可能活过其 Run（Linux 上 bwrap 的 PID 命名空间阻止这种情况）；上面的等待
  看不到它，它仍保有对该 checkout 的写权限。
- 对源 checkout 写入者的等待是交付或清理之前的检查，而不是与 Run 准入一同持有的锁
  （视图移动则是）：`/apply` 或清理在源仓库工作期间被准入的 Run、或被取出执行的排队
  请求不会被看到。
- 等待使用共享的路径比较，它尚不把大小写不敏感文件系统（macOS 默认）上只差字母大小写
  的同一目录拼写视为同一路径；以这种拼写可写绑定该 checkout 的 Run 不会被看到。修复
  属于该共享比较，随准入路径工作完成。

按上述规则，它们都无法让 daemon 侧 Git 运行程序，但都能在 `/apply` 或清理期间改动
checkout 的引用与对象；比较交换建引用、`--strict` 导入、对每个新建引用的复核以及
源仓库持有该工作的证明会把这种干扰变成被拒绝的操作，而不是错误的分支或被删除的
归档；证明之后才从源仓库删除的对象不在其范围内。普通读取的保证依赖这些命令在已验证的
Git 版本（2.54）中没有由配置驱动的程序；若 Git 新版本加入，需要重新审查。

`tool-safety.md` 的 Managed Git views 一节给出完整契约，包括归档/清理与 Run 准入
的原子性。

### 5.1.4 daemon 私有状态对受限命令不可见

工具与 daemon 以同一个操作系统用户运行，文件权限位挡不住受限命令。`(allow file-read*)`
和共享网络曾让沙箱内的 `cat` 读出 daemon 的 token，并让 `curl` 直连 daemon 的控制
监听。因此 `sandbox.Policy` 增加 `Private`，由网关 runner 按数据目录布局和自己的
监听地址一次性派生并安装（`tools.SetDaemonPrivateState`），每个隔离后端在命令自身的
可写根与网络模式**之后**应用，二者都无法重新打开它：

- 运行时目录（状态、锁、pid、日志、控制 socket）与 `backups` 目录；控制 socket
  不在运行时目录内时（数据目录路径过长，socket 落到 `/tmp/selfmind-<uid>`），其所在
  目录也整体私有——该目录由同一用户的所有 daemon 共用，受限命令连其他 daemon 的
  socket 也够不着；
- 控制库及其 `-wal`、`-shm`、`-journal`；
- daemon 实际加载的配置文件（`--config`、`SELF_CONFIG` 或默认路径；含全部 provider
  key 与聊天 token），连同写在它旁边的副本：保存时的 `config.yaml.tmp`、编辑后的
  `config.yaml.bak-<时间>`，包括 daemon 启动之后才出现的；
- 凭证文件（`auth.credentials_file`）及其存储在旁边写的临时文件，以及聊天账号凭证
  （`~/.selfmind/weixin/accounts`）；
- 控制 socket 路径与 daemon 的 TCP 端口（绑定到 loopback 之外时，该端口对所有
  地址一并拒绝）。

私有名字只在它仍指向状态时才起保护作用。seatbelt 按路径匹配，能写数据目录父目录
的命令曾可以把目录挪走（`mv ~/.selfmind ~/.x`、rename 交换，或先挪走再在原名处放
符号链接），再通过没有任何规则提到的新名字读取或连接其中的一切，或者让 daemon 在它
使用的路径上什么也找不到。因此，凡位于命令某个可写根**内部**、且处于私有路径上方的
目录，都被钉住，不能被改名、交换或删除；私有 socket 本身不能被改名或删除。可写根
本身早已被钉住，根之上的目录本就不可写。同一目录里的其他读写不受影响。

| 契约 | bubblewrap | seatbelt |
| --- | --- | --- |
| 目录不可读写 | `--tmpfs <dir>` 覆盖 | `(deny file-read* file-write* (subpath …))`，置于所有 allow 之后 |
| 文件不可读写 | `--ro-bind /dev/null <file>` | `(deny file-read* file-write* (literal …))` |
| 同前缀文件不可读写（配置及其副本） | 命令启动时对已存在的同前缀文件逐个 `--ro-bind /dev/null`（目录用 tmpfs）；其后才出现的副本从下一条命令起生效 | `(deny file-read* file-write* (prefix …))`，其后才出现的副本同样被拒 |
| 不可连接控制 socket | 隐藏其所在目录 | `(deny network-outbound (literal …))`，加 `(deny file-write-unlink (literal …))`，socket 不能被改名或删除 |
| 私有目录内的任何 socket 不可连接 | 目录被隐藏 | `(deny network-outbound (subpath …))`（连接 socket 只检查网络规则，不检查文件规则） |
| 私有路径上方的目录不可改名、交换、删除 | 把每个这样的目录 `--bind-try` 绑定到自身使其成为挂载点（rename、rmdir 得 EBUSY，内容读写不变），排在可写根之后、overlay 与掩码之前；`/tmp` 之下、合成目录或 overlay 路径之内的不绑 | 对每个位于可写根内部的祖先目录 `(deny file-write-unlink (literal …))`；rename、交换、覆盖都检查被替换条目的 unlink |
| 不可连接 daemon TCP 端口 | **无法做到**：共享网络时沿用宿主网络命名空间 | `(deny network-outbound (remote ip "localhost:<port>"))`，非 loopback 绑定用 `*:<port>` |
| `/tmp` 之下的路径 | 不生成挂载（`/tmp` 是 run scratch 或私有 tmpfs，挂载点会写进 scratch） | 照常拒绝 |
| 路径不存在 | 丢弃（只读根下无法创建挂载点，否则整个命令中止） | 照常拒绝（literal 规则对不存在的路径无害） |

后果与取舍：

- seatbelt 规则中的路径同样以 `-D` 参数传入；非绝对、未规范化的条目或越界端口使
  整个 policy 拒绝渲染（fail closed），退回审批受控的 host 执行，而不是悄悄丢掉一条
  私有规则。路径先解析符号链接再进入 policy，因为 seatbelt 匹配真实路径。
- 硬链接：seatbelt 下，为任何带拒绝规则的路径创建硬链接，都由沙箱自身拒绝（在
  macOS 27 上观察到，并有测试固定），不是 SelfMind 写的规则；因此私有文件无法被
  硬链接到别处。
- Linux 上共享网络的沙箱命令仍能连到 daemon 的 TCP 监听。它得不到 CLI 身份（TCP 从不
  承载 CLI 身份）、够不着控制 socket（已被隐藏），在未设置 `gateway.token` 时也无法
  停止 daemon 或读取超出存活信息的状态，但仍可冒用它能说出的非 CLI 身份。缓解方式是
  设置 `gateway.token` 或 `exec_sandbox.allow_network: false`；bubblewrap 的网络模型
  无法按端口拒绝。挂载点与掩码的行为只有 argv 单元测试，没有在真实 bubblewrap
  上运行过。
- 数据目录里不属于上述私有条目的部分（每人记忆库、`cron.db`、附件、工具输出、prompt、
  skill）不是私有的：它们是受信任的家目录工作区可以读写的用户内容。同一用户的第二个
  daemon 若把 socket 放在自己的数据目录里，共享网络时也能被连到（`/tmp` 回退目录是
  私有的）；每个用户只运行一个 daemon，或设置 `exec_sandbox.allow_network: false`。
- 这条规则**只约束隔离执行**。host 模式本来就不受约束（审批门控的逃逸口），
  它对控制 socket 的访问由 daemon 对对端的验证（见 `docs/tool-safety.md`）负责。
- 与 §5.1.2 的原地写凭据缓存（`host_state_writes`，policy 里的
  `StateWritePaths`/`FlatStatePaths`）组合：seatbelt 按顺序渲染，可写根、原地写子路径
  的允许规则及其保护规则（目录不可删除、只许建普通文件、扁平缓存不许建目录、canary、
  运行时记录目录只读）都在前，私有状态的拒绝规则**最后**，所以缓存允许不会重新打开
  任何私有名字，私有规则也不影响缓存写入。原地写子路径本身就是可写子路径，因此
  `protectedAncestors` 与 `protectedSymlinks` 把它当作可写根：夹在它与私有路径之间的
  目录和链接同样被钉住。生产中只声明扁平缓存（只含文件），这一条是 policy 构造器自身
  不依赖调用方的纵深防御。bubblewrap 拒绝携带原地写子路径的计划，所以这一组合只在
  seatbelt 上存在。运行时记录目录（`<runtime>/host-state`，canary 记录与隔离区）位于
  数据目录旁而非私有目录内，仍是只读而不是不可见。
- 这些组合由真实 `sandbox-exec` 下的测试固定（`private_state_state_writes_darwin_test.go`）：
  同一份 policy 下缓存写入成功，而重命名/交换/删除数据目录及其祖先、读取配置与备份、
  植入 `auth.json.tmp` 等临时名、替换符号链接的数据目录或配置都被拒绝；每项对照都在
  去掉被测规则后观察到攻击成功，调换规则顺序或去掉 `StateWritePaths` 对祖先钉住的
  计入都会使这些测试失败。

### 5.2 类型边界

```go
// 可序列化、版本化。未来直接是 Runner 协议载荷。
type SandboxPlan struct {
    Version       int      `json:"version"`
    SnapshotID    string   `json:"snapshot_id"`
    Generation    int64    `json:"generation"`
    ReadOnlyRoots []string `json:"read_only_roots,omitempty"`
    WritableRoots []string `json:"writable_roots"`
    HostStateWrites []string `json:"host_state_writes,omitempty"` // 不能挂载的后端原地写的声明子路径（§9.4）
    ScratchHandle string   `json:"scratch_handle"` // = lease ID，不是绝对路径
    NetworkMode   string   `json:"network_mode"`
    Profiles      []string `json:"profiles,omitempty"`
    Backend       string   `json:"backend"`
}

// 永远只在执行节点内存生成。无导出字段、无 String()、无 MarshalJSON。
type ProcessMaterial struct{ env []string }

type ExecutionRequest struct {
    ToolCallID, RunID, LeaseID string
    Command        []string
    CWD            string
    WorkspaceRoots []string      // 来自 ExecutionScope.AllowedRoots，不从 CWD 推
    Profile        string        // 超时型 ToolProfile，与环境 profile 正交
    Timeout        time.Duration
    Sandbox        SandboxMode
    Boundary       SandboxMode   // 审批前确定并被批准的边界；执行固定于它（§5.1.2）
}

type ExecutionResult struct {
    Started           bool
    ExitCodeKnown     bool
    ExitCode          int
    Output            string
    Plan              SandboxPlan
    ProfilesMatched   []string
    FailureClass      string
    RecoveryAttempted bool
    RecoveryOutcome   string // none | prepared_and_retried | not_eligible
    HostEscapeReason  string // login | gui | host_write | sandbox_gap | ""
    ScratchBytes      int64
}
```

`WorkspaceRoots` 的当前输入是 run 级 `RootBinding` 快照：持久 workspace 的
主根/允许根与本机 CLI 的可重复 `--add-dir` 合并后，在入队前完成规范化并冻结。
附加目录不是 workspace 注册或信任操作；它仍受同一审批、安全地板和沙箱策略约束。
上下文扫描会分别发现每个显式 context root 的项目约定，写调度则按规范化路径的
相等或祖先/后代重叠关系串行，避免 `/repo` 与 `/repo/packages/shared` 被误判为
互不相关。非本机、未认证的 HTTP/IM 请求不得把 daemon 主机路径注入该快照。

两处刻意的设计：

- **`ScratchHandle` 而非 `ScratchRoot`**：绝对路径若进入持久状态，远期 Runner
  会把节点本地路径写进 `control.db`，换节点即失配。路径由执行节点从 handle 解析。
- **环境值不进 `SandboxPlan`**：`ProcessMaterial` 不可序列化，使"打印/序列化
  plan"这一最常见的泄漏动作在类型层被堵住。

### 5.3 工具结果的单一路径（2026-09-16）

`ExecutionResult → ToolDispatchResult → ToolResultEnvelope` 分别负责执行观察、
跨中间件传输和模型/UI 展示。Registry 只维护一条类型化执行链；旧的字符串工具与
审批中间件在 Registry 边界适配，`Dispatch` 也进入同一条链。`terminal`、`verify`
和 `execute_code` 直接返回类型化结果，文件工具的变更观察由 EvidenceMiddleware
加入结果。参数转换、审批和脱敏不得丢失进程事实、失败语义或证据引用。

- `Invoked` 表示工具体是否进入；缺失表示旧 backend 无法证明。它不代表副作用完成。
- `Process.Started` 与可选 `ExitCode` 来自真实进程；无法获得退出码时保持未知，
  不默认成功。既有持久命令证据格式使用 `-1` 表达未知，不修改历史行或数据库结构。
- 退出码、验证绑定和恢复结果不再借可变参数的私有字段回传。验证绑定先校验，
  再由证据中间件局部持有。失败仍可同时带有输出和实际文件变更。
- 外部工具不能声明本地进程/验证事实；Registry 丢弃这类声明。类型化结果不包含
  执行权限或重试许可，执行账本与恢复策略仍拥有这些约束。
- 工具返回后若结果落库失败，保留已观察输出，报告 `tool_outcome_unrecorded` /
  `outcome_recording`，要求先观察效果。不能伪装成“尚未执行”，也不能直接重放。

新增接口不携带 Task/Thread。账本从可信 `ToolInvocationScope.RunID` 取执行身份；
旧 `TaskRuntimeContext.RunID` 的兼容读取集中在一个接缝，两者冲突则派发前拒绝。
存储迁移继续由既有 Run + Work Journal active plan 管理；这里不新增 Session/Lane
对象，不用结果类型承担持久迁移，也不将兼容层的存在视为迁移完成。

### 5.4 完成前检查与证据依赖（2026-09-17）

`finish_run(done)` 在既有 Run 计划与执行账本检查之后，调用最终化使用的同一份
持久证据投影。缺口在 Main 仍能修复时返回；最终化仍重读证据，不能把早先的通过
当成对后来变更的豁免。证据查询失败或记录损坏保留 blocked，不能当作无验证需求。

`verify.local_dependencies` 由 Main 声明当前观察方法读取的本地文件/目录，运行时
归一化并约束路径，再按实际文件变更判断新鲜度。显式空数组只适用于不依赖本地
文件的观察；省略该字段保留全 Run 文件变更失效规则。版本 2 只赋予新声明依赖
语义；旧绑定不升级、不重写历史结果。判据充分性仍由 Main 判断，声明本身不授予
权限，也不证明目标完成。

新声明的检查可在原工作单元关闭之后，在同一个归属明确的 Run 内替代重验；工作
单元的历史快照保持不变。旧绑定仍限制在当前打开的工作单元，不能借重验升级权限。
输入目录覆盖其子项，符号链接同时保留调用路径与实际目标。失败的写操作只要留下
不同哈希也使相关证据过期。替代检查必须保留原判据、目标、cwd 与依赖；无关检查
成功不能擦掉其他失败或过期义务。报告、配置、脚本与源文件使用同一规则，没有
扩展名、场景或模型白名单。只覆盖当前工具证据链实际观察到的变更；不声称能检测
任意 Shell 或外部进程产生的全部文件/远端变化，也不替代持久事件传输保障。

`/resume` 按精确 Run 读取终止说明和下一步，区分等待用户决定与验证未完成；这
只是证据展示，不调用模型、不自动清理 Attention、不修改历史交接或审批状态。
验证未完成的 Run 优先展示运行时缺口，避免模型先前的可选下一步掩盖真正的阻塞。

## 6. 环境快照与三指纹

### 6.1 Lease 扩展

```go
// internal/executionenv/types.go
type Lease struct {
    // ... 现有字段 ...
    EnvironmentSnapshotID  string
    EnvironmentGeneration  int64
    PrincipalFingerprint   string // 已有：账号 / profile / context
    EnvironmentFingerprint string // 新增：归一化后的 PATH / HOME / proxy / toolchain
    CredentialSourceHash   string // 新增：凭证来源名称与路径，不含值
}
```

### 6.2 EnvironmentRegistry

进程内注册表，放 `internal/executionenv`，**不 import gateway 包**（Runner 前提）：

- run 开始时从过滤后的 operator 环境创建 snapshot；`BuildProcessEnv`
  （`process_env.go:32`）保留为**唯一过滤路径**；
- secret 值只保存在内存；SQLite 只保存 snapshot ID、generation、变量名摘要与
  三个指纹；
- 所有 exec 工具必须经 lease 获取环境，**禁止任何其他 `os.Environ()` 调用点**
  （加一条扫描式测试）；
- 同一 run 重试复用原 snapshot；只有新 run 才选用最新 generation。

### 6.3 指纹归一化（必须）

若对 PATH 原文取指纹，则每次 daemon 重启（新 shell、新 fnm 目录）指纹必变 →
每次恢复都进 `waiting_user`，机制沦为噪音闸门。依据见 §2.3。

```
指纹计算前先归一化：
  1. 丢弃会话级易失条目：/run/user/*/**、*/fnm_multishells/*、入表时已不存在的目录
  2. PATH 只保留存在的目录，顺序保留
  3. 只纳入语义相关键：PATH / HOME / SHELL / LANG / *_PROXY / NO_PROXY / profile 声明的变量
  4. 不纳入：PID、会话 ID、临时目录、终端变量、凭据形状变量（后者归 CredentialSourceHash）
  5. 易失条目数量单独记为 volatile_count，只做诊断，不参与指纹
```

### 6.4 重启恢复判定

snapshot 在内存、lease 在 SQLite，daemon 重启后必然失配。判定规则：

| 情况 | 处理 |
|---|---|
三指纹一致 | 重建 snapshot，发 `environment.snapshot_rebuilt` 事件，续跑 |
token 轮换但 principal 与凭证来源不变 | 允许恢复，透明重读 |
PATH / 账号 / 凭证来源任一变化 | `waiting_user: environment_changed` |

**绝不允许静默用新环境续跑。**

### 6.5 Durable Binding（2026-08-01）

新 watcher 持久化版本化、无密钥的 `executionenv.Binding`。它由创建 run 的
lease 派生，冻结环境 profile、`CredentialRef`、workspace 信任等级、实际获批
capability 及其来源，同时记录 snapshot id/generation 与三类非秘密指纹。该
Binding 也是未来 Runner job envelope 的执行环境契约；真实环境变量值只存在于
节点内存的 `Snapshot`/`ProcessMaterial`，绝不序列化。

注册 preflight 与后台轮询解析同一个 Binding。同一进程内优先使用精确 snapshot；
重启后 snapshot id 只能当索引，仍须校验 principal/environment/
credential-source 三类指纹才能重绑。后续新增授权不能扩大已运行 watcher 的
权限；workspace trust 被撤销，或持久 capability 被撤销/过期时，watcher 必须在
下一次命令前停止。一次性注册授权只活到 watcher 的有界 deadline，并可通过取消
watcher 收回。旧版本没有 Binding 的 watcher 继续走兼容路径，升级不会直接遗弃
历史工作。

Binding 还冻结三项事实：创建它的调用所处的沙箱策略（`sandbox_policy`：启用、
required），注册审批所解析出的边界（`approved_boundary`：`isolated` 或
`host`），以及该审批所覆盖的原地凭据写入（`host_state_writes`，见 §9.4）。网络
不被冻结：轮询的网络就是 Binding 的 `network:shared` capability，每次轮询前按
当前策略校验。轮询只靠 Binding 描述自己：执行引擎从中推导策略，无论由谁发起轮询。
每次轮询在冻结策略之上叠加当前配置，只收紧、绝不放宽，所以注册之后关闭沙箱不会
把轮询移到 host。每次轮询还像任何已批准的调用一样被钉在已批准的边界上：以隔离
批准、而后端此刻兑现不了时不派发（`sandbox_boundary_unavailable`，环境失败），
不会改在 host 上运行；以 host 批准的，在策略要求隔离后被拒绝。host 钉住在运维者
仅仅启用沙箱时依然有效——审批覆盖的是 host，只有 `required` 才收紧它，要获批隔离
必须重新注册。每次轮询都受已记录的写入约束。三个字段在存储的 JSON 中都是可选的，
没有记录它们的旧行照旧：没有冻结策略的 watch 跟随当前配置；没有记录边界的 watch
只按策略解析（后端消失则落到 host，与以往一致）；没有记录写入的 watch 不覆盖任何
写入，因此配置让它原地写凭据缓存时，这类轮询被拒绝。本二进制无法解释的边界会拒绝
轮询而不是猜测；早期版本存在策略里的网络标志被忽略。

凭证值不进入 Binding。文件型 token 由每个 watch 的稳定 overlay 承载，CLI 在
overlay 内刷新的 token 可供后续轮询继续使用；永久登录变更仍需获批 host 流程，
因为 `write_back` 有意未实现。person 级 toolchain cache 不可用时降级为 watch
私有 cache，缓存缺失不得让合法 durable work 失败。

## 7. Run scratch

```
~/.selfmind/runtime/leases/<lease-id>/     0700
├─ tmp/                                    0700
└─ state/                                  0700
~/.selfmind/runtime/toolchain/<person-id>/ 0700   ← person 级持久，不随 run 清理
```

执行规则：

- **`$SELFMIND_RUN_TMP` 是唯一的跨模式承诺**，`TMPDIR` 同时指向它；
- isolated 模式**同一 source 绑两次**：`--bind <lease>/tmp <lease>/tmp` 与
  `--bind <lease>/tmp /tmp`，使该绝对路径在 host 与 isolated 下字面一致（E4）；
- **scratch root 绝不能位于 `/tmp` 之下**（E4 已复现遮蔽失败）；
- 提示词只承诺 `$SELFMIND_RUN_TMP`，**不承诺 host 模式下字面 `/tmp` 连续**；
- run 终态后 24h TTL 延迟清理；启动时清理超 TTL 且无活跃 run 的目录；
- `runtime/` 禁止进入 artifact、memory、索引与备份；
- 容量软阈值 2 GB：不强杀当前命令，超限记事件并阻止下一次执行，错误文本必须
  包含清理入口。

## 8. Profile catalog（L1，Go 数据表）

### 8.1 类型（无插值语言）

```go
// internal/tools/envprofiles/types.go
type CredentialAccess string

const (
    CredentialAccessOperator  CredentialAccess = "operator"  // 需宿主 operator 凭证
    CredentialAccessToolchain CredentialAccess = "toolchain" // 只需工具链缓存，无凭证语义
    CredentialAccessNone      CredentialAccess = "none"
)

// StateSource 用有序候选替代 ${VAR:-default} 插值
type StateSource struct {
    EnvVar      string // 非空且该变量有值时优先
    HomeRelPath string // 回退：$HOME 下相对路径
}

type CopyIn struct {
    From     StateSource
    Include  []string // 相对 glob，必须非空
    Exclude  []string
    MaxBytes int64
    MaxFiles int
    MaxDepth int
}

type TargetKind int

const (
    TargetLeaseState TargetKind = iota // <lease>/state/<key>
    TargetToolchain                    // ~/.selfmind/runtime/toolchain/<person>/<key>
    TargetScratch                      // <lease>/tmp
    TargetHostPath                     // 宿主原路径（只读）
)

type EnvRedirect struct{ Name string; Kind TargetKind; RelPath string }
type MapRO       struct{ From StateSource }
type MapRW       struct{ Key string; Persistent bool }

type EnvProfile struct {
    ID               string
    MatchExecutables []string // 全局唯一，构建期测试保证
    RequiresProfiles []string
    CredentialAccess CredentialAccess
    CopyIn           []CopyIn
    MapRO            []MapRO
    MapRW            []MapRW
    EnvRedirect      []EnvRedirect
    WriteBack        *WriteBackSpec // P0 恒为 nil，仅占协议位
}
```

### 8.2 首批 catalog

```go
// internal/tools/envprofiles/catalog.go
var Catalog = []EnvProfile{
    {
        ID:               "gcloud",
        MatchExecutables: []string{"gcloud", "gsutil", "bq"},
        CredentialAccess: CredentialAccessOperator,
        CopyIn: []CopyIn{{
            From:    StateSource{EnvVar: "CLOUDSDK_CONFIG", HomeRelPath: ".config/gcloud"},
            Include: []string{"*.db", "active_config", "config_sentinel",
                              "configurations/**", "legacy_credentials/**"},
            Exclude: []string{"logs/**", "cache/**"},
            MaxBytes: 5 << 20, MaxFiles: 100, MaxDepth: 5,
        }},
        MapRW:       []MapRW{{Key: "gcloud/logs"}, {Key: "gcloud/cache"}},
        EnvRedirect: []EnvRedirect{{Name: "CLOUDSDK_CONFIG", Kind: TargetLeaseState, RelPath: "gcloud"}},
    },
    {
        ID:               "kubectl-gke",
        MatchExecutables: []string{"kubectl", "helm"},
        CredentialAccess: CredentialAccessOperator,
        RequiresProfiles: []string{"gcloud"}, // 状态叠加，不重复复制
        MapRO:            []MapRO{{From: StateSource{EnvVar: "KUBECONFIG", HomeRelPath: ".kube/config"}}},
        MapRW:            []MapRW{{Key: "kube-cache"}},
        EnvRedirect:      []EnvRedirect{{Name: "KUBECACHEDIR", Kind: TargetLeaseState, RelPath: "kube-cache"}},
    },
    {
        ID:               "go-toolchain",
        MatchExecutables: []string{"go", "gofmt"},
        CredentialAccess: CredentialAccessToolchain,
        MapRW: []MapRW{{Key: "go-build", Persistent: true}, {Key: "go-mod", Persistent: true}},
        EnvRedirect: []EnvRedirect{
            {Name: "GOCACHE",    Kind: TargetToolchain, RelPath: "go-build"},
            {Name: "GOMODCACHE", Kind: TargetToolchain, RelPath: "go-mod"},
        },
    },
}
```

后续批次（第 10 步）：`aws`、`gh`、`docker`。

### 8.3 构建期校验（CI 拦住，而非运行时报错）

`catalog_test.go` 断言：ID 唯一；`MatchExecutables` **全局唯一**（一个可执行
文件只映射一个 profile，从源头消除匹配歧义）；`RequiresProfiles` 指向存在的
ID 且无环；`CopyIn.Include` 非空且三个上限均 > 0；所有相对路径不含 `..`；
`WriteBack` 全部为 nil。

### 8.4 匹配规则

> **2026-07-30 更正（批次 2 已实现）：匹配规则按"可判定性"分岔。**
> 任何能执行代码的载荷都能把真实程序藏起来：`python3 - <<'PY' …
> subprocess.run(['gcloud',…])` 的程序集只有 `python3`，于是 gcloud 拿不到
> 凭据状态（当日 GCP watcher 失败链的第一因）。同类还有脚本文件、`make`、
> `npm run`、`find -exec`、`execute_code`。
>
> 现行规则：
>
> - **可判定载荷** → 只准备它点名的 profile。非 GKE 的 kubectl 依然拿不到
>   Google 凭据（round 2(e) 的最小权限结论不被推翻，有测试钉住）。
> - **不可判定载荷** → 并上 `AvailableOnHost`（宿主上确实存在状态的
>   operator profile）。理由是此时"它会跑什么"不可知，能依据的只有宿主事实。
> - 准备结果按 lease 记忆（`exec_prepared.go`），键包含 state 目录、**解析后**
>   的 profile 集合（`envprofiles.Resolve`，否则 kubeconfig 中途变成 GKE 会复用
>   缺 gcloud 的准备）、trust 与 credential 访问位。
> - inventory 只收 `credential_access=operator` 且宿主有状态者；toolchain 缓存
>   没有工具就没有意义，仍然只走匹配；缺 toolchain root 时不擅自引入需要它的
>   profile（否则会让从未提到该工具的命令硬失败）。
>
> 副作用必须记录：整包应用 inventory 后，catalog 里两个 profile 重定向同一个
> 环境变量将变成**所有命令**的硬失败，因此唯一性改为构建期断言
> （`TestCatalogRedirectVariablesAreUnique`）。

用 shell AST 提取命令**所有 segment 的真实程序集合**，跳过内建与控制结构
（`set`/`for`/`if`/`while`/`cd`/`export`/`trap`/`echo`/`printf`），对集合逐个
查 catalog，命中的 profile **叠加**：

- `CopyIn` / `MapRO` / `MapRW` 取并集，同 key 去重；
- `RequiresProfiles` 递归引入，已引入则跳过；
- **同一环境变量被两个 profile 重定向到不同目标 → 硬失败并报错**，不静默取其一。

## 9. 原语

| 原语 | 语义 | P0 |
|---|---|---|
`copy_in` | 有界、选择性复制进 lease state | ✅ |
`map_ro` | 将已批准的宿主路径只读映射 | ✅ |
`map_rw` | 映射 lease state 或 person 缓存内的可写目录 | ✅ |
`map_rw_at` | 把 lease state 的可写目录绑定到不可配置的宿主路径上；不能挂载的后端改为该声明子路径原地可写（§9.4） | ✅ |
`synthesize_dir` | 在声明的 state root 上挂可写 tmpfs，仅保留声明的子项可读；不能挂载的后端用它界定原地写的范围 | ✅ 2026-07-30 |
`env_redirect` | 把工具环境变量指向 lease state / toolchain / scratch | ✅ |
`write_back` | 把 state 变更写回宿主 | ❌ **仅占协议位** |

引擎只理解这些原语，不含任何 vendor 分支；具体规则全在 catalog。

### 9.3 `synthesize_dir` 存在的理由（挂载点，不是权限）

`map_rw_at` 单独不成立：bind mount 需要**挂载点**，而只读根下 bwrap 无法创建它。
宿主从未用过 SSO 时 `~/.aws/sso/cache` 不存在，于是 overlay 挂不上，bwrap 在命令
启动前整体失败（`Can't mkdir parents for …/.aws/sso/cache: Read-only file
system`）——后果不是"SSO 刷新失败"而是**命中该 profile 的所有命令全部失败**，
包括 `aws --version`；durable watcher 无 host 逃逸，只能重试到超时。

tmpfs 是可写的，因此声明 state root 后其下任意嵌套挂载点都能创建。语义要点：

- 未声明的子项被 mount 遮蔽 → **fail-closed**（`credentials_bak` 在沙箱内不可见）；
- 宿主没有该 state root 时跳过（没有可壳化的状态，且同样缺挂载点）；
- 挂载顺序固定为 `tmpfs → 只读子项 → 可写 overlay`，顺序错了等于没修；
- plan 构造后自校验：目标既不存在、又不在任何 synthesized root 之下的 overlay
  **不进 plan**，改为记录 note——宁可让工具自己报"状态缺失"，也不要交出一个
  执行面无法兑付的 plan。这条校验就是 §12 接缝 10（节点可拒绝 plan）的本地版。

### 9.4 不能挂载的后端：声明子路径原地可写（D3，2026-10-07）

seatbelt 没有挂载命名空间，既做不出 tmpfs 外壳，也做不出私有覆盖层。准备阶段
在后端回答 `MountsToolState() == false` 时设置 `ApplyContext.InPlaceState`：
`synthesize_dir` 与 `map_rw_at` 不再物化（没有 lease 内私有缓存，也不复制种子
token），改由 `PlanHostStateWrites` 把每个 `map_rw_at` 需求翻译为宿主真实路径上
的一条写授权（plan 的 `HostStateWrites`，其中扁平缓存另列于
`FlatHostStateWrites`，plan 版本 5；seatbelt 策略中的 `STATE_WRITE_n` 参数）。
规则：

- 只授权声明的子路径本身（如 `~/.aws/sso/cache`、`~/.aws/cli/cache`），且必须
  严格位于同一 profile 声明的 state root（`synthesize_dir`，如 `~/.aws`）之内；
  state root 本身、`config` 与 `credentials` 等只读状态永远不可写。子路径目录
  本身是权限边界，与可写根一样不可删除，其 mode、flags、owner、扩展属性与 ACL
  也不可修改；目录内的文件仍可写。
- 只有 profile 声明为扁平（`MapRWAt.Flat`：只放文件，如 AWS SSO 与 CLI 缓存
  每个 token 一个 JSON 文件）的缓存才原地写入；未声明扁平的需求在规划时拒绝。
  修复因此只是列出一个沙箱无法替换的目录，沙箱在其下互换什么都无法引偏修复。
  宿主工具在这样的缓存里只应遇到单链接的普通文件。四层保证：
  - seatbelt 策略按操作所指的条目拒绝：在缓存内创建符号链接、FIFO、socket、
    设备节点或目录（原地创建、rename 进来或 clone 进来），以及任何硬链接（指向
    普通文件的硬链接本身也是普通文件，单靠类型规则会放过它）。
  - 策略拒绝不了操作没有指名的东西：用 `renamex_np` 的 `RENAME_SWAP` 把工作区
    的链接、管道或目录与缓存文件互换。因此每条原地写缓存的命令两侧都有守护：
    启动前，缓存里已有的宿主工具不应遇到的条目（来自更早的运行、其他写入者等）
    先移走，然后照常运行，规划不再因此拒绝。之后由一个无论如何结束都会执行的
    延迟步骤（成功、失败、超时、取消，或启动后的 panic）先停掉命令的进程组，再
    停掉仍在这条命令沙箱下运行的每个进程：每条命令的 seatbelt profile 带一个
    逐命令的 canary（`com.selfmind.exec.` 加 24 位随机十六进制：只有该 profile
    能查找的唯一 mach 全局名，背后没有服务），对本用户的进程做 `sandbox_check`
    即可找到其下的每个进程，无论它换到了哪个进程组或会话（`setsid`、守护进程）。
    每个 pid 在发信号前再检查一次。检查本身先被确认：在同一 canary 下启动的探针
    必须被找到，无法回答的进程绝不被当作 canary 之外的进程。无法确认的停止在
    工具结果与证据中如实报告，绝不算作成功。之后才再次列出缓存。
  - daemon 启动时（任何命令之前）先停掉仍在某个已记录 canary 下运行的进程
    （命令未完成——daemon 在其运行时崩溃或被杀，或命令自己的停止未能确认，
    此时记录保留），只修复这些命令记录过的缓存写路径。版本化记录有大小上限，
    在命令产生效果前同步落盘，保存失败则不启动命令；根或祖先路径变化、未知记录
    版本均报告而不扩大范围。旧空记录仅用于清理进程，缺少缓存范围会在日志与
    `/diag execution` 中说明，不从当前环境推测；未解决的修复保留记录。记录按 daemon 区分（以数据目录
    命名，因为数据目录并列的 daemon 共用 runtime 根），且只处理生成格式的 canary
    记录。无论因何关闭，daemon 最多等待 15 秒，让被中断的命令完成修复后再退出。
    私有路径、进程组收据、恢复记录归属、在途修复计数及最近扫描结果由该 daemon 的
    `ExecutionRuntime` 持有，通过冻结执行作用域与上下文注入；前台、后台和持久
    watcher 均使用自己的实例。启动或停止另一个 runtime 不会替换这些状态。
  - runtime 的记录（`<runtime>/host-state/`：canary 记录与隔离区）对每条
    seatbelt 命令只读，即使其可写根包含 runtime 根。
  - 修复把宿主工具不应遇到的条目（链接、特殊文件、多链接文件、目录）移入
    runtime 根下的隔离区（`<runtime>/host-state/quarantine/…`，绝不在工具的
    state root 之下）。缓存路径本身不再是真实目录、或超过 4096 个条目时整体
    移走，由工具重建。列出期间消失的条目（写入者重命名就位的临时文件）直接跳过，
    绝不因此移走正在使用的缓存。移动失败时，不含自身数据的条目（链接、特殊文件、
    多链接文件）直接删除，其余留在原处并报告。工具结果写明修复了什么、停掉了
    什么，最多列出十个条目（"and N more"），每个路径加引号并转义控制字符与方括号；
    调用证据记录修复（`tool.environment` 的 `state_repairs`，最多列二十个并给出
    总数）。隔离的副本可能含 operator 的 token，因此每个隔离目录在七天后、或已有
    二十个更新的隔离目录时删除，启动时与每次修复后清理；`/diag execution` 显示
    隔离区、保留策略与最近一次启动清理。
  保证因此是：沙箱命令种下的条目在调用返回前移除，它在沙箱下留下的每个进程也
  在返回前停掉。宿主工具只可能在这样的命令运行期间遇到种下的条目（窗口是命令
  本身的运行时间，直到其后的扫描）；若 daemon 在命令运行时死掉，则从那时起直到
  下一次启动清理——命令的进程在被那次清理停掉之前仍保有写权限。停止无法确认时
  （探针未被找到、`sandbox_check` 无法回答某个进程、或 canary 下的进程多次 kill
  后仍存活），调用会如实说明，而不是声称缓存已受守护。Intel Mac（darwin/amd64）
  上这一停止从未实际运行过，因此不尝试：遗留进程不会被停掉，每次原地写调用都说明
  在该架构上无法确认停止，也不保留启动清理同样无法处理的记录；只有命令前后的
  扫描生效。沙箱始终能留下的只是普通文件内容（见下文的污染说明）。
- 根以下的每一级都必须是真实目录：任何一级是符号链接都拒绝，无论它指向根外
  （报告"解析到声明根之外"）还是根内——根内的链接可能比声明的子路径更宽
  （`sso/` 之于 `sso/cache`），能改指向的人就能引导后续授权。不是目录、会覆盖
  或落入只读状态、或者没有声明根，同样表达不了，拒绝并给出具体原因。
- 缺失时只在声明根内逐级创建（每级 `0700`，每级都必须是真实目录而非链接）；
  规划之后若中间目录被换成指向根外的链接，同样拒绝，根外什么也不创建。规划与
  创建在每条命令上重做，不进准备缓存。
- 只有策略允许本次调用使用 operator 凭据时才授权。凭据被扣留时没有可给的私有
  空缓存，宿主缓存保持只读且可见（seatbelt 也无法隐藏它），工具可能报只读而
  不是"未登录"。
- 宿主没有 state root 时没有可写的状态，与 Linux 一样跳过并记录 note。

这**弱于** Linux 的重定向：被写的是操作者真实的缓存，沙箱内命令可以刷新、替换
或污染其中的 token，缓存里的其他内容也可读。因此这些路径属于审批边界：
containment 摘要列出它们（§5.1.2），`/diag execution` 按 profile 列出在当前配置
策略下本机是否可在沙箱中运行（`exec_sandbox` 关闭时一律在 host 执行）以及会原地
写哪些路径（只列路径，不读内容）。

### 9.1 `copy_in` 实现硬约束

```
include / exclude / max_bytes(5MiB) / max_files(100) / max_depth(5)
拒绝：越界 symlink、设备文件、socket、FIFO、指向界外的 hardlink
staging 目录完成后原子 rename（staging 与目标必须同文件系统）
保留权限位（credentials.db 必须仍是 0600）
超限 = 硬失败 + 可诊断错误，绝不"尽力复制"
```

依据：E1（231 MB，其中 logs 占全部）与 E2（按 include 集实际 49,779 字节）。

### 9.2 `write_back` 本轮不实现的理由

gcloud 的 `credentials.db` / `access_tokens.db` 是 SQLite，复制回宿主涉及锁、
WAL、一致性与覆盖冲突，普通文件复制不可接受。将来实现时必须带源文件指纹、
冲突检测与原子提交。

**本轮的后果必须写进提示词与文档**：沙箱内的 `gcloud auth login` /
`docker login` / `gh auth login` **不会生效**，永久身份变更必须走审批的 host
流程。否则 agent 会在沙箱里"成功登录"、下一条命令又未登录。

## 10. Trust × credential_access 矩阵

profile 声明 `credential_access`，**策略层**结合 workspace trust 裁决：

| `credential_access` | trusted workspace | untrusted workspace |
|---|---|---|
`operator` | 使用已批准的 operator 凭证（copy_in + env_redirect） | **默认拒绝**，需 `credential:read`；未授权时给空 state → 工具报"未登录"而非 read-only |
`toolchain` | 映射 person 级持久缓存 | **同样映射**（无凭证语义） |
`none` | — | — |

`credential:refresh` 只用于永久身份变更（登录类），本轮通过审批的 host 流程
处理。三个能力常量（`credential:read`、`credential:refresh`、`host:escape`）
在 `internal/executionenv/types.go` 中已存在。

### 三种存储生命周期

| 生命周期 | 路径 | 内容 | 清理 |
|---|---|---|---|
per-lease | `runtime/leases/<lease>/{tmp,state}` | 临时文件、凭证覆盖层副本 | run 终态 + 24h TTL |
per-person 持久 | `runtime/toolchain/<person>/` | Go/npm/pip 等缓存（无凭证） | 容量上限 + LRU |
host 只读 | 宿主真实路径 | 一切只读映射 | 永不写 |

**为什么必须区分 `toolchain`**：`workspaces` 表里 `selfmind` / `ai` / `game`
当前都是 `untrusted`（`migration_review_required`）。若按"untrusted → 私有
HOME"一刀切，本仓库每次构建都拿冷缓存。E6 也证明两类风险不同：Go 在只读
GOCACHE 下降级但不失败，gcloud 硬失败。

## 11. 失败分类、恢复与授权账本

### 11.1 分类（照 codex `sandboxing/src/denial.rs:6`）

```
if mode != isolated → 跳过全部 sandbox_* 判定        ← “有沙箱”是判定前提
exit_code ∈ {2,126,127} → 不算沙箱拒绝
关键词：read-only file system / operation not permitted / permission denied
        / configuration directory may not be writable / unable to create private file
```

分类集：`sandbox_fs_denied`、`credential_state_readonly`、`credential_missing`、
`credential_expired`、`network_denied`、`tool_missing`、`shell_syntax`、
`timeout`、`command_failed`。

必须修的现存 bug：`tool_errors.go:39` 的 `auth` 规则含裸 `credential` 子串且排
在 `permission` 之前；`:223` 对 auth 追加的"host 不是修复手段"必须只在**真
auth** 时出现。

**单一分类路径（2026-07-30 收口）**：`ClassifyToolError` 是唯一分类器。watcher
曾另有一张 marker 表（`classifyExternalWatchCommandDefect`），两张表恰好在关键
用例上分歧——`read-only file system` 只在其中一张里——于是一个连沙箱都构造不出
的 watch 被判为"仍在运行"，每 30 秒重试到两小时超时。该表已删除；durable 路径
返回完整 `ExecutionResult`，`httpapi/external_watch_policy.go` 只做**策略**
（park / retry / observe），不做分类。新增失败类只改分类器与策略表两处。

### 11.1.1 Durable check 的四层与策略表

| 层 | 判定来源 | 失败后的动作 |
|---|---|---|
L0 环境 | `credential_state_readonly` / `sandbox_fs_denied` / `permission` / `credential_missing` / `credential_expired` / `auth` / `environment` | **park**（`blocked_environment`），零重试 |
L1 执行 | `syntax` / `not_found` | **park**（`invalid_check`） |
L1 瞬时 | `timeout` / `network` | retry |
L2 观察 | 其余（含 `unknown` + 非零退出） | observe：允许匹配 pattern |
L3 业务 | success/failure pattern | 终态 |

### 11.1.2 注册前预检（批次 2）

durable check 是唯一没有 host 逃逸、也没有模型在环的执行路径，所以那里的环境或
命令缺陷是终局的：daemon 只能重复它。因此 `watch_external` 注册时先用**本 run 的
材料**跑一次，四种结果不对称：

| 预检结果 | 动作 |
|---|---|
观察到成功 | 不注册，直接告诉 agent 已完成 |
观察到**失败**（首次即命中 failure_pattern） | **返回错误**，不产生终态。首检失败正是歧义最大的情形（当日 GCP 检查打印自己的 `BUILD_FAILED`，而两个构建其实都成功），agent 还在回合内、有工具可以直接核实 |
阻塞类失败（环境 / syntax / not_found） | 拒绝注册，带上 typed class |
非终态 | 正常注册 |

预检不新增审批面：`watch_external` 本身就是 exec tool，其 `command` 在注册时已过
安全下限与审批；同一条命令原本稍后就会被 daemon 无人值守地执行。预检超时从
调用参数读取，但硬限制为 120 秒；超过 30 秒的预检进入 long-running 执行档案，
让需要网络或认证握手的真实检查有完成机会，同时保持注册调用有界。

轮询命令自身必须是**只读观测**：要么命中声明式只读命令目录，要么是 operator
批准且内容哈希固定的观测脚本。shell 写入、变更型子命令、未知客户端，以及把观测与
变更混在一起的管道都在注册前拒绝。这个限制不妨碍最终状态回写；回写属于观测达到
终态后的一次性 watcher finalization run，不属于周期轮询命令。

注册成功本身就是可信的生命周期交接。kernel 直接写入结构化
`waiting_external` outcome 并结束前台 turn，不再要求模型额外调用 `finish_run` 或
再生成一轮回复。同一批次可以注册多个 watcher，但首个 watcher 之后的非 watcher
调用会被隔离。等待期间没有 active person run，也不消耗模型 token，因此用户可立即
开始新任务；终态收尾仍是 one-active-run 约束下的真实 daemon 工作，客户端在这段短
时间显示“后台收尾”，新输入如实排队，而不是继续显示前台计时器。

硬约束：**L0/L1 失败永不进入 pattern 匹配**；`success_pattern` 额外要求
`ExitCode==0`（失败的检查打印出的 "SUCCESS" 是自相矛盾的证据），`failure_pattern`
不要求（状态类 CLI 会以非零退出报告真实失败）。同一 `failure_class + output_hash`
连续 3 次即熔断 park，并落 `external_watch.blocked` 事件。park 的 `last_error`
带结构化前缀，下游（finalization prompt、任务摘要、IM 通知）据此声明"外部状态
未被观察"，绝不写入 succeeded/failed。

watcher 收尾同时维护两条不同的事实：finalization run 描述 SelfMind 是否成功完成
本地核验与记录，`RunOutcome.external` 描述被观察的外部目标状态。外部构建失败时，
finalization run 可以正常 `done`，而任务仍根据 external outcome 显示为 blocked；
不得再把外部失败伪装成 agent 执行失败。`notified` 也不是“已创建收尾队列行”的
同义词：只有 IM/outbound 已确认接收，或原始 CLI 仍在线且已接收耐久事件时才置位；
否则保留待通知状态供后续有界补投。

### 11.2 恢复许可（按引擎阶段判定，不猜命令语义）

| 阶段 | 一次自动恢复 |
|---|---|
引擎准备阶段失败（copy_in / 挂载 / env 解析） | ✅ 用户命令尚未启动 |
命令已启动、**零输出**、分类为 `sandbox_fs_denied` 且命中已知 profile | ✅ 准备状态后重跑一次 |
命令已启动并产生任何输出 | ❌ 返回结构化错误交给 agent |
`network_denied` | 工作区已有网络许可（信任且策略允许、存量或 run 授权）：不再询问，返回类型化失败 `network_withheld_by_policy`（保留原输出、分类与 effect 确定性，说明未重放，并提醒：命令在联网前若已改变状态，应先检查该状态，因为重试会再次执行那些步骤），由模型以 `network: true` **显式**重跑（`terminal`、`verify`、`execute_code` 都接受该参数）；没有任何许可：沿用 `network:shared` 审批，授权后同样以 `network: true` **显式**重跑（保持不重放立场）。只有命令自身的失败走这条路径：拒绝、安全分诊的否决（其理由可能提到沙箱网络）、挂起的审批、硬底线拦截和未派发的调用都是各自有含义的结果，原样返回，不会被改写成该类型化失败 |
host escape | ❌ 永远审批 |

**单个工具调用最多一次恢复重试。**

### 11.3 L1 对 L2 的下限表

```go
// internal/tools/grant_floor.go —— 与 hardlineProtectedRoots 同级的源码常量
var bannedGrantPrograms = map[string]struct{}{
    // 解释器 / shell：等于任意代码执行
    "sh": {}, "bash": {}, "zsh": {}, "dash": {}, "ksh": {}, "fish": {}, "env": {},
    "python": {}, "python3": {}, "node": {}, "nodejs": {}, "deno": {}, "bun": {},
    "perl": {}, "ruby": {}, "php": {}, "lua": {}, "Rscript": {},
    "osascript": {}, "powershell": {}, "pwsh": {}, "cmd": {},
    // 可执行任意命令的工具：git（hook/alias/-c core.pager）、find（-exec）、xargs
    "git": {}, "find": {}, "xargs": {},
    // 破坏性
    "rm": {}, "dd": {}, "mkfs": {}, "chmod": {}, "chown": {}, "sudo": {}, "doas": {},
}
```

加上结构性排除：含重定向、命令替换、变量赋值、heredoc、通配或多段的命令，
**一律不生成可复用授权**（codex 的同一判据）。两者任一命中 → 只批这一次。

### 11.4 L2 授权账本与简化交互

授权账本的查询、撤销、期限和审计面已经补齐；当前交互审批进一步收窄为上下文相关
的紧凑菜单：普通操作最多提供“仅本次 / 当前 run 内复用 / 拒绝”三项；host escape、
凭证读取、显式拒绝覆盖以及高风险或无法判定的请求只提供“仅本次 / 拒绝”。CLI 与
IM 都渲染 daemon 持久化的同一组选项，服务端只接受当次请求实际提供的 decision，
客户端不能通过隐藏参数升级授权范围。

`/approvals grants|revoke` 与 `selfmind approvals grants|revoke` 继续用于查看、撤销
历史 task/person 授权和显式管理的持久授权。新交互提示不再创建 task/person 级授权；
旧授权在到期或撤销前仍可读取，以保证升级兼容，但不会成为新默认。授权创建、命中和
撤销仍写 durable 事件并进入 `/diag execution`。

## 12. Runner 接缝（零新增功能）

依据 `docs/vision-saas-enterprise.zh-CN.md` §3.1/§3.9/§4.1：Runner 的触发条件是
"B 拓扑跑通且有人真把 gateway 放上云"，本轮不预建。只守住接缝，使将来那一刀
切得开。

**判据一句话：将来把 `ExecutionEngine` 整包搬进另一个进程，不需要改它的调用契约。**

| # | 接缝 | 今天的状态 | 本轮动作 |
|---|---|---|---|
1 | `SandboxPlan` 可序列化 + 版本化；`ProcessMaterial` 永不出节点 | 不存在 | §5.2 |
2 | scratch 用 handle，节点本地绝对路径不进持久状态 | 不存在 | §5.2 |
3 | `EnvironmentRegistry` 放 `executionenv`，不 import gateway 包 | 不存在 | 建包即守 |
4 | 审批保持回调形态（将来换控制面 RPC） | ✅ `scope.Approval` 已是函数指针 | 不要内联进 tools |
5 | 能力**裁决**归控制面，执行面只请求与兑付 | ❌ 有效期在 `execution_capability.go:153` 由 tools 层决定 | 由审批决策返回值携带 |
6 | 执行策略进 request/plan，不用进程级全局 | ❌ `exec_sandbox.go:46-61` 是 package 变量 | 降级为默认值来源 |
7 | `executionScopes` 按 runID key | ❌ `workspace_scope.go:72` 按 tenantID | 迁移（约 10 处） |
8 | `SandboxBackend` 平台无关接口 | ❌ 直接调 `sandbox.Wrap` | 抽接口（约 30 行） |
9 | 环境需求是 job 信封里的**声明**，不是从命令文本推断 | ❌ §8.4 仍是推断 | 批次 2：lease 级 inventory；`SandboxPlan.Profiles` 由"结果记录"升级为"输入声明" |
10 | 节点可**拒绝**自己无法兑付的 plan（能力协商，vision §3.9） | 🔶 已有本地版：不可挂载的 overlay 不进 plan（§9.3） | 远端化时把同一校验放到节点侧 |
11 | typed 执行结果是唯一回执格式 | ✅ 2026-07-30：durable 路径返回 `ExecutionResult`，gateway 只做 reducer | 不要再增加"输出字符串 + 正则"的回执路径 |

Runner 的第一个 workload 应当是 watcher：它已经是"控制面创建、执行面重复执行、
无交互审批、结果结构化回执"的形状，`waiting_external` 与 `waiting_runner` 同构。
前置条件是它先满足 `docs/STATUS.md` 里批次 1–3 的验收，否则只是把误判搬到远端。

macOS：本轮统一 `SandboxPlan` 语义即可。Linux 由 bubblewrap 实现；macOS 第一
阶段用审批控制的 host 执行，但**复用同一份环境快照、scratch 与 profile**；
后续接 Seatbelt 时不改上层接口。

## 13. 实施顺序

`0a` / `0b` / `0c` 并列在最前：都不依赖执行引擎，改动小，且都是当日正在造成
损害的问题。

实施状态:**0a–10 全部完成**(2026-07-29)。下表保留依赖关系与文件位置作为索引。

| # | 内容 | 主要文件 | 依赖 |
|---|---|---|---|
**0a** | 分类顺序修正；仅 isolated 识别 sandbox denial；新增 `sandbox_fs_denied` / `credential_state_readonly` | `tool_errors.go:29-118` | — |
**0b** | L1 下限表 `bannedGrantPrograms` + 复杂 shell 结构性排除 | 新 `tools/grant_floor.go`、`middleware.go:598-613` | — |
**0c** | L2 管理面：list / revoke / expires_at + 审批提示显示将记住的类 + 授权事件 | `control/approval_grants.go`、`control/store.go:378`、`approval_resolver.go:256`、`gateway/command` | — |
**1** | `EnvironmentRegistry` + lease 三指纹 + 归一化 + 禁止其他 `os.Environ()` | `executionenv/{types,registry}.go`、`run_coordinator_lifecycle.go:317`、`process_env.go:55` | — |
**2** | run scratch + `SELFMIND_RUN_TMP`/`TMPDIR` + 双绑定 + TTL/配额 | `sandbox/sandbox.go`、`exec_sandbox.go` | 1 |
**3** | catalog + 有界 `copy_in` + gcloud / kubectl-gke / go-toolchain + trust 矩阵 | 新 `tools/envprofiles/*` | 2 |
**4** | `ExecutionEngine` 统一入口 + `SandboxBackend` + `WorkspaceRoots` + 类型边界 | `exec_sandbox.go`、`builtin.go:476`、`execute_code.go:128` | 3 |
**5** | 全部 exec 路径迁入：`RunSandboxedShell`（watch）、cron、maintenance | `exec_sandbox.go:288`、`httpapi/external_watch.go:162` | 4 |
**6** | shell AST 程序集合提取 + 多 profile 合并 + 冲突硬失败 | `tools/command_segments.go` | 4 |
**7** | 阶段式一次恢复 + 能力裁决归位（接缝 5） | `tools/execution_capability.go` | 5, 6 |
**8** | 接缝 6/7：进程级策略降级、scope 换 runID key | `exec_sandbox.go:46`、`workspace_scope.go:72` | 4 |
**9** | `env refresh` + 重启重建判定 + `/diag execution` | `cliapp`、`httpapi/diag.go` | 1 |
**10** | AWS / GH / Docker profile；最后 macOS Seatbelt 后端 | catalog、新 backend | 4 |

**1–4 完成后**即覆盖当日全部主要痛点：26 次 read-only 拒写、5 处 `/tmp` 断链、
2 次 workspace_scope 失败、watch 静默失败。

### P1：环境刷新（第 9 步）

- Gateway 启动时创建初始 snapshot（记 `sampled_at` 与来源）；
- `selfmind env refresh` 从当前登录 shell 抓取新环境（`bash -lc 'env -0'`），
  原子替换基线；
- 本地 CLI 启动时通过**仅限本机**的认证通道刷新；
- 新 run 使用新 generation；**已运行的 run 不静默更换 PATH、HOME 或账号**；
- 检测到 PATH 目录失效等可判定信号时自动重采一次；**不做定时重采**；
- 远程 CLI 与 IM **不允许**上传或改变 Gateway 环境。
- 操作系统托管的 Gateway 把稳定的非凭据配置位置、locale，以及安装 shell
  中精确匹配的标准代理变量写入 launchd/systemd 定义。代理只接受
  `HTTP_PROXY`、`HTTPS_PROXY`、`ALL_PROXY`、`NO_PROXY` 及其小写形式；名称
  类似但非标准的变量与带内联凭据的 URL 一律拒绝。由于 Go 的
  `http.ProxyFromEnvironment` 不读取 `ALL_PROXY`，安全的 `ALL_PROXY` 会补足
  缺失的 `HTTP_PROXY`/`HTTPS_PROXY`。Provider HTTP transport 仍保留标准进程
  环境语义；VPN、TUN 与透明路由仍由宿主网络接管。
- detached restart 可合并旧 Gateway 的 PATH 以保留工具发现，但不得复活当前
  环境已经不存在的代理变量。再次执行 `selfmind gateway service install` 或 setup
  会用当前 shell 的安全代理值刷新服务定义；普通重启只使用当前进程或既有服务
  定义，不从旧进程恢复已消失的代理。
- snapshot 中的代理值在每次执行前按实际网络视图投影：隔离网络不向子进程暴露
  标准代理变量；共享网络保留远端代理和可达的 loopback 代理。若 loopback 监听
  已失效，只从该次子进程环境移除，并在 `SandboxPlan.proxy_mode` 与执行事件中记录
  非敏感原因。模型无需记忆 `unset HTTPS_PROXY` 一类前缀；执行引擎也不按云厂商、
  项目或命令正文分支。命令自己显式删除代理变量时，识别程序名的方式与只读证明一致
  （basename 不区分大小写），`Env -u`、`BASH -c 'unset …'` 与 `/usr/bin/ENV` 都
  记入 `requested_proxy_removals` 并进入既有审查；审批理由在危险命令理由之后追加
  代理理由，不替换原理由。

### P1：可观测性（第 9 步）

`/diag execution` 增加：当前 lease 与 snapshot generation、snapshot 年龄、
命中的 ToolEnvironmentProfile、sandbox backend、run scratch 是否有效与体积、
network/credential capability、每次调用的网络决定（`tool.sandbox` 事件与进程事实里的
`network_decision`：`shared` 或 `withheld: …` 及原因，授权来源 `network_permission`）、
最近失败分类与恢复结果、host escape 次数与原因分布。网络授权（信任、存量或 run 授权、
一次性审批）只是**许可**：命令是否真正获得网络，取决于它是否按名字访问远程服务或设置了
`network: true`；显式的离线或本地形式（`--offline`、`--no-index`、指向本机路径的 git 远程、
`--help`/`--version`）不算访问远程服务，但出现远程目标（URL、`user@host:path`）或
`network: true` 时不适用。离线通过的验证只对可能下载依赖的命令（构建、测试等）追加提示，
`go vet`、lint、格式化不追加。**不显示环境变量名、值、凭证路径与命令正文。**

## 14. 测试矩阵

| 用例 | 判据 |
|---|---|
同 run 跨命令共享 `$SELFMIND_RUN_TMP` | host 与 isolated 双模式均通过 |
scratch 位置约束 | 断言 scratch root 不在 `/tmp` 之下（E4 已复现遮蔽失败） |
跨 run 隔离 | run2 看不到 run1 的 scratch |
catalog 校验 | ID / 可执行文件唯一、无环、三上限非零、无 `..`、`WriteBack` 全 nil |
`copy_in` 边界 | 超三个上限硬失败；越界 symlink / 设备 / socket / FIFO 被拒；权限位保留 0600；原子 rename |
fake gcloud 覆盖层 | 能在 state 写入；宿主凭证目录 checksum 不变 |
指纹归一化 | 注入伪造的 `fnm_multishells/<新>` 后指纹**不变** |
重启恢复 | 三指纹一致 → 重建 + `environment.snapshot_rebuilt`；任一变化 → `waiting_user: environment_changed` |
snapshot 代际 | 更新只影响新 run；在跑的 run 不换 PATH/HOME/账号 |
一次恢复上限 | 单次调用最多一次；有输出后永不重放 |
多 profile 冲突 | 同变量被重定向到不同目标 → 硬失败并报错 |
L1 下限 | `bannedGrantPrograms` 中任一程序、或复杂 shell → 不产生可复用授权（表驱动） |
L2 期限 | host-escape 授权带 `expires_at`；过期后不再命中 |
L2 可见 / 可撤销 | `/approvals list` 渲染人类可读、不含 raw hash；revoke 后重新要求审批并有事件 |
审批提示 | 文本包含将记住的具体类；断言不含 `pattern_key` 原文 |
密钥不外泄 | SQLite、事件、argv、日志、模型上下文均无环境密钥；`SandboxPlan` 序列化不含 env |
真实路径 | `gcloud auth list` 与 GKE `kubectl` 在隔离模式首次成功 |
**配置面** | 断言 `config.yaml` 顶层键数与 `exec_sandbox` 键数**不增加** |

最后一条是"不增加配置复杂度"这条约束的唯一可执行保障。

### 14.1 每批执行链变更的故障矩阵

每批变更同时维护下列故障边界；检查输入保留、效果不被盲目重复、未完成状态不被
覆盖。注入失败证明运行时约束，真实 provider eval 验证模型行为，两者不互相替代。

| 边界 | 故障与应保留事实 | 当前回归入口 |
| --- | --- | --- |
| 派发前 | 审批拒绝或持久 claim 失败：工具执行次数为零；已接受输入仍归原 Run | `tools/TestTypedFactsSurviveRedactionAndApprovalRefusal`、`kernel/TestTypedDispatchFailureMatrix`、`control/TestSteeringDeferralAndBootRecovery` |
| 效果发生后 | 取消进程或文件写入后返回失败：保留已启动、退出码未知/非零、文件前后哈希；失败不能擦掉实际变更 | `tools/TestTypedCancellationPreservesPartialEffects`、`tools/TestLegacyFileFailurePreservesObservedEffect` |
| 结果保存前 | 结果写库失败保留不确定 claim 和输出；新 Agent 不能重放同一调用；artifact 保存失败不重执行工具 | `kernel/TestTypedDispatchFailureMatrix`、`control/TestToolLedgerUncertainWindow` |
| 最终提交时 | 原子提交失败不能留下半套完成状态；不同 Run 的未完成工作不被覆盖 | `control/TestMaterializeRunFinalizationRollsBackMismatchedTask`、`control/TestSameKeyUnfinishedWorkSurvivesCompletion` |
| 完成检查前后 | 无关输出保留验证；相关部分写入即使失败仍阻止完成；修复并重验可解除；新增效果或损坏证据不能沿用先前的通过 | `httpapi/TestCompletionPreflightMatchesFinalEvidence`、`httpapi/TestCompletionPreflightRecoveryAndNewEffects`、`verification/TestDeclaredInputsBoundInvalidation` |
| 提交后通知前 | 已提交后通知失败：只修复投递；重放提交不产生第二份结果，工具不重开，待消费输入与 `waiting_user` 原样保留 | `delivery/TestNotificationFailurePreservesCommittedRunAndToolOutcome` |
| 投递效果不确定 | `sent_unconfirmed` 不盲目重发，入站触发补投仍有界 | `delivery/TestUnconfirmedDeliveryIsTerminalButDistinct`、`delivery/TestCatchUpStillUnconfirmedNeverLoops` |

类型链另覆盖成功/非零退出、参数转换、进程未启动、脱敏、外部伪造结果、旧 backend
未知事实与 Run 身份冲突。`reliability/verification-pipeline-failure.yaml` 从生产派发
事件检查观察到的非零退出，且 Run 仍为可恢复的 `verification_partial`。
`reliability/verification-independent-report.yaml` 使用真实 provider 录制，检查验证
源输入之后写独立报告能完成，且不为报告重复执行源检查；
`reliability/verification-recheck-closed-unit.yaml` 录制工作单元关闭后变更输入并
替代重验的闭环。相关输入改变必须阻止直接完成的反向约束由运行时注入测试覆盖。
单一 provider 的记录不代表跨模型覆盖。

### 14.2 验证义务与检查方法

Main 通过扁平的 `verify.criterion`、`target`、`local_dependencies` 声明检查；旧版
`check` 对象只在工具边界迁移已发布字段，未知字段仍会拒绝。`replaces` 与
`reason` 引用同一 Run 的原检查：标准、目标与 cwd 不变，省略的依赖继承；若
纠正后的观察方法读取不同输入，Main 可显式声明新的依赖集合并保留原因。历史无
绑定证据不自动获得替换权限。公开接口不要求 Main 复制 `step_id`；运行时优先
绑定活动的 `verification_required` 步骤；若活动步骤是诊断步骤，则只在同一工作
单元内按持久计划顺序选择最早的 pending 验证义务，不跨越下一个工作单元，也不
匹配模型文本。替代重验在原步骤仍属于活动工作单元时保留该步骤身份，即使 Main
改了状态；原步骤被重新规划移除，或其工作单元已冻结时，声明的重验才归属新的
活动 required 步骤，并且不改写历史工作单元。运行时不猜测一次检查在语义上
覆盖哪些其他步骤。

`verification_required` 表示该步骤声称一个独立验收条件已经通过，并要求成功的
验证证据。诊断性、允许失败的观察不会设置它；失败仍作为证据保留。同一个检查
是工作步骤的证据，不应再建第二个 required 步骤来表示“运行检查”。
失败结果会要求保持原步骤进行中；成功替代会提示取消仅表示同一次重试的冗余
步骤，而不会把一份证据静默复制给另一个验证义务。

拒绝完成返回有界的阻塞证据 ID、标准、目标与 cwd。步骤归属与 Run 最终检查
均保留未解决失败。依赖路径始终以工具 cwd 为基准，不解释 shell 内部的 cd；
不存在或无法读取的依赖按所有本地变化保守失效，并向 Main 暴露解析问题。
这允许验证“目标应不存在”，但不能凭错误路径声明与真实输入无关。

本批回归包括 `httpapi/TestReadBeforeResumeUsesActualDispatcherEffectClassification`
的 artifact 读取与真实写入反向约束、`tools/TestMissingDependencyCannotClaimUnrelatedMutation`、
`control/TestVerificationResolutionAndExactBlocker` 的事务回滚、引用隔离和失败历史保留。
既有派发、部分效果、落库与通知故障矩阵继续共同执行。

## 15. 指标与埋点

| 指标 | 目标 | 2026-07-28 基线 |
|---|---|---|
沙箱/环境类失败率（`FailureClass ∈ {sandbox_fs_denied, credential_state_readonly, credential_missing}`） | < 1% | ≈ 5.5%（16 / 293） |
可避免的 host escape（`HostEscapeReason == sandbox_gap`） | < 5% | 无法计算，需先埋点 |
host 总占比（含 login/gui/host_write 类） | 单独统计 | 28%（83 / 293） |

新增 tool 事件字段：`profiles_matched`、`recovery_attempted`、
`recovery_outcome`、`host_escape_reason`、`scratch_bytes`。

没有 `host_escape_reason`，"可避免的 host escape" 无法度量，只能得到混在一起
的 28%。

## 16. 与既有不变量的一致性

程序集识别区分被执行的程序与数据参数：普通 `find` 遍历、`go test ./...` 或读取
脚本文件不因为路径或文件后缀而加载主机凭据库存；解释器脚本、实际执行的脚本路径和
`find -exec/-execdir/-ok/-okdir` 仍保留不透明执行的环境要求。确实需要挂载状态而
后端无法兑现时，隔离请求以类型化 preparation/not_dispatched 失败，并保留不支持的
要求及未产生效果的事实；不能丢弃挂载要求或改为并行 host 执行来伪造成功。

| 不变量（`AGENTS.md` / `docs/tool-safety.md`） | 结论 |
|---|---|
每个工具子进程都经 `BuildProcessEnv` 构造环境 | ✅ 保留为 snapshot 构造时的唯一过滤器，不绕过 |
执行状态只存引用与策略，不存原始凭据字节 | ✅ snapshot ID / generation / 变量名摘要 / 三指纹入库，值只在内存 |
沙箱可写视图由 `ExecutionScope` + 数据驱动的 profile 元数据 + 通用平台约定派生；不加 per-vendor 分支 | ✅ 引擎只认五原语；vendor 细节全在 catalog 数据表 |
`kernel` 不依赖 gateway 包或具体工具 | ✅ `EnvironmentRegistry` 放 `executionenv`，不引 gateway |
安全硬底线先于审批模式与授权，不可被绕过 | ✅ L1 下限表是同一性质的补充（约束 L2 能持久化什么） |
审批拒绝不触发自动重试 | ✅ §11.2 保持 |
命令元数据跨端唯一，来自 `internal/gateway/command` | ✅ `/approvals` 注册进目录 |
用户可见控制与提示为英文 | ✅ 提示与命令输出英文；本设计文档为中文 |
可重复的消息路径缺陷需要 eval case | 第 3、5 步各补一条 `evalcases/**`（gcloud 隔离首次成功、watch 不再静默失败） |

## 17. 未决事项

1. **`write_back`**：本轮只留协议位置，不实现。将来必须带源文件指纹、冲突
   检测与原子提交。
2. **scratch 配额策略**：软阈值 2 GB，不强杀当前命令，超限后阻止下一次执行。
3. **历史授权撤销范围**：按 §11.3 的下限表，建议撤销 `set`、`for`、`python3`、
   `git`、`find` 五条，加 1 条 2026-07-18 缺 resource fingerprint 的旧行，共
   6 条；保留 `aws`、`kubectl`、`gcloud`、`gh`、`argocd` 五条真实凭据 CLI（第
   3 步完成后它们大多不再需要 host）。
4. **`approval_grants` 加期限的行为变化**：person 级 host 授权从"永久"变为
   8 小时，代价是长期项目每天多问一次。
5. **catalog 覆盖机制**：P0 不提供。将来若确有需求（企业自定义内部 CLI），走
   独立文件 `~/.selfmind/envprofiles/*.yaml` + 显式加载，**绝不进 `config.yaml`**。
6. **文档落点**：本文档实施后，`docs/tool-safety.md` 增加一节 "Execution
   engine" 指向本文档并固化 §4 的三层分工与跨层规则；`docs/STATUS.md` 增加
   对应进度行。

## 附录 A. 实验复现命令

```sh
# E1 状态目录体积
du -sh ~/.config/gcloud; du -sh ~/.config/gcloud/* | sort -rh | head

# E2 最小复制 + 覆盖层（gcloud / GKE 首次成功，宿主不变）
L=~/.selfmind-exp/lease1; mkdir -p $L/tmp $L/state/gcloud; chmod 700 $L $L/tmp
for f in credentials.db access_tokens.db default_configs.db \
         hidden_gcloud_config_universe_descriptor_data_cache_configs.db \
         active_config config_sentinel; do
  cp -a ~/.config/gcloud/$f $L/state/gcloud/ 2>/dev/null
done
cp -a ~/.config/gcloud/configurations ~/.config/gcloud/legacy_credentials $L/state/gcloud/
mkdir -p $L/state/gcloud/logs
bwrap --die-with-parent --unshare-pid --ro-bind / / --dev /dev --proc /proc \
  --bind $L/tmp $L/tmp --bind $L/tmp /tmp --bind $L/state $L/state \
  -- env -i HOME=$HOME PATH=$HOME/google-cloud-sdk/bin:/usr/bin:/bin \
     CLOUDSDK_CONFIG=$L/state/gcloud KUBECONFIG=$HOME/.kube/config \
  sh -c 'gcloud auth list --filter=status:ACTIVE --format="value(account)";
         gcloud auth print-access-token >/dev/null && echo TOKEN_OK;
         kubectl --context <gke-context> get ns | head -3'

# E4 双绑定与遮蔽（lease 必须不在 /tmp 下）
#   预期：lease 在 $HOME 下 → 两路径同 inode；lease 在 /tmp 下 → Directory nonexistent

# E5 overlay 不可用
bwrap --overlay-src /tmp --tmp-overlay / -- true   # → bwrap: Unknown option --overlay-src

# E6 只读 GOCACHE
bwrap --die-with-parent --unshare-pid --ro-bind / / --dev /dev --proc /proc --tmpfs /tmp \
  --bind <repo> <repo> -- env -i HOME=$HOME PATH=/usr/local/go/bin:/usr/bin:/bin GOWORK=off \
  sh -c 'touch "$(go env GOCACHE)/probe"; go build ./... ; echo exit=$?'
```

## 附录 B. codex 参照索引

| 主题 | codex 位置 |
|---|---|
文件系统策略条目集与特殊路径 | `codex-rs/protocol/src/permissions.rs:133`、`:570`、`:990` |
可写根内的只读挖洞（含未创建路径的合成挂载） | `permissions.rs:1592`、`linux-sandbox/src/bwrap.rs:1085-1110` |
bwrap argv 构造（2751 行）与网络三态 | `linux-sandbox/src/bwrap.rs`、`:87` |
平台默认只读根 | `bwrap.rs:44` |
沙箱拒绝判定 | `sandboxing/src/denial.rs:6` |
环境变量策略与 core 白名单 | `protocol/src/shell_environment.rs:52`、`protocol/src/config_types.rs:213` |
把策略渲染进模型上下文 | `core/src/context/environment_context.rs` |
两级升级与一次升级重试 | `prompts/templates/permissions/approval_policy/on_request_rule_request_permission.md`、`core/src/tools/orchestrator.rs:293-340` |
禁止授权前缀（L1 下限） | `core/src/exec_policy.rs:53` |
用户规则的独立存储（L2） | `core/src/exec_policy.rs:841`（`~/.codex/rules/default.rules`） |
配置面仅三键（L3） | `config/src/config_toml.rs:182`、`:195`、`:198` |
持久会话（本轮不做） | `core/src/unified_exec/` |

## 附录 C. 评审后的修复(2026-07-29 第二轮)

外部评审指出 1 个高风险缺口与 4 个兼容性问题,全部核实成立并已修复:

| 编号 | 问题 | 修复 |
|---|---|---|
**C-1(P0)** | 耐久 watch 未持久化环境身份,重启后可能**静默换账号执行**。watch 是生命周期最长、最可能跨越重启的执行路径,而失败模式不是报错——是对着**错误的账号/项目**返回"成功" | `external_watches` 增 `environment_snapshot_id`/`generation`/三指纹(均为非敏感);注册时记录,**每次检查前**比对,不一致即以 `environment_changed` 结束并发事件,不再执行。旧 watch 无指纹 → 放行(不能因为一个从未记录过的属性把已开始的工作搁死) |
**C-2(P1)** | `selfmind env refresh` 采样后只打印,不应用;而且提示"运行 gateway restart 采纳"是**错的**——restart 继承的是 CLI 自己的环境,正是要替换的那个 | `StartOptions.Environment` 让 restart 用给定环境启动;`env refresh --restart` 把采样**直接**交给新守护进程(绝不落盘——环境文件会持久化只应存在于内存的凭据值);不带 `--restart` 时如实说明两个命令的区别。macOS 上 launchd plist 固定环境,额外提示需重新 `service install`,并让 plist 透传代理/配置位置等**非凭据**变量(plist 是 0644,凭据形状的名字与内嵌凭据的值一律不写) |
**C-3(P1)** | `background:true` 绕过执行引擎,拿不到 run 的环境绑定、scratch、profile 与证据。同一 run 的前台 gcloud 成功、后台 gcloud 可能失败 | `StartProcess` 接受环境参数;后台分支走同一 `execMaterialForArgs`,并记录 `tool.environment` 与 `tool.sandbox`(含 host escape 原因)。仍要求 `sandbox=host` + 审批 |
**C-4(P1)** | gh profile 设的是 `GH_STATE_DIR`,而 gh 读的是 `GH_CONFIG_DIR`——重定向是空操作,gh 仍写只读宿主配置 | 实测确认(`gh help environment` 无 `GH_STATE_DIR`;`GH_CONFIG_DIR=$T gh config set` 确实写入 `$T`)。改为 `copy_in` + 重定向 `GH_CONFIG_DIR` 到**可写**副本(gh 刷新 token 会重写 `hosts.yml`) |
**C-5(P1)** | 所有 kubectl/helm 被当成 GKE 并强制依赖 gcloud,影响 EKS/AKS/本地集群,且无谓扩大凭据暴露面 | 拆成通用 `kubernetes` profile;新增通用的 `ConditionalRequire` 原语(按配置文件标记条件引入依赖),按 kubeconfig 的 exec plugin 决定是否引入 gcloud/aws。标记是启发式,失败方向安全:漏判 = 该助手状态未准备(等同旧行为),绝不会用错凭据 |
**C-6(P2)** | `SandboxPlan` 的 snapshot/generation/scratch 身份字段在生产路径上为空;`ExecutionRequest`/`ExecutionResult` 未真正统一调用 | `execMaterial` 携带真实身份,`planFromMaterial` 从中读取(测试断言不得为空)。`terminal`/`verify`/`execute_code`/`watch_external`/`background` 现在共用同一份材料构造与同一后端接口 |

一个自我修正:上一轮把步骤 4 判为"实质已达成"是错的。类型定义好而调用方未真正统一,等于把接缝画在纸上——**C-1 能溜过去正是这个结构原因**:如果 watch 与 run 早就共用同一个入口,watch 就不可能漏掉指纹校验。

## 附录 D. 第三轮复查修复(2026-07-29)

| 编号 | 问题 | 修复 |
|---|---|---|
**D-1(P1)** | `env refresh` 拿 **CLI 自己的环境**作比较基线。而 CLI 通常是最先看到新工具链的进程,所以"新 shell + 旧 daemon"从 CLI 看完全一样 → 输出 `unchanged` 并直接返回,恰好在该命令唯一有用的场景下什么都不做。另外 macOS launchd 模式走 `gatewayServiceRestartIfInstalled`,**忽略采样却返回成功** | 基线改为**运行中的 daemon**:`/v1/gateway/status` 增加 `environment_generation` 与三指纹(仅指纹,无变量名/值),CLI 与之比对;拿不到 daemon 身份时按"新"处理(拒绝在信息缺失时行动会让操作者无路可走);`--restart` 是显式指令,即使无差异也照做(daemon 可能因无关原因卡住)。**launchd 模式明确拒绝**并给出真实路径(`gateway service install` 重写服务定义),不再假装已采纳 |
**D-2(P1)** | helm 挂在 kubernetes profile 下,只重定向了 `KUBECACHEDIR`。helm 还写 `HELM_CACHE_HOME`(chart 归档与 repo 索引)、`HELM_CONFIG_HOME`(`repositories.yaml`、`registry/config.json`)。**这正是最初实测到的失败**:`open ~/.cache/helm/repository/argo-cd-9.2.1.tgz: read-only file system` | 独立 `helm` profile,依赖通用 `kubernetes`:`HELM_CACHE_HOME` → person 级持久缓存(重下载昂贵、无凭据语义);`HELM_CONFIG_HOME` → 有界 `copy_in` + 重定向到可写副本(`helm repo add`/`registry login` 会重写它)。`HELM_DATA_HOME` **故意不重定向**——重定向会让操作者已装的插件消失,而它只被 `helm plugin install` 写,那是个应走 host 审批的显式动作 |
**D-3(P2)** | `ExecutionRequest`/`ExecutionResult` 只是类型,生产代码不构造也不消费 | 新增真正的 `Execute(ctx, ExecutionRequest, args) (ExecutionResult, error)`,**terminal / verify / execute_code / watch_external 四条路径全部经它**;`executeForegroundCommand` 退化为参数映射适配器。加一条机械断言:除 `Execute` 外任何文件都不得构造沙箱命令(`TestExecuteIsTheOnlySandboxConstructionSite`),并测试三种请求形态(shell / argv / durable)。`background:true` 仍不经 `Execute`——它是 fire-and-forget,没有流式输出与退出码可返回——但共用同一份 material,这一点在文档里如实说明,不再笼统称"全部统一" |

`args` 现在只是**派发侧信封**(scope 查找、审批中间件回写的字段、事件 id);沙箱本身需要的一切都在请求里。这是"引擎可搬到 Runner"的实际判据:远端节点收一个 request、回一个 result,调用方不变。

## 附录 E. 自查修复(2026-07-30)

自查发现 11 项欠账,全部修完。前三项是"文档承诺了但代码做不到"或真实失败类:

| 编号 | 问题 | 修复 |
|---|---|---|
**E-1** | `credential:read` **只被读取、从未被授予**:全仓只有常量定义和一处读取,没有任何审批入口。文档写的"untrusted 需要凭据时申请 credential:read"**做不到**,实际只能 `ws trust` 整个 workspace —— 正是这个能力想避免的 all-or-nothing 提权 | 与 `network:shared` 同构的完整路径:`resolveCredentialCapability` 在**执行前**决策(失败后再问会让工具先报一个不存在的登录问题),仅在"untrusted + 程序集命中 operator profile + 无现存授权"时询问;拒绝**不是错误**——命令照跑、覆盖层为空、工具报自己的"未登录",决定权留给人 |
**E-2** | AWS SSO 的 token cache 没覆盖。SSO 登录态在 `~/.aws/sso/cache`,每次刷新都要**写**,而该路径由 `HOME` 派生、**不是** `AWS_CONFIG_FILE`,所以任何 env 重定向都搬不动它 | 新增通用原语 **`map_rw_at`**:把可写状态目录 bind 到宿主路径**之上**(宿主不变,遮蔽只在沙箱内可见),并用 `copy_in` seed 使既有 token 仍可用。host 模式无 mount namespace,此原语无效,工具用真实目录——与既有行为一致 |
**E-3** | `KUBECONFIG` 支持冒号分隔多路径,而 `resolveSource` 当单路径处理 → `map_ro` 挂到不存在的路径(`--bind-try` 静默跳过),conditional-require 也读不到 → GKE/EKS 助手**不会被引入** | `StateSource.List` 标记 + `resolveSources`;`map_ro` 挂每一项,标记扫描每一项(第二个 kubeconfig 里的 marker 同样决定性) |
**E-4** | `ExecutionScope.SandboxPolicy` 生产代码 **0 处填充**,"每请求策略"是装饰性接缝 | `installExecutionScope` 用 `CurrentExecSandboxPolicy()` 快照填充,请求自描述 |
**E-5** | scratch 只在 boot 清理一次。长期不重启的 daemon 永不回收——per-lease 软阈值救不了(100 个已结束的 run 各占 1 GiB 也不触发) | 接入既有 60s sweep;活跃 run 的 lease 与未终结 watch 的 id 一律豁免(目录的年龄不代表 run 是否还在写) |
**E-6** | 配额检查在每条命令前全树遍历,代价与该 run 自己的输出量成正比 | `ScratchBytesCached`(30s),清理时失效 |
**E-7** | 搬到 backend 时丢了可写根去重,且 request 的 roots 会与 scope 的重复叠加 | 在 `planFromMaterial` 统一去重(plan 里出现重复挂载看起来就像 bug) |
**E-8** | background 无超时、无上限:run 结束后再没有任何东西会停它,一个卡住的进程会占着进程、scratch 与复制出来的凭据状态直到 daemon 重启 | 加 ceiling(显式 timeout 优先,long-running 用其上限,默认 2h)+ 进程被 reap 时关闭 watchdog;并记录 plan(snapshot/generation/scratch handle)作为证据。它仍不经 `Execute`——detached 进程没有流式输出与退出码可返回——文档如实说明 |
**E-9** | 计划里承诺的两条 eval case 一条都没加 | 补 `execution-run-scratch-continuity.yaml`(跨命令交接 + 拒绝把状态写拒绝分类成 auth)与 `execution-state-overlay-first-try.yaml`(状态目录可写、不得出现误导文案)。**写完自查又发现 `disallowed_error_category` 被我放在 `checks:` 下会被静默忽略**,已移到 `expect:` 并用解析校验确认生效 |
**E-10** | `docs/tool-safety.md` 落后两轮 | 补"One execution entry / Tool environment profiles / Durable execution identity"三节;并改掉把 `credential:read` 说成"计划中"的过时表述 |
**E-11** | 79 个文件未提交 | 见提交记录 |
