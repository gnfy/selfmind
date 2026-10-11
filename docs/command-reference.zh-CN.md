# SelfMind 命令参考

[English](command-reference.md)

这是面向用户的完整命令索引。已安装版本自带的帮助可通过
`selfmind --help`、`selfmind <command> --help` 或 `/help` 查看。

## 全局与生命周期命令

```text
selfmind [--config PATH] [--resume SESSION_ID] [--add-dir DIR]...
selfmind --version
selfmind setup [--non-interactive] [--skip-model] [--skip-gateway] [--check-model]
selfmind update [check] [--channel latest|next] [--force] [--no-restart]
selfmind uninstall --prepare [--purge-data --yes]
selfmind feedback [--out FILE|--send] [--repo OWNER/REPO] [--include-crash] <message>
```

- 直接运行 `selfmind` 打开 TUI。首次在交互式终端启动时，如果缺少“模型就绪”，
  会打开唯一的 Model Manager；后续启动再恢复 Runtime/首次使用设置。Linux 与
  macOS 使用同一流程；
  脚本、cron 和管道等非交互场景不会弹出提示，而会输出可执行的 `setup`
  命令。`--config` 指定其他配置文件，`--resume` 恢复已有 TUI
  会话。可重复使用 `--add-dir`，仅为本次 CLI 调用授予另一个本地目录的访问权，
  不会修改已登记的 workspace。
- `setup` 创建缺失的配置，但只负责 Runtime 与首次使用状态。缺少“模型就绪”时，
  它只引导用户运行 `selfmind model`，不会展示第二套选择或探测流程。Model Manager
  展示主模型、后台模型及可选角色覆盖，并自动校验每个完成的选择。随后 setup 确认
  项目工作区及其信任边界、审批模式和后台运行方式；Runtime 步骤失败后只恢复该
  步骤，不会重复或再次确认模型工作。安装后的 daemon 会从自己的运行环境验证所选
  路由，成功后才记录“模型就绪”。用户确认使用的 shell 环境 API 凭据会复制到
  SelfMind 私有凭据文件，绝不会写进操作系统服务定义。工作区不会静默使用 `/` 或
  用户主目录。macOS 的 launchd 与 Linux 的用户级 systemd 只是内部实现。
  托管就绪要求运行中的服务 job 与 Gateway 报告相同的非敏感 service generation、
  版本和配置。存在活动任务的兼容 Gateway 可以保持 Runtime Degraded，由系统延期
  修复服务。TUI 中首个成功的非命令任务会完成独立的首次使用回执。
  各类 `skip` 参数仍可用于受控自动化环境。
- `gateway service status` 会区分 absent、starting、managed healthy、managed
  unhealthy、conflicting/unowned 和 Runtime Degraded。只有 service generation、
  版本、配置身份及不含秘密的有效路由指纹与响应中的 Gateway 和当前安装一致时，
  运行中的 job 才算健康。
- `update check` 只检查更新。`update` 执行完整升级：检查所选 npm 通道、
  调用包管理器安装、用 `selfmind --version` 验证新二进制，并对运行中的
  gateway daemon 做默认排空（drain）重启以切换到新版本。同版本也会重新
  安装，用于恢复本地开发过程中临时替换过的 npm 包内容；若当前构建比所选
  通道更新，则默认不降级。`--force` 允许显式降级，也会在检查失败时继续安装；
  `--no-restart` 跳过重启（daemon
  在手动重启前继续运行旧版本）。`update` 绝不会拉起未运行的 daemon。
  渠道默认为 `auto`：预发布版本自动跟随 `next`，正式版本跟随 `latest`；
  在配置中显式写 `updates.channel: latest|next` 可固定某条线。`--channel`
  只影响当次执行，绝不改写配置中的固定值。
  从 npm 包启动的开发替换版可直接用 `selfmind update` 恢复；独立源码构建仍需
  显式使用 `--force`，SelfMind 才会覆盖它。
- `uninstall --prepare` 停止并注销 daemon。只有显式指定
  `--purge-data --yes` 才会删除数据。
- `feedback` 默认在本地生成脱敏诊断报告。`--send` 通过已登录的 `gh`
  创建 GitHub Issue；`--repo` 覆盖目标仓库，`--include-crash` 附加最近一次
  本地崩溃报告。

## 日常 daemon 客户端命令

以下命令与 TUI、IM 渠道连接同一个 gateway。

```text
selfmind send [--async] [--mode MODE] [--add-dir DIR]... <message>
selfmind status
selfmind watchers [active|attention|recent|all [page]|<n|id>|cancel <n|id>]
selfmind resume [n|run_id]
selfmind search [query]
selfmind ws [<n|workspace_id>|default <n|id>|add|trust|untrust|grants|observe|effect|revoke] ...
selfmind approvals
selfmind approve [token]
selfmind reject [token]
selfmind stop [n|run_id]
selfmind id
selfmind new [title]
selfmind cron adopt <job_id> [--yes]
```

- `watchers` 是不调用模型的持久 watcher 管理入口。默认优先显示运行中和需要
  关注的检查；可使用稳定的短 watcher ID 查看详情或只取消该 watcher。取消
  watcher 不会取消外部系统中已经启动的操作。

- `send` 在不打开 TUI 的情况下提交消息。`--async` 在 gateway 接收后立即返回。
  `MODE` 可用值为 `on-request`、`read-only`、`auto-edit`、`full-auto`
  或 `smart`。未保存个人偏好时默认使用 `smart`；未配置裁决模型时会安全
  降级为人工审批。
