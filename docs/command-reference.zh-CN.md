# SelfMind 命令参考

[English](command-reference.md)

这是面向用户的完整命令索引。已安装版本自带的帮助可通过
`selfmind --help`、`selfmind <command> --help` 或 `/help` 查看。

## 全局与生命周期命令

```text
selfmind [--config PATH] [--resume SESSION_ID]
selfmind --version
selfmind setup [--non-interactive] [--skip-model] [--skip-gateway] [--check-model]
selfmind update [check] [--channel latest|next]
selfmind uninstall --prepare [--purge-data --yes]
selfmind feedback [--out FILE|--send] [--repo OWNER/REPO] [--include-crash] <message>
```

- 直接运行 `selfmind` 打开 TUI。`--config` 指定其他配置文件，
  `--resume` 恢复已有 TUI 会话。
- `setup` 创建或升级配置、配置模型并启动本地 gateway。各类 `skip`
  参数可用于自动化环境。
- `update check` 只检查更新；通过 npm 安装时，`update` 会安装指定发布通道。
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
selfmind tasks [done|archived|all|<keyword>]
selfmind task <n|task_id> [runs|rename <name>|pin|unpin|archive|merge <dst>]
selfmind resume <n|task_id>
selfmind workspaces
selfmind ws [list|add|use|<n|workspace_id>] ...
selfmind approvals
selfmind approve [token]
selfmind reject [token]
selfmind stop
selfmind id
selfmind new [title]
```

- `send` 在不打开 TUI 的情况下提交消息。`--async` 在 gateway 接收后立即返回。
  `MODE` 可用值为 `on-request`、`read-only`、`auto-edit`、`full-auto`
  或 `smart`。
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
selfmind workspace [list|add|use|<n|workspace_id>] ...
```

- 同时存在多个审批时，`approve` 和 `reject` 可接审批 token。
- `stop` 取消活跃 run；`new` 创建新的可见任务。

## 配置、模型与渠道

```text
selfmind config [doctor|upgrade]
selfmind model [current|check|list|set <provider> <model>]
selfmind auth [login|status|logout] ...
selfmind doctor [--out FILE] [--probe-models]
selfmind selfcheck [--skip-go] [--skip-eval] [--eval-dir DIR]
selfmind gateway [run|start|status|stop|restart] ...
selfmind weixin [login|status] ...
```

详细语法：

```text
selfmind auth login minimax-oauth [--region global|cn] [--no-browser]
selfmind auth status [provider]
selfmind auth logout minimax-oauth

selfmind gateway run [--replace] [--addr ADDR]
selfmind gateway start [--replace]
selfmind gateway status [--json]
selfmind gateway stop [--force]
selfmind gateway restart [--drain] [--force]

selfmind weixin login [--timeout 8m] [--owner-person-id ID] [--no-enable]
selfmind weixin status
```

- `config doctor` 只报告配置缺失或过期项；`config upgrade` 在保留已有值的
  前提下补充受支持的默认项。
- `model check` 解析凭证、协议、端点和模型，但不会暴露密钥。
- `doctor` 检查安装和配置；`--probe-models` 会真实调用 provider，可能消耗额度。
- `selfcheck` 执行发布前使用的仓库检查。
- `gateway restart` 默认等待安全的 turn 边界；`--force` 仅作为最后手段。

## Eval 与维护命令

```text
selfmind eval [list|run|report|repair|scorecard|capture|clean]
selfmind maintenance [replay|migrate-memory|memory-audit|memory-dedup] ...
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
selfmind maintenance memory-audit [--archive-confirmed] [--partition P] [--data-dir DIR]
selfmind maintenance memory-dedup [--apply] [--partition P] [--data-dir DIR]
```

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
/task <n|id> [runs|rename <name>|pin|unpin|archive|merge <dst>]
/queue [drop <n>|clear]
/diag [memory|context|tasks|models|delivery]
/events
/approvals
/approve <n|id|all> [task|always]
/reject <n|id|all>
/mode [mode]
/stop
/cancel
/notify <platform|auto>
/new [title]
/resume <n|task_id>
/workspace [n|id]  (bare = list; alias: /ws)
/workspaces  (same as bare /workspace or /ws)
```

- `/approve ... task` 为当前任务记住同类授权；`always` 为当前 person
  持久记住同类授权。
- `/mode` 支持 `on-request`、`read-only`、`auto-edit`、`full-auto`
  和 `smart`。
- `/notify` 选择 CLI 脱离后接收进度和最终结果的已绑定 IM 渠道。

## 本地 TUI slash commands

以下命令依赖本地 TUI 状态，不会通过 IM 渠道执行。

```text
/skills [list|view|history|undo|search|install|audit|delete|archive|pin|unpin|stats|reload]
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
/history
/copy
```

- `/memory` 展示便于人类阅读的记忆索引；子命令可查看、纠正、置顶、遗忘
  和审计 canonical memory。
- `/history` 打开当前 transcript，`/copy` 复制最近一次 assistant 回复。
- `/capture` 把最近完成的一轮保存为 eval case 草稿。

## 文档同步规则

可执行命令分发和 `internal/gateway/command/registry.go` 是行为事实源。
自动测试要求每个已注册命令的 usage 同时出现在英文和中文命令参考中。
