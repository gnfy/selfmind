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
  → SandboxBackend          （bwrap | macOS host）
  → ProcessRunner
  → FailureClassifier
  → RecoveryPolicy          （至多一次）
  → ExecutionResult
```

### 5.2 类型边界

```go
// 可序列化、版本化。未来直接是 Runner 协议载荷。
type SandboxPlan struct {
    Version       int      `json:"version"`
    SnapshotID    string   `json:"snapshot_id"`
    Generation    int64    `json:"generation"`
    ReadOnlyRoots []string `json:"read_only_roots,omitempty"`
    WritableRoots []string `json:"writable_roots"`
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
}

type ExecutionResult struct {
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
`map_rw_at` | 把 lease state 的可写目录绑定到不可配置的宿主路径上 | ✅ |
`synthesize_dir` | 在声明的 state root 上挂可写 tmpfs，仅保留声明的子项可读 | ✅ 2026-07-30 |
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
`network_denied` | 沿用 `network:shared` 审批，授权后**显式**重跑（保持 `execution_capability.go:78` 的不重放立场） |
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

### P1：可观测性（第 9 步）

`/diag execution` 增加：当前 lease 与 snapshot generation、snapshot 年龄、
命中的 ToolEnvironmentProfile、sandbox backend、run scratch 是否有效与体积、
network/credential capability、最近失败分类与恢复结果、host escape 次数与
原因分布。**不显示环境变量名、值、凭证路径与命令正文。**

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