- `--add-dir` 可重复使用，且只允许你自己的 CLI 通过本机 gateway 的本地控制
  socket 使用（见下文“CLI 如何连接 gateway”）。相对路径以启动 SelfMind 的 shell 为基准解析；路径会被规范化，最多八个，
  并冻结到排队的 run 中，同时参与项目指令发现和文件系统冲突调度。它只扩大该 run
  的文件系统范围，不会授予 workspace 信任、绕过审批或沙箱，也不会修改持久
  workspace。运行中的 turn 不能改变附加目录集合；需要不同目录时应新开 run。
  若消息正文要原样包含 `--add-dir`，请先写 `--`。
- `tasks` 默认列出当前 Attention：运行中的 Run、待处理审批与澄清、活动
  watcher，以及仍是所属 Thread 最新 Run 的未认领可恢复 Run（`interrupted`
  的 Run 只有留下工作证据时才显示）。同 channel 的条目优先。settled、
  archived、all 和关键词视图用来查看 Thread 历史，不会因此重新打开它。
- `task` 支持列表序号或稳定的 Thread 兼容 task ID。序号绑定当前端点最近展示的
  快照；即使后台活动随后改变实时排序，也不会指向另一个 Run。
- `task ... complete` 只消除该精确 Run 的 Attention，不改写 Run 历史；若该 Run
  仍有待处理审批、待处理澄清或活动 watcher 则拒绝执行——应先回答、拒绝或取消
  这些对象。`task ... archive` 只从普通界面隐藏 Thread。显式 `resume` 仍可继续
  可恢复 Run，并重新显示已归档 Thread。
- `resume` 接受列表序号、稳定的 Thread 兼容 task id 或完整 run id。序号解析为
  该端点最近展示快照中的精确 Run；Thread id 只在该 Thread 恰有一个未解决 Run
  时被接受。
- `ws` 就是工作区操作的全部入口。不再有 workspace、workspaces、ws list
  或 ws use 这些写法：不带参数即列出，直接给序号或 id 即选中。

```text
selfmind ws
selfmind ws <n|workspace_id>
selfmind ws default <n|workspace_id>
selfmind ws add [path] [name...]
selfmind ws trust [workspace_id]
selfmind ws untrust [workspace_id]
selfmind ws grants [workspace_id]
selfmind ws observe <script> [--network] [--credentials] [--all-args | -- <argv-prefix...>] [--workspace <id>]
selfmind ws effect <script> --target <kind:id> [--target <kind:id>...] [--observe-command <exact-read-only-script-command>] [--cwd <dir>] [--env NAME=VALUE...] [--stdin <text> | --stdin-file <path>] [--network] [--credentials] [--workspace <id>] [-- <exact-script-args...>]
selfmind ws revoke <capability> [workspace_id]
```

- 选中工作区只对当前会话生效。`ws default` 设置持久默认值，供 IM 与定时任务
  使用——这类回合本身没有所属目录。本地 CLI 或 TUI 在某个目录里启动时，无论
  默认值为何，都仍然使用该目录对应的工作区。

- 只有你自己的 CLI 通过本地控制 socket 才能修改工作区信任状态。省略 workspace ID 时操作当前工作区；
  `untrust` 还会撤销该工作区当前有效的执行能力授权。
- `observe` 将可信工作区内的一份脚本登记为只读观测脚本。授权同时绑定工作区、
  解析后的脚本路径、内容哈希、参数形状、网络和凭证选择；`--network` 登记只匹配实际
  带网络运行的调用，规则见 `effect`。脚本被修改或替换后授权自动失效。只有确认所有
  参数都只读时才使用 `--all-args`，否则在 `--` 后给出允许的参数前缀。
- `effect` 由本地用户对某一条精确脚本调用声明完整的外部目标集合。并行模式下，
  匹配的调用占用这些目标，而不是全人未知目标。脚本内容、工作区、全部参数、网络
  和凭证模式都必须相同，调用的其余部分也必须相同：运行目录（`--cwd`，相对工作区，
  默认工作区根目录）、写在脚本前的每个 `NAME=VALUE` 赋值（`--env`），以及唯一的
  标准输入——here-string（`--stdin`）或用 `<` 重定向的工作区文件（`--stdin-file`，
  同时绑定文件内容）。脚本路径和运行目录既按写出的形式也按解析后的实际路径匹配，
  因此二者的符号链接别名都算另一次调用。`--network` 登记的调用只匹配实际带网络运行的
  调用：脚本名看不出访问远程服务时，调用要设置 `network: true`，因为网络授权或信任
  只是许可，并不会让每条命令联网。脚本必须直接执行（例如 `./scripts/deploy.sh`），不能换
  解释器。它不代替执行审批，正常审批照常进行。未绑定 `--observe-command` 时，
  匹配的调用以退出码 0 返回即像普通命令一样释放占用。可用 `--observe-command`
  绑定观察同一目标集合的精确脚本命令，同时用 `ws observe` 将该脚本登记为只读；
  此时占用保留，直到同一 Run 或其后续精确续接 Run 在效果派发后创建的该命令的
  `status_json.v1` watcher 成功且留下持久事件，运行时在两个授权与脚本哈希
  仍有效的前提下自动解除精确占用。变更、含糊或未登记的调用回退到未知目标通道。
