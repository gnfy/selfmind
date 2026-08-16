# SelfMind 命令参考

[English](command-reference.md)

这是面向用户的完整命令索引。已安装版本自带的帮助可通过
`selfmind --help`、`selfmind <command> --help` 或 `/help` 查看。

## 全局与生命周期命令

```text
selfmind [--config PATH] [--resume SESSION_ID]
selfmind --version
selfmind setup [--non-interactive] [--skip-model] [--skip-gateway] [--check-model]
selfmind update [check] [--channel latest|next] [--force] [--no-restart]
selfmind uninstall --prepare [--purge-data --yes]
selfmind feedback [--out FILE|--send] [--repo OWNER/REPO] [--include-crash] <message>
```

- 直接运行 `selfmind` 打开 TUI。首次在交互式终端启动时，如果缺少模型配置，
  程序会先进入引导设置，并且只在配置完成后启动 daemon。取消设置会直接退出；
  脚本、cron 和管道等非交互场景不会弹出提示，而会输出可执行的 `setup` 或
  `model set` 命令。`--config` 指定其他配置文件，`--resume` 恢复已有 TUI
  会话。
- `setup` 创建或升级配置，引导选择一个主模型和一个可选辅助模型，然后启动本地
  gateway。辅助模型统一承担审批、记忆、召回、摘要和 Skill 工作；逐角色调优仍可在
  YAML 中作为高级配置完成。各类 `skip` 参数可用于自动化环境。
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
selfmind send [--async] [--mode MODE] <message>
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
selfmind model [current|check [--live] [--role <name>]|list|set <provider> <model>]
selfmind auth [login|status|logout] ...
selfmind doctor [--out FILE] [--probe-models]
selfmind usage
selfmind report daily [--since 24h]
selfmind docs [check|index]
selfmind selfcheck [--fast | --profile local-full|local-fast|ci] [--skip-go] [--skip-eval] [--eval-dir DIR]
selfmind gateway [run|start|status|stop|restart|service] ...
selfmind weixin [login|status] ...
```

详细语法：

```text
selfmind model set <provider> <model> [--reasoning <level|auto>] [--service-tier <tier|auto>]
selfmind model check [--live] [--role <name>]

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
- `model set` 只写入 `models.primary` 这一处主模型配置。能力元数据存在时，
  会动态校验 reasoning/service tier；`auto` 表示交给 provider/模型默认处理。
- 共享 daemon 不支持在 TUI `/model` 中热切换。修改后使用
  `selfmind gateway restart --drain`，在安全 turn 边界重启。
- `model check` 解析凭证、协议、端点和模型，但不会暴露密钥。`--role auxiliary`
  检查共享辅助路由；`--role <name>` 按 auxiliary/角色覆盖规则检查具体角色；
  `--live` 会发起有界请求并验证原生工具 schema 兼容性。对于 DeepSeek V4
  这类具有多轮思考/工具契约的 provider，还会回放一次无副作用工具结果并验证
  最终答复，因此会消耗少量 provider 额度。
- `usage` 是 `/diag context` 的 CLI 别名。若 provider 上报相应字段，会展示调用级
  输入、缓存命中/未命中输入、输出和推理 token；它展示 token，不估算货币成本。
- `report daily` 生成当前 person 的无模型质量与成本摘要。它在有界时间窗口内汇总
  run 结局、工具与审批、provider 用量和缓存、后台维护健康、投递状态及召回采用
  趋势；默认最近 24 小时，最长 30 天。
- `doctor` 检查安装和配置；`--probe-models` 会真实调用 provider，可能消耗额度。
- `selfcheck` 是本地发布门禁。默认 `local-full` 运行本机能够证明的全部发布用例；
  `--fast`/`local-fast` 跳过已测量的慢用例；`--profile ci` 只运行明确分配给
  当前平台 CI 的用例。Provider 响应离线回放，但工具调用仍使用当前主机工具链。
  缺少工具时默认以环境不可用失败；只有明确由当前平台 CI 负责的用例才会显示
  `CI-DEFERRED`，且发布仍要求对应 Action 通过。退出码：`0` 通过、`1` 回归失败、
  `2` 参数或环境不可用。
- `gateway restart` 默认等待安全的 turn 边界；`--force` 仅作为最后手段。
- 在 macOS 上，`gateway service install` 会为当前用户创建 launchd
  LaunchAgent。之后 `gateway start`、`stop`、`status`、`restart` 都操作这个
  稳定服务；`restart --drain` 会先等待活跃 turn 到达安全边界，再由
  launchd 重新拉起 daemon。
- 如果微信 iLink 会话过期，重新运行 `selfmind weixin login`。正在运行的
  gateway 会监听账号凭据文件，在新凭据保存后自动恢复轮询，不需要重启
  daemon。
- `weixin login` 默认把扫码微信用户绑定到当前 CLI person，将私聊策略改成
  `allowlist`，并且只记录该发送者。`--owner-person-id` 可指定另一个已有
  person；`--no-bind` 用于明确保留现有的独立身份策略。

## Eval 与维护命令

```text
selfmind eval [list|run|report|repair|scorecard|capture|clean]
selfmind maintenance [replay|migrate-memory|migrate-skills|migrate-task-references|memory-audit|memory-dedup|task-audit] ...
```

```text
selfmind eval list [path]
selfmind eval run [case-or-dir] [--suite NAME] [--provider ID] [--model ID] [--tenant ID] [--workspace PATH] [--output PATH] [--record-content] [--live]
selfmind eval report <jsonl-or-dir>
selfmind eval repair [case-or-dir] [--worktree]
selfmind eval scorecard [case-or-dir] [--provider ID] [--out PATH] [--live]
selfmind eval capture [turn-id|latest] [--title "..."] [--suite NAME]
selfmind eval clean [--yes]

selfmind maintenance replay [--limit N]
selfmind maintenance migrate-memory [--apply] [--data-dir DIR]
selfmind maintenance migrate-skills [--apply] [--root DIR] [--governance-grace 30d]
selfmind maintenance migrate-task-references [--apply] [--limit N] [--data-dir DIR]
selfmind maintenance memory-audit [--archive-confirmed] [--partition P] [--data-dir DIR]
selfmind maintenance memory-dedup [--apply] [--partition P] [--data-dir DIR]
selfmind maintenance task-audit [--apply] [--limit N] [--data-dir DIR]
```

`maintenance replay` 只会重新入队每个 run 最新 analyzer generation 中因重试耗尽而
暂停的作业。旧 generation 保留为不可变历史；处理积压前应先用较小的 `--limit`
执行 canary。

- `task-audit` 默认只读，列出缺少持久 blocker 证据的暂停任务。只有任务当前无
  active run，且状态与最新已结束 run 完全一致时，`--apply` 才补写 blocker；
  历史混杂或状态冲突只报告，不修改 task/run 状态。
- `migrate-task-references` 默认只做 dry-run。只有历史 `work_key` 的完整表面
  形式确实出现在该 run 的原始用户输入中时才允许迁移；从标题或摘要推断出的
  值只报告并跳过。`--apply` 可重复执行，且不会赋予工作区或执行权限。

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
/notify <platform|auto>
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
