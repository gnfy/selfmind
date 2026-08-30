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
selfmind tasks [done|archived|all|<keyword>]
selfmind task <n|task_id> [runs|rename <name>|pin|unpin|archive|merge <dst>]
selfmind task <n|task_id> references|reference add <name>|reference remove <name>
selfmind resume <n|task_id>
selfmind workspaces
selfmind ws [list|add|use|trust|untrust|grants|observe|revoke|<n|workspace_id>] ...
selfmind approvals
selfmind approve [token]
selfmind reject [token]
selfmind stop
selfmind id
selfmind new [title]
```

- `watchers` 是不调用模型的持久 watcher 管理入口。默认优先显示运行中和需要
  关注的检查；可使用稳定的短 watcher ID 查看详情或只取消该 watcher。取消
  watcher 不会取消外部系统中已经启动的操作。

- `send` 在不打开 TUI 的情况下提交消息。`--async` 在 gateway 接收后立即返回。
  `MODE` 可用值为 `on-request`、`read-only`、`auto-edit`、`full-auto`
  或 `smart`。未保存个人偏好时默认使用 `smart`；未配置裁决模型时会安全
  降级为人工审批。
- `--add-dir` 可重复使用，且只允许已认证、连接本机 loopback gateway 的 CLI
  使用。相对路径以启动 SelfMind 的 shell 为基准解析；路径会被规范化，最多八个，
  并冻结到排队的 run 中，同时参与项目指令发现和文件系统冲突调度。它只扩大该 run
  的文件系统范围，不会授予 workspace 信任、绕过审批或沙箱，也不会修改持久
  workspace。运行中的 turn 不能改变附加目录集合；需要不同目录时应新开 run。
  若消息正文要原样包含 `--add-dir`，请先写 `--`。
- `tasks` 默认列出开放工作，也可以按状态或关键词过滤。
- `task` 支持列表序号或稳定 task ID。
- `resume` 必要时会重新打开已归档任务，并在该任务上启动 TUI。
- `workspaces`、`ws` 和 `workspace` 共用以下工作区操作：

```text
selfmind workspaces
selfmind ws
selfmind ws list
selfmind ws <n|workspace_id>
selfmind ws add [path] [name...]
selfmind ws use <workspace_id>
selfmind ws trust [workspace_id]
selfmind ws untrust [workspace_id]
selfmind ws grants [workspace_id]
selfmind ws observe <script> [--network] [--credentials] [--all-args | -- <argv-prefix...>] [--workspace <id>]
selfmind ws revoke <capability> [workspace_id]
selfmind workspace [list|add|use|trust|untrust|grants|observe|revoke|<n|workspace_id>] ...
```

- 只有已认证的本地 CLI 可以修改工作区信任状态。省略 workspace ID 时操作当前工作区；
  `untrust` 还会撤销该工作区当前有效的执行能力授权。
- `observe` 将可信工作区内的一份脚本登记为只读观测脚本。授权同时绑定工作区、
  解析后的脚本路径、内容哈希、参数形状、网络和凭证选择；脚本被修改或替换后授权
  自动失效。只有确认所有参数都只读时才使用 `--all-args`，否则在 `--` 后给出允许的
  参数前缀。
- 同时存在多个审批时，`approve` 和 `reject` 可接审批 token。
- `stop` 取消活跃 run；`new` 创建新的可见任务。

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
  `[PROMPT]`、`[DELIVERY]`、`[SKILLS]` 等稳定分类标识，所有处理命令统一汇总到带编号的
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
selfmind maintenance [replay|migrate-memory|migrate-skills|cleanup-person-partitions|prune-skill-candidate-refs|migrate-task-references|memory-audit|memory-dedup|task-audit|restore-control] ...
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

- `task-audit` 默认只读，列出缺少持久 blocker 证据的暂停任务。只有任务当前无
  active run，且状态与最新已结束 run 完全一致时，`--apply` 才补写 blocker；
  历史混杂或状态冲突只报告，不修改 task/run 状态。
- `migrate-task-references` 默认只做 dry-run。只有历史 `work_key` 的完整表面
  形式确实出现在该 run 的原始用户输入中时才允许迁移；从标题或摘要推断出的
  值只报告并跳过。`--apply` 可重复执行，且不会赋予工作区或执行权限。
- `restore-control` 是恢复迁移备份的显式入口，备份必须位于所选数据目录的
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
/status
/tasks [done|archived|all]
/task <n|id> [runs|rename <name>|pin|unpin|archive|merge <dst>|references|reference add|remove <name>]
/queue [drop <n>|clear]
/watchers [active|attention|recent|all [page]|<n|id>|cancel <n|id>]
/diag [memory|context|tasks|models|delivery|execution|tools]
/report daily [--since 24h]
/events
/approvals [grants|revoke <n>]
/approve <n|id|all> [run]
/reject <n|id|all>
/mode [mode]
/stop
/cancel
/notify <platform|auto|desk-first|phone-first>
/new [title]
/resume [n|task_id]  (bare = pick from recent tasks)
/workspace [n|id]  (bare = list; alias: /ws)
/workspaces  (same as bare /workspace or /ws)
```

- `/watchers` 在 CLI 与 IM 中使用同一个按 person 隔离的视图，展示 checker、
  operation、verification、finalization 和 notification 状态；原始命令、环境指纹
  与凭证不会显示在输出中。默认视图和 `all` 视图带稳定序号：使用
  `/watchers 1` 查看第一条 watcher，使用 `/watchers cancel 1` 停止监控。
  取消 watcher 不会取消外部操作。
- `/notify <platform|auto>` 选择首选 IM 目标；`desk-first` 让新审批先留在
  已附着的 TUI，并在 T1 后补推，`phone-first` 则立即同步到 IM。
- `/task <id> references` 查看可用于定位该任务的受治理名称和标识；
  `reference add <名称>` 由用户直接确认，`reference remove <名称>` 停用它。
  自动学习的 reference 需要不同 run 的原始用户文本重复支持；冲突时系统不会猜测。

- 审批请求自身携带权威选项。普通请求显示“仅本次 / 当前 run 内复用 / 拒绝”，
  高敏感请求只显示“仅本次 / 拒绝”；新提示不再创建 task/person 级授权。
- `/approvals grants` 与 `/approvals revoke <n>` 仍可查看和撤销历史记忆授权。
- `/mode` 支持 `on-request`、`read-only`、`auto-edit`、`full-auto`
  和 `smart`。
- `/notify` 选择 CLI 脱离后接收进度和最终结果的已绑定 IM 渠道。
- `/diag tools` 显示注册期工具 schema 目录。被修复或隔离的外部工具只显示
  工具名、问题类别和 schema 哈希，不显示原始 schema 或参数值。隔离工具不会
  发送给模型，也不能执行。
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