- 同时存在多个审批时，`approve` 和 `reject` 可接审批 token。
- `stop` 取消活跃 run；没有活跃 run 时只 dismiss 精确 pinned 的 Run，且在该 Run
  仍有待处理审批、澄清或活动 watcher 时拒绝。`new` 创建新的可见任务。
- `cron adopt` 显示一个由旧版本保存、无法证明由谁创建的定时任务（它的 prompt 和
  运行时间）；加上 `--yes` 时记录它属于你，此后以你的身份运行。只有你自己的 CLI
  通过本地控制 socket 才能认领定时任务；可证明由他人创建的任务不能认领。
  `selfmind doctor` 会列出这类任务。

### CLI 如何连接 gateway

daemon 只把 CLI 身份授予你自己的终端。`selfmind` 与 TUI 通过本地控制 socket
连接本机 gateway（gateway 运行时目录中的 `control.sock`；该路径对 socket 地址
过长时，改用 `/tmp` 下按用户划分的目录），该目录只有你的操作系统用户可以进入。
daemon 依据操作系统记录的“谁发起了连接”来验证每个客户端：进程必须以你的身份
运行，且不能是 gateway 本身或 gateway 启动的进程，因此 agent 运行的工具永远无法
冒充你的 CLI。

- 所有以你的身份执行的操作都走这条通道，且不会回退到 TCP：发送消息、审批
  （`approve`、`reject`）、模式切换、工作区信任与授权、`--add-dir`、模型变更、
  `cron adopt`，以及 TUI 的事件流。通过 TCP 声明 `cli` 平台的请求会被拒绝，
  配置的 `gateway.token` 也不会授予该身份。
- `selfmind gateway status` 与 `selfmind gateway stop` 同样经这个 socket 访问本机
  gateway；早于该 socket 的 gateway 则走 TCP。未设置 `gateway.token` 时，gateway
  会拒绝 TCP 上的停止请求并指明该 socket，并且在 TCP 上只报告自己是运行中还是排空
  中、有多少 run 在活动以及构建信息；完整状态与 `stop` 需要你自己的终端或该 token。
  这样读到的状态会标明 `liveness only`，并说明其余信息在哪里。
- 指向远程 gateway（`SELF_GATEWAY_URL` 或 `gateway.url` 指向非 loopback 主机）的
  CLI 无法在那里以你的身份行动；请改为指向本机的 gateway。通过 TCP 的集成保留
  各自的认证，并以其声明的非 CLI 身份行动。
- 早于本地控制 socket 的 gateway 仍能回答健康检查，但无法服务 CLI：命令会说明
  这一点并指向 `selfmind gateway restart`（对旧 gateway 同样有效）。
  `selfmind gateway status` 会显示该 socket，`selfmind doctor` 会报告通道状态以及
  当前终端是否被接受（`[CONTROL]` 分类）。请在你自己的终端运行 `selfmind`：在
  gateway 启动的工具内部运行时会被拒绝。

## 配置、模型与渠道

```text
selfmind config [doctor|upgrade]
selfmind env [show|refresh]
selfmind model
selfmind prompt [list|show|edit|diff|validate|test|reset|apply] ...
selfmind auth [login|status|logout] ...
selfmind doctor [--verbose] [--out FILE] [--probe-models]
selfmind usage
selfmind report daily [--since 24h]
selfmind docs [check|index]
selfmind selfcheck [--fast | --profile local-full|local-fast|ci] [--skip-go] [--skip-eval] [--eval-dir DIR]
selfmind gateway [run|start|status|stop|restart|service] ...
selfmind weixin [login|status] ...
```

详细语法：

```text
selfmind prompt [list|status]
selfmind prompt show <agent|role>
selfmind prompt edit <agent|role>
selfmind prompt diff [agent|role]
selfmind prompt validate
selfmind prompt test [agent|role]
selfmind prompt reset <agent|role|all>
selfmind prompt apply

selfmind auth login minimax-oauth [--region global|cn] [--no-browser]
selfmind auth status [provider]
selfmind auth logout minimax-oauth

selfmind gateway run [--replace] [--addr ADDR]
selfmind gateway start [--replace]
selfmind gateway status [--json]
selfmind gateway stop [--force]
selfmind gateway restart [--drain] [--force]
selfmind gateway service [install|status|uninstall]

selfmind weixin login [--timeout 8m] [--owner-person-id ID] [--no-bind] [--no-enable]
selfmind weixin status
```

- `docs check` 检查源码文档契约：UTF-8、清单完整性、本地链接、中英文源
  哈希、文档体积和活跃方案生命周期。`docs index` 根据
  `docs/manifest.yaml` 重新生成 `docs/README.md`，不会自动把过期译文标为
  已同步。
- `selfcheck` 始终执行文档契约，因此同时指定 `--skip-go --skip-eval`
  会执行一次有效的纯文档发布检查。

- `config doctor` 只报告配置缺失或过期项；`config upgrade` 在保留已有值的
  前提下补充受支持的默认项。
- `selfmind model` 是唯一的 CLI 模型命令；它与 TUI 中不带参数的 `/model`
  打开同一个 Model Manager。传入子命令会直接拒绝，避免出现第二条修改路径。
- 管理器显示 Main、Background、六个可选角色覆盖
  (`fast_classifier`、`memory_extract`、`background_review`、`skill_curator`、
  `semantic_recall`、`summarizer`) 以及变更/恢复状态。未覆盖的角色会明确显示
  “使用 Background”。只有 Main 可以负责完整的用户可见回合。
