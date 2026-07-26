# SelfMind Command Reference

[中文](command-reference.zh-CN.md)

This page is the complete user-facing command index. Run `selfmind --help`,
`selfmind <command> --help`, or `/help` for the help bundled with the installed
binary.

## Global and lifecycle commands

```text
selfmind [--config PATH] [--resume SESSION_ID]
selfmind --version
selfmind setup [--non-interactive] [--skip-model] [--skip-gateway] [--check-model]
selfmind update [check] [--channel latest|next] [--force] [--no-restart]
selfmind uninstall --prepare [--purge-data --yes]
selfmind feedback [--out FILE|--send] [--repo OWNER/REPO] [--include-crash] <message>
```

- Running `selfmind` opens the TUI. On the first interactive launch, a missing
  model configuration opens guided setup before any daemon is started.
  Cancelling setup exits cleanly. Non-interactive launches never prompt and
  instead print the exact `setup` or `model set` command to run. `--config`
  selects another configuration file and `--resume` restores a prior TUI
  session.
- `setup` creates or upgrades configuration, configures a model, and starts the
  local gateway. Its skip flags make the flow suitable for automation.
- `update check` only checks for an update. `update` performs the full
  upgrade: it checks the selected npm channel, runs the package-manager
  install, verifies the new binary with `selfmind --version`, and restarts a
  running gateway daemon (drain-by-default) so it picks up the new version.
  `--force` reinstalls even when up to date or the check fails; `--no-restart`
  leaves the daemon on the old version until it is restarted manually. A
  stopped daemon is never started by `update`. The channel defaults to `auto`:
  a prerelease build follows `next`, a stable build follows `latest`, and an
  explicit `updates.channel: latest|next` in config pins one line. `--channel`
  affects only that invocation and never rewrites the config pin.
- `uninstall --prepare` stops and unregisters the daemon. Data is preserved
  unless `--purge-data --yes` is explicitly supplied.
- `feedback` writes a redacted diagnostic report locally by default. `--send`
  creates a GitHub issue through an authenticated `gh` CLI; `--repo` overrides
  the configured repository and `--include-crash` attaches the latest local
  crash report.

## Daily daemon client commands

All of these commands talk to the same gateway used by the TUI and IM channels.

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

- `send` dispatches a message without opening the TUI. `--async` returns after
  acceptance. `MODE` is one of `on-request`, `read-only`, `auto-edit`,
  `full-auto`, or `smart`.
- `tasks` lists open work by default; a status or keyword narrows the result.
- `task` accepts either the displayed list number or a stable task ID.
- `resume` reopens an archived task when necessary and starts the TUI on it.
- `workspaces`, `ws`, and `workspace` share the workspace controls:

```text
selfmind workspaces
selfmind ws
selfmind ws list
selfmind ws <n|workspace_id>
selfmind ws add [path] [name...]
selfmind ws use <workspace_id>
selfmind workspace [list|add|use|<n|workspace_id>] ...
```

- `approve` and `reject` accept a pending approval token when more than one
  request is waiting.
- `stop` cancels the active run. `new` creates a fresh visible task.

## Configuration, models, and channels

```text
selfmind config [doctor|upgrade]
selfmind model [current|check|list|set <provider> <model>]
selfmind auth [login|status|logout] ...
selfmind doctor [--out FILE] [--probe-models]
selfmind selfcheck [--skip-go] [--skip-eval] [--eval-dir DIR]
selfmind gateway [run|start|status|stop|restart|service] ...
selfmind weixin [login|status] ...
```

Detailed forms:

```text
selfmind auth login minimax-oauth [--region global|cn] [--no-browser]
selfmind auth status [provider]
selfmind auth logout minimax-oauth

selfmind gateway run [--replace] [--addr ADDR]
selfmind gateway start [--replace]
selfmind gateway status [--json]
selfmind gateway stop [--force]
selfmind gateway restart [--drain] [--force]
selfmind gateway service [install|status|uninstall]

selfmind weixin login [--timeout 8m] [--owner-person-id ID] [--no-enable]
selfmind weixin status
```

- `config doctor` reports missing or stale configuration without changing it;
  `config upgrade` adds supported defaults while preserving existing values.
- `model check` resolves credentials, protocol, endpoint, and model without
  exposing secrets.
- `doctor` checks the installation and configuration. `--probe-models` performs
  live provider probes and may consume provider quota.
- `selfcheck` runs repository checks used before release.
- `gateway restart` drains to a safe turn boundary by default; `--force` is an
  explicit last resort.
- On macOS, `gateway service install` creates the current user's launchd
  LaunchAgent. `gateway start`, `stop`, `status`, and `restart` then operate on
  that stable service; `restart --drain` lets the active turn reach a safe
  boundary before launchd relaunches the daemon.
- If Weixin reports an expired iLink session, run `selfmind weixin login`
  again. The running gateway watches the account credential file and resumes
  polling after the refreshed credentials are saved; no daemon restart is
  required.

## Evaluation and maintenance

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

- Eval commands are intended for reproducible agent-quality testing. `--live`
  permits provider calls; `--record-content` may persist sensitive content and
  should be used deliberately.
- Destructive maintenance commands are dry-run by default and require the
  explicit apply/archive flag shown above.

## Gateway slash commands

Gateway commands work in the TUI and supported IM channels. They are handled
before normal agent dispatch.

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

- `/approve ... task` remembers the approval class for the current task;
  `always` remembers it for the person.
- `/mode` accepts `on-request`, `read-only`, `auto-edit`, `full-auto`, or
  `smart`.
- `/notify` chooses the bound IM destination for CLI-origin progress and final
  notifications.

## Local TUI slash commands

These commands depend on local TUI state and are not sent through IM channels.

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

- `/memory` presents the human-oriented memory index; its subcommands inspect,
  correct, pin, forget, and audit canonical memories.
- `/history` opens the current transcript, while `/copy` copies the last
  assistant answer.
- `/capture` turns the last completed turn into an eval-case draft.

## Documentation policy

The executable command dispatch and
`internal/gateway/command/registry.go` are the behavioral sources of truth.
Tests require every registered command usage to appear in both this page and
the Chinese reference.