- 一次会话可同时编辑 Main、Background 和多个角色。每完成一个选择，daemon
  都会自动探测；不再提供单独的 verify 命令。探测失败只保留可编辑草稿，不改
  YAML。缺少 API key 时可在关闭回显的输入框中补充，并且只在探测成功后写入私有
  凭据文件。最终确认会提交一个不含密钥的原子快照，并且只安排一次安全重启。
- 已有 reasoning、service tier、context 和 YAML 专属 transport 字段会在兼容时
  保留；兼容性未知时只把受影响的可选项恢复为 `auto`。第一次 Main 选择可以初始化
  空的 Background；之后 Main 的修改不会覆盖 Background。
- 接受在线变更后会安排一次安全 daemon 重启。当前 run 继续使用冻结的旧路由；drain
  期间收到的新工作会排队给新 daemon。启动时再次探测候选项，并且只有 Agent 和
  `/health` 都健康后才提交。确定的模型不兼容会恢复上一次运行路由；基础设施或
  未知失败会进入 `recovery_required`。重试/恢复选项位于 Change status 页面；有界、
  不含密钥的历史以及 running/configured/pending 状态仍由管理器展示，不再暴露成
  独立命令。对于 DeepSeek V4 这类具有多轮思考/工具契约的 provider，自动校验还会
  回放一次无副作用工具结果并验证最终答复，因此会消耗少量 provider 额度。
- `prompt` 管理当前配置文件同级的 operator 工作区（通常是
  `~/.selfmind/prompts/`），不需要增加 `config.yaml` 配置项。`list`/`status`
  会比较磁盘快照与运行中 daemon 已加载的哈希和 build，因此相同的自定义哈希不会
  掩盖仍需重启才能生效的内置提示词变更；`show` 展示静态内置角色契约
  和本地文件，但不输出运行时 memory 或项目上下文；`edit` 为 `agent`、
  `memory_extract`、`background_review`、`skill_curator`、`summarizer` 或
  `semantic_recall` 创建带章节标记的模板；标记章节内部的 Markdown 标题会原样
  保留为自定义内容。编辑现有无标记文件时，会先迁移成标记语法，并写入带时间戳的
  `.legacy-*` 备份；Markdown 代码围栏内的精确标记示例仍按正文处理。
- 提示词章节内容为 `default` 时继承内置行为。只有明确可替换的前台章节允许
  使用 `off`；质量、验证、响应 schema、治理、工具和安全底线都不能关闭。
  未知章节标记（以及旧版无标记文件中的未知保留标题）、非法 UTF-8，以及 prompt
  root、嵌套目录或文件上的符号链接、组/其他用户可写权限和超限内容都会导致校验失败。
  启动时若活跃工作区无效，daemon 会写入
  持久的降级诊断，并使用匹配的最近一次有效快照；没有匹配快照时使用内置默认，
  因此 Agent 端点仍保持可用。对于报告宽松写权限的文件系统，也不会绕过安全检查。
  `reset` 会创建带时间戳的备份；`apply` 先校验再重启运行中的 daemon；显式执行
  `gateway restart` 也会在停止当前进程前先校验。不支持热重载；`test` 会确定性验证
  当前章节组合，不调用模型。
- `usage` 是当前 person 最近 24 小时的无模型执行与 token 报告（与
  `/report daily --since 24h` 使用同一数据路径）。若相应信号可用，它会展示
  provider 输入、缓存读取/未命中、输出、推理 token、原生工具 schema 占比、
  工具/审批活动以及当前维护与投递健康；它不估算货币成本。
- `report daily` 生成当前 person 的无模型质量与成本摘要。它在有界时间窗口内汇总
  run 结局、工具与审批、provider 用量和缓存、后台维护健康、投递状态及召回采用
  趋势；默认最近 24 小时，最长 30 天。事件会分页扫描到诊断上限；若仍有更多
  行，报告会把受影响的汇总明确标为“下界”，不会静默显示截断后的总数。
- `doctor` 默认只显示当前问题，并为每个问题给出恢复或排查命令；健康子系统以及
  历史 run、错误、事件、活动和日志默认隐藏。问题使用 `[CONFIG]`、`[TRUST]`、
  `[PROMPT]`、`[CONTROL]`、`[DELIVERY]`、`[SKILLS]` 等稳定分类标识，所有处理命令统一汇总到带编号的
  `Next actions`。交互终端复用 TUI 的警告、错误、信息、成功、正文和弱化色；管道、
  重定向、`NO_COLOR`、`CLICOLOR=0` 和 `TERM=dumb` 保持纯文本。`--verbose`
  打印完整的脱敏诊断包，`--out FILE` 始终把完整诊断包写入文件，便于支持或离线
  排查。`--probe-models` 会真实调用 provider，可能消耗额度。
- `selfcheck` 是本地发布门禁。默认 `local-full` 运行本机能够证明的全部发布用例；
  `--fast`/`local-fast` 跳过已测量的慢用例；`--profile ci` 只运行明确分配给
  当前平台 CI 的用例。Provider 响应离线回放，但工具调用仍使用当前主机工具链。
  缺少工具时默认以环境不可用失败；只有明确由当前平台 CI 负责的用例才会显示
  `CI-DEFERRED`，且发布仍要求对应 Action 通过。退出码：`0` 通过、`1` 回归失败、
  `2` 参数或环境不可用。
- `gateway restart` 默认等待安全的 turn 边界；`--force` 仅作为最后手段。
- `gateway service install` 在 macOS 上创建当前用户的 launchd LaunchAgent，
  在 Linux 上创建当前用户的 systemd unit。之后 `gateway start`、`stop`、
  `status`、`restart` 都操作这个稳定服务；`restart --drain` 会先等待活跃 turn
  到达安全边界，再由服务重新拉起 daemon。两者都不需要管理员权限。Linux 上若已有
  系统级 `selfmind.service` 正在运行，个人设置会明确报告冲突，不会停止或替换它。
  把启动权交给 launchd/systemd 后，安装流程只等待该受管进程，绝不会再启动 detached
  fallback 来掩盖服务 job 的失败。
- 如果微信 iLink 会话过期，重新运行 `selfmind weixin login`。正在运行的
  gateway 会监听账号凭据文件，在新凭据保存后自动恢复轮询，不需要重启
  daemon。
- `weixin login` 默认把扫码微信用户绑定到当前 CLI person，将私聊策略改成
  `allowlist`，并且只记录该发送者。`--owner-person-id` 可指定另一个已有
  person；`--no-bind` 用于明确保留现有的独立身份策略。

## Eval 与维护命令

```text
selfmind eval [list|run|report|repair|scorecard|capture|clean]
selfmind maintenance [replay|migrate-memory|migrate-skills|cleanup-person-partitions|prune-skill-candidate-refs|migrate-task-references|memory-audit|memory-dedup|task-audit|reset-work-history|restore-control] ...
```

```text
selfmind eval list [path]
selfmind eval run [case-or-dir] [--suite NAME] [--provider ID] [--model ID] [--tenant ID] [--workspace PATH] [--output PATH] [--record-content] [--live]
selfmind eval report <jsonl-or-dir>
selfmind eval repair [case-or-dir] [--worktree]
selfmind eval scorecard [case-or-dir] [--provider ID] [--out PATH] [--live]
selfmind eval capture [turn-id|latest] [--title "..."] [--suite NAME]
selfmind eval clean [--yes]

selfmind maintenance replay [--limit N] [--prompt-revision]
selfmind maintenance migrate-memory [--apply] [--data-dir DIR]
selfmind maintenance migrate-skills [--apply] [--root DIR] [--governance-grace 30d]
selfmind maintenance cleanup-person-partitions [--apply] [--root DIR --data-dir DIR]
selfmind maintenance prune-skill-candidate-refs [--apply] [--data-dir DIR]
selfmind maintenance migrate-task-references [--apply] [--limit N] [--data-dir DIR]
selfmind maintenance memory-audit [--archive-confirmed] [--partition P] [--data-dir DIR]
selfmind maintenance memory-dedup [--apply] [--partition P] [--data-dir DIR]
selfmind maintenance task-audit [--apply] [--limit N] [--data-dir DIR]
selfmind maintenance reset-work-history [--apply] [--data-dir DIR]
selfmind maintenance restore-control --backup PATH --yes [--data-dir DIR]
```

`maintenance replay` 会重新入队每个持久 key 最新 analyzer generation 中因重试耗尽
的 post-run 作业。停靠在 `blocked_prompt_revision` 的工作属于另一个由 operator 显式
触发的范围：该命令会报告停靠数量，`--prompt-revision` 才会重新入队它们。应先恢复缺失
的内容寻址 revision；若 revision 仍不可用就重放，作业不会调用模型，而会再次回到可见的
阻塞状态，并且其重试计数与已记录的原因会被重置。旧 generation 保留为不可变历史；处理
积压前应先用较小的 `--limit` 执行 canary。

`maintenance cleanup-person-partitions` 会把文件系统直属的 `person_*` 分区与
`control.db` 中全部 person 对照。默认只做 dry-run；应用清理前必须停止 gateway。
应用模式只会把已复核的孤儿移动到同一根目录下带时间戳的
`.orphaned-person-partitions` 隔离目录，不会删除数据。默认 root 与 control
database 来自同一份已加载配置；如果用 `--root` 指向另一棵资产目录，就必须用
`--data-dir` 明确指定权威 `control.db`。该命令不会打开或创建猜测出来的空数据库。

`maintenance prune-skill-candidate-refs` 默认只预览 owner work unit 已结束或
不存在的引用。`--apply` 只删除同一个事务查询选中的引用，排除仍存活的 work unit，
然后再次预览以验证结果为空。应结合 dry-run 输出中的 owner 行与
`selfmind doctor --verbose` 复核；不要直接在 SQLite 中删除 candidate ref。

- `task-audit` 是只读的 Thread/Run 连续性审计：升级回填未能转换的旧 resume
  边、非法 parent 边，以及失去归属的待处理 approval/clarify。`--apply` 仅作为
  兼容参数保留，不会自动改写执行历史。
- `migrate-task-references` 默认只做 dry-run。只有历史 `work_key` 的完整表面
  形式确实出现在该 run 的原始用户输入中时才允许迁移；从标题或摘要推断出的
  值只报告并跳过。`--apply` 可重复执行，且不会赋予工作区或执行权限。
- `reset-work-history` 默认只做 dry-run，并且只显示汇总数量。应用模式要求先
  停止 gateway；若存在运行中的 Run、活动 watcher 或已启动队列项则拒绝执行。
  命令会先创建并校验 SQLite 备份，再清理当前 tenant 的 Thread/Run 历史及依赖
  控制记录。已发布的 Skill 包保留，但进行中的 Skill 学习证据会被删除：
  workflow observations、workflow profiles、skill candidate refs、attributions、
  run skill activations 和 task skill bindings；failure guards 与候选证据快照
  保留冻结内容，只清除对已删除 Run 和 observation 的引用。每个磁盘上的记忆
  分区中，以已删除 Thread 为键的会话和偏好上的 run 来源引用会被清理，偏好
  本身保留。身份、账号、工作区、个人设置、记忆偏好、授权和 provider 状态均
  保留。
- `restore-control` 是恢复迁移或工作历史重置备份的显式入口，备份必须位于所选数据目录的
  `backups/` 下。执行前先停止 gateway；命令要求 `--yes`，替换前会验证 SQLite
  快照，并把失败数据库保留在恢复后的 `control.db` 旁供诊断。

- Eval 命令用于可复现的 Agent 质量测试。`--live` 允许真实 provider 调用；
  `--record-content` 可能持久化敏感内容，应谨慎使用。
- 具有破坏性的维护命令默认只做 dry-run，只有显式提供上面列出的
  apply/archive 参数才会写入。

## Gateway slash commands

Gateway 命令可用于 TUI 和受支持的 IM 渠道，并且会在普通 Agent 分发前处理。

```text
/help
/model
/id
/status [run_id]
/views [run_id] | /views archive|restore|prune <run_id>
/apply <run_id>
/queue [drop <n>|clear]
/watchers [active|attention|recent|all [page]|<n|id>|cancel <n|id>]
/effects [settle <claim_id> [reason] | resolve <claim_id> <watch_id>]
/diag [learning|memory|context|models|delivery|inbound|execution|tools]
/report daily [--since 24h]
/events
/approvals [grants|revoke <n>]
/approve <n|id|all> [run]
/reject <n|id|all>
/mode [on-request|read-only|auto-edit|full-auto|smart]
/tools readonly [<server>/<tool> on|off]
/stop [n|run_id]
/cancel
/notify <on|off|auto|platform|desk-first|phone-first>
/new [title] | /new --run <request>
/resume [n|run_id]  (bare = view work)
/choose <choice_id> <number>
/remember <preference>
/forget <text|ref>
/ws [n|id | default <n|id> | trust|untrust|decline]  (bare = list)
/add-dir [path]  (bare = list this session's extra roots)
```

- 每个终端是独立的会话：只完整显示本会话的运行；其他会话正在运行的任务、待处理的
  审批或问题只以一行状态提示出现，审批面板只在发起该运行的会话里弹出。
  裸 `/resume` 打开任务列表，包含等待中、运行中和最近的 Run；方向键选择，
  Enter 查看精确 Run 的状态、Plan 和待答内容，`r` 刷新详情，Esc/q 返回。
  查看不会执行或回答工作。允许续接时，`c` 将该精确 Run 选为本会话下一条
  用户消息的目标。`/attach [run_id]` 保留为只读兼容入口。本终端没有正在
  执行的前台 Run 时，运行中的选择可在这里流式观察。等待仍显示为等待，
  观察中断不代表任务完成。新输入保留在本会话，精确回复边或显式 `/resume`
  选择才会指向另一 Run；仅查看不会选择输入目标。
- `/views` 列出最近的受管 Git 执行视图；`/views <run_id>` 查看精确归属的 Run
  中已提交、未提交、未跟踪、被忽略及被隐藏的工作。仍有 Run 可能写入该视图时，
  只显示已提交的工作。`/apply <run_id>` 只把干净且已提交的视图送成
  原仓库里独立的 `selfmind/<view_id>` 分支。源分支可以前进，工作树也可以保留
  另一个 Run 的修改；原仓库身份和准入时的基准对象必须仍然有效。命令不改变当前
  分支或丢弃视图文件；仍有 Run 可能写入该视图时会拒绝交付。原 checkout 仍可能被
  改动时也会等待并点名原因：本人另一个对它有写权限、正在启动、运行或停放的 Run，
  或这类 Run 启动的后台进程。视图里的未提交、
  未跟踪及被忽略的文件仍留在原处供检查。Git 元数据指向别处，或配置新增了可运行
  程序的键时，所有命令都不再使用该视图，但保留其文件。
  Run 完成后，`/views archive <run_id>` 可把已交付且干净的视图整体移到 daemon
  保留区；仍有未完成 Run、队列项或正在启动的 Run 引用时会拒绝，尚未交付、含被
  忽略文件或含隐藏工作（标记 skip-worktree 或 assume-unchanged 的索引项、链接
  工作树、子模块仓库）时也会拒绝。`/views restore <run_id>` 为绑定该视图的任一
  本人 Run 将原有文件和 Git 元数据移回，包括因视图已归档而停放的工作。归档保留
  全部数据，不删除文件，也不回收磁盘空间。`/views prune <run_id>` 仅在 Run 已
  完成、没有待办引用、视图干净且已交付，并确认没有额外本地 Git 工作后，才显式
  清理归档：没有额外引用，视图自身存储的对象（无论松散还是已打包）都能从已交付
  提交到达。清理前会在源仓库建立保留提交的引用，随后删除检出及其本地 Git 元数据；
  与 `/apply` 一样，原 checkout 仍可能被改动时会等待。清理后不能恢复原检出；安全
  检查失败时保留归档。
- `/watchers` 在 CLI 与 IM 中使用同一个按 person 隔离的视图，展示 checker、
  operation、verification、finalization 和 notification 状态；原始命令、环境指纹
  与凭证不会显示在输出中。默认视图和 `all` 视图带稳定序号：使用
  `/watchers 1` 查看第一条 watcher，使用 `/watchers cancel 1` 停止监控。
  取消 watcher 不会取消外部操作。
- `/effects` 列出本人尚未释放的外部效果：每个占用的 Run、目标、状态（在途、
  未确定或有绑定 watcher）、脱敏的命令摘要及其调用如何结束。多项工作同时运行时，
  可能改变外部系统的命令或工具会占用目标；调用成功返回即自行释放，只有失败、
  超时、取消或崩溃才留下未确定的占用。
  `/effects settle <claim_id> [reason]` 释放你已知结果的未确定效果。只有你能结算
  自己的占用；结算记录你的账号、当前 channel 与理由，等待该效果的工作随后继续。
  仍在途的调用暂不能结算。
  产生该效果的 Run 也可以用绑定到该占用的成功只读检查来释放它。
  `/effects resolve <claim_id> <watch_id>` 把一个无人绑定、已成功定稿的 watcher
  关联到该占用；运行时核对本人归属、Run、watcher 晚于该效果以及终态证据后，才释放
  该效果占用的全部目标。模型自述成功不能解锁。
  效果画像可以为同一目标集合绑定一条精确的观察脚本命令，并另用 `ws observe`
  把该脚本登记为只读。由同一 Run 或其后续精确续接 Run 在派发后创建的
  `status_json.v1` watcher 得到持久的成功结论后，可在两项授权与脚本哈希仍然有效时
  自动释放该精确占用。
- `/tools readonly` 列出被标为只读的 MCP 工具：运维在
  `mcp.servers[].read_only_tools` 中写明的名字，以及本人的标注和每条标注的时间与
  来源端点。`/tools readonly <server>/<tool> on` 为本人把已连接服务器上的一个
  精确工具标为只读，`off` 撤销。只有本人亲自输入的命令能改变标注，定时任务的提示
  词不能。被标注工具的调用无需审批、不占用外部目标，并可
  与其他读取并行，因此只标注从不改变任何东西的工具。服务器对自身工具的标注
  （annotations）一律不算数。标注绑定服务器配置的命令、参数或 URL；配置改变后，
  列表会显示该标注已失效，直到本人重新标注。
- `/new [title]` 保留现有的 task 标签行为；`/new --run <request>` 是确定性的
  新工作入口，不经过连续性模型判断。
- `/status` 在同时有多个活动 Run 时列出总览；`/status <run_id>` 查看一个
  精确 Run 的状态，不按最近活动猜测目标。
- 裸 `/resume` 在 CLI 中查看任务，在 IM 中展示端点本地的 Attention 编号列表；
  `/resume <n|run_id>` 为发起选择的会话下一条用户消息选择精确 Run，入队前
  固定目标；恰有一个未解决 Run 的 Thread id 仍然
  接受。空闲时的 `/stop` 只 dismiss 该精确 Run，且在它仍有待处理审批、待处理
  澄清或活动 watcher 时拒绝。`/stop <n|run_id>` 清掉列表里的某一项而**不运行
  它**——陈旧项此前没有这个出口:自动归档从不碰仍有待处理人工输入的条目，而
  「先 pin 再 dismiss」会把你正想放下的工作启动起来。若指向正在执行的 Run，则
  只取消该 Run；同时有多个活动 Run 时，裸 `/stop` 会要求给出精确 Run ID。
  线上兼容字段 `Task.status` 使用派生词汇：
  Attention 为 `active`、`needs_attention`、`monitoring` 或 `resumable`，已
  settled 的 listed 工作为 `done`，已归档 Thread 为 `archived`；该值由 Run 和
  待处理控制对象计算得出，不会持久化。
- `/search [query]` 在所有端点检索过去的工作会话，裸 `/search` 列出最近的几条。
  在终端里，`/search current` 是当前对话的本地全保真回看。
- `/ws` 把会话与个人的持久默认分开。本地会话属于它被启动的那个目录，裸 `/ws` 会在
  该目录尚无工作区时创建它，所以列表里一定包含会话真正所在的位置。`/ws <n|id>`
  只切换**本会话**；`/ws default <n|id>` 改的是没有自己目录的回合（IM、cron）所用的
  默认值。这两者原先是同一个 person 级取值，于是两个终端在不同项目里会把对方的选择
  显示成自己的 current，而各自的工作其实在别处运行。
- 未信任的工作区不能使用工作区级 Skill 和已记住的审批观察。在未信任工作区中启动的
  会话，启动时**只问一次**：`/ws trust` 开启这两项能力，`/ws decline` 保持未信任并
  不再询问。普通文件与命令工作不受影响。
- `/choose <choice_id> <number>` 精确回答一个持久化的连续性问题，可从另一个
  已绑定端点或 daemon 重启后回答。裸数字只回答同一账号在同一聊天或 CLI 窗口里
  最近显示的那一个问题；在别处它是普通输入，原生回复或其他精确边始终送往自己的
  目标。
- `/notify <platform|auto>` 选择首选 IM 目标，`/notify <on|off>` 开关脱离
  会话时的通知；`desk-first` 让新审批先留在已附着的 TUI，并在 T1 后补推，
  `phone-first` 则立即同步到 IM。
- `/remember <preference>` 把用户明确表达的个人偏好写入长期记忆（个人记忆
  只存偏好），跨所有端点生效；`/forget <text|ref>` 按文本或 `/memory` 显示的
  ref 遗忘一条，多条匹配时返回带 ref 的编号列表。临时运行/构建状态会被拒绝
  并给出指引。

- 审批请求自身携带权威选项。普通请求显示“仅本次 / 当前 run 内复用 / 拒绝”，
  高敏感请求只显示“仅本次 / 拒绝”；新提示不再创建 task/person 级授权。
- `/approvals grants` 与 `/approvals revoke <n>` 仍可查看和撤销历史记忆授权。
- `/mode` 支持 `on-request`、`read-only`、`auto-edit`、`full-auto`
  和 `smart`。设置作用于新准入的 Run；收紧也作用于排队和停放的工作，放宽则
  不会：等待中的工作（排队，或停放在模型提供方、审批或提问上）按它准入时的
  模式与你当前模式中更严格的一个运行，且仅当你是在该工作首次准入之后设置当前
  模式时才如此，因此显式的单次运行模式不会被更早的偏好降级。已经在运行的 Run
  保持它的模式（用 `/stop` 停止它）。待处理审批仍需用 `/approve` 或 `/reject`
  回答；你已批准的仍然有效。在终端里 `/mode` 只作用于当前会话：它设置本会话
  之后的消息所携带的模式，不会更改你保存的偏好，因此不会收紧已经排队或停放的
  工作。请在 IM 或 HTTP 中设置 `/mode`（会保存偏好），或先 `/stop` 该工作再重新
  提出。显式的终端会话模式也会随运行中的补充消息传递到延期的新工作；未设置的
  会话模式保持为空，直到该工作准入时再读取偏好。
- `/notify` 选择 CLI 脱离后接收进度和最终结果的已绑定 IM 渠道。
- `/diag tools` 显示注册期工具 schema 目录。被修复或隔离的外部工具只显示
  工具名、问题类别和 schema 哈希，不显示原始 schema 或参数值。隔离工具不会
  发送给模型，也不能执行。
- `/diag inbound` 只列出当前用户已保存、但尚未确认接收的 IM 输入，并附上记录的原因。
  `pending` 收据尚未开始执行（例如在 SelfMind 关闭期间到达），来源会再次投递；
  内容已变化的重投会被拒绝，并记录在这张收据上。
  `dispatching` 状态可能已经产生效果，因此该命令不会自动重试；再次发送前
  需要先核对相关工作。
- `/diag context` 首行展示包含原生工具 schema 的最近一次 provider 请求估算；
  单独的 assembled prompt 小计会明确标注“不含原生工具 schema”。支持请求指纹
  的 provider 会展示 prefix 覆盖；不支持或 wrapper 转发断开时会给出明确状态。
- `/diag delivery recover stale-results` 会在当前 IM peer 内，把超出自动窗口的
  `pending_session` 最终结果合成一条有界摘要。只有确认送达后，才关闭摘要中列出的
  确切消息。`/diag delivery dismiss stale-results` 不发送消息而直接关闭这些旧最终
  结果。两者都不会影响审批、澄清、`sent_unconfirmed` 或其他 channel。

## 本地 TUI slash commands

以下命令依赖本地 TUI 状态，不会通过 IM 渠道执行。

```text
/skills [list|view|candidates|candidate|promote|reject|rollback|binding|bind|unbind|history|undo|search|install|audit|delete|archive|pin|unpin|stats|reload]
/bundles [list|view|create|delete]
/reload-skills
/memory [category|conflicts|search|show|correct|forget|pin|unpin|raw|history|undo]
/curator [status|run|restore]
/checkpoint [list|save|load|delete] [name]
/migrate
/clear
/exit
/compact
/paste-image
/capture [title]
/search [current|query]
/copy
```

在本机 TUI 中，`Ctrl+V` 会从 GUI 剪贴板附加图片，`/paste-image` 是明确的
备用入口。在 macOS 上，`Cmd+V` 由终端程序处理，应视为文字粘贴，而不是图片
快捷键。原生 Linux 需要 `wl-paste` 或 `xclip`；SSH 会话无法访问本机 GUI
剪贴板，应改用图片路径或通过 IM 发送图片。提交前删除完整的
`[Image #N · name]` token，就会解除该图片的附件引用。

- `/memory` 展示便于人类阅读的记忆索引；子命令可查看、纠正、置顶、遗忘
  和审计 canonical memory。
- `/skills bind <name>` 为当前任务指定一个默认逻辑 Skill；`/skills unbind`
  解除绑定。`/skills candidates` 列出尚未激活的 curator 候选，`candidate`、
  `promote`、`reject` 和 `rollback` 按明确的版本哈希进行管理。候选与历史版本
  正文不会进入普通 Agent prompt。
- `/search` 是唯一的回看入口：`/search current` 打开当前对话并展示完整
  diff，带关键词则搜索过去的工作会话，不带参数列出最近会话。`/copy`
  复制最近一次 assistant 回复。
- `/capture` 把最近完成的一轮保存为 eval case 草稿。

## 文档同步规则

可执行命令分发和 `internal/gateway/command/registry.go` 是行为事实源。
自动测试要求每个已注册命令的 usage 同时出现在英文和中文命令参考中。
