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
- `setup` creates or upgrades configuration, guides the person through one
  primary model and one optional auxiliary model, then starts the local
  gateway. The auxiliary model covers approval triage, memory, recall,
  summaries, and skill work; per-role tuning remains an advanced YAML option.
  Its skip flags make the flow suitable for automation.
- `update check` only checks for an update. `update` performs the full
  upgrade: it checks the selected npm channel, runs the package-manager
  install, verifies the new binary with `selfmind --version`, and restarts a
  running gateway daemon (drain-by-default) so it picks up the new version.
  An equal version is reinstalled so local package replacements are restored;
  a running build newer than the selected channel is left untouched unless
  `--force` is set. `--force` also continues when the check fails; `--no-restart`
  leaves the daemon on the old version until it is restarted manually. A
  stopped daemon is never started by `update`. The channel defaults to `auto`:
  a prerelease build follows `next`, a stable build follows `latest`, and an
  explicit `updates.channel: latest|next` in config pins one line. `--channel`
  affects only that invocation and never rewrites the config pin.
  A development binary launched from the npm package can be restored directly
  with `selfmind update`; an independent source build still requires
  `--force` before SelfMind overwrites it.
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

- `send` dispatches a message without opening the TUI. `--async` returns after
  acceptance. `MODE` is one of `on-request`, `read-only`, `auto-edit`,
  `full-auto`, or `smart`. When no preference has been saved, the default is
  `smart`; without a configured triage model it safely falls back to asking.
- `tasks` lists open work by default; a status or keyword narrows the result.
- `watchers` lists durable external checks without invoking a model. The
  default view prioritizes active and attention-needed checks; a stable short
  watcher ID opens details or cancels only that check. Cancelling a watcher
  does not cancel the external operation.
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
selfmind ws trust [workspace_id]
selfmind ws untrust [workspace_id]
selfmind ws grants [workspace_id]
selfmind ws observe <script> [--network] [--credentials] [--all-args | -- <argv-prefix...>] [--workspace <id>]
selfmind ws revoke <capability> [workspace_id]
selfmind workspace [list|add|use|trust|untrust|grants|observe|revoke|<n|workspace_id>] ...
```

- Only an authenticated local CLI can change workspace trust. Omitting the
  workspace ID targets the current workspace. `untrust` also revokes active
  execution-capability grants for that workspace.
- `grants` lists temporary execution capabilities such as `network:shared`;
  `revoke` removes one immediately. These controls are also local-CLI only.
- `observe` registers one trusted workspace script as observation-only. The
  grant is bound to the workspace, resolved script path, content hash, argument
  shape, network choice, and credential choice. Editing or replacing the script
  invalidates it automatically. Use `--all-args` only when every argument shape
  is read-only; otherwise put the allowed argument prefix after `--`.
- `approve` and `reject` accept a pending approval token when more than one
  request is waiting.
- `stop` cancels the active run. `new` creates a fresh visible task.

## Configuration, models, and channels

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

Detailed forms:

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

- `docs check` validates the source documentation contract: UTF-8, complete
  manifest coverage, local links, language-pair source hashes, document size,
  and active-plan lifecycle. `docs index` regenerates `docs/README.md` from
  `docs/manifest.yaml`; it does not accept a stale translation automatically.
- `selfcheck` always runs the documentation contract. Combining `--skip-go`
  and `--skip-eval` is therefore a valid docs-only release check.

- `config doctor` reports missing or stale configuration without changing it;
  `config upgrade` adds supported defaults while preserving existing values.
- `model set` writes the single primary selection under `models.primary`.
  Reasoning/service-tier values are validated against discovered capabilities
  when available; `auto` leaves the choice to the provider/model.
- A shared daemon does not hot-switch from TUI `/model`. Restart at a safe turn
  boundary with `selfmind gateway restart --drain` after changing the model.
- `model check` resolves credentials, protocol, endpoint, and model without
  exposing secrets. `--role auxiliary` checks the shared auxiliary route;
  `--role <name>` checks that role after auxiliary/override resolution;
  `--live` sends bounded requests and validates native tool-schema
  compatibility. Providers with a multi-turn thinking/tool contract, such as
  DeepSeek V4, also replay one no-op tool result and verify the final answer;
  the probe therefore consumes a small amount of provider quota.
- `usage` is a CLI alias for `/diag context`. It reports provider-call input,
  cache-hit/cache-miss input, output, and reasoning-token totals when the
  provider supplies those fields. It reports tokens, not estimated currency.
- `report daily` produces a model-free quality and cost summary for the current
  person. It combines run outcomes, tool and approval counts, provider usage,
  cache statistics, maintenance health, delivery status, and recall adoption
  signals for a bounded window (24 hours by default, up to 30 days).
- `doctor` checks the installation and configuration. `--probe-models` performs
  live provider probes and may consume provider quota.
- `selfcheck` is the local release gate. The default `local-full` profile runs
  all locally provable release cases; `--fast`/`local-fast` skips measured slow cases;
  `--profile ci` runs only cases explicitly assigned to CI for this platform.
  Provider responses replay offline, while tool calls still use the current
  host toolchain. A missing tool fails unless the case is explicitly CI-owned
  for this platform; then local output says `CI-DEFERRED` and release requires
  the matching Action job. Exit codes are `0` pass, `1` regression, and `2`
  invalid or unavailable environment.
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
- `weixin login` binds the scanned WeChat user to the current CLI person by
  default, changes direct messages to `allowlist`, and records only that
  sender. `--owner-person-id` selects another existing person; `--no-bind`
  deliberately keeps identity-policy settings unchanged.

## Evaluation and maintenance

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

`maintenance replay` requeues retry-exhausted jobs only from each run's latest
analyzer generation. Older generations remain immutable history; use a small
`--limit` as a canary before replaying a backlog.

- Eval commands are intended for reproducible agent-quality testing. `--live`
  permits provider calls; `--record-content` may persist sensitive content and
  should be used deliberately.
- Destructive maintenance commands are dry-run by default and require the
  explicit apply/archive flag shown above.
- `task-audit` reports parked tasks without durable blocker evidence. `--apply`
  only backfills a blocker when the inactive task status exactly matches its
  newest finished run; conflicting or mixed histories remain review-only, and
  task/run statuses are never rewritten.
- `migrate-task-references` is dry-run by default. It imports a historical
  `task_runs.work_key` only when the exact reference occurs in that run's
  original user input. Inferred titles and summaries are reported and skipped;
  `--apply` is idempotent and never changes workspace or execution authority.

## Gateway slash commands

Gateway commands work in the TUI and supported IM channels. They are handled
before normal agent dispatch.

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

- Approval requests contain their authoritative choices. Ordinary requests show
  `once`, one optional `run`-local reuse choice, and `deny`; sensitive requests
  show only `once` and `deny`. New prompts never mint task/person-wide grants.
- `/approvals grants` and `/approvals revoke <n>` remain available for viewing
  and removing historical remembered grants.
- `/mode` accepts `on-request`, `read-only`, `auto-edit`, `full-auto`, or
  `smart`.
- `/notify` chooses the bound IM destination for CLI-origin progress and final
  notifications.
- `/watchers` is the same person-scoped, model-free view in CLI and IM. It
  shows checker/operation/verification phases plus finalization and notification
  state, while hiding raw commands, environment fingerprints, and credentials.
  The default and `all` views are numbered: use `/watchers 1` to inspect the
  first watcher or `/watchers cancel 1` to stop monitoring it. Cancelling a
  watcher does not cancel the external operation.
- `/task <id> references` lists the governed names and identifiers that may
  address that task. `/task <id> reference add <name>` confirms one immediately;
  `reference remove <name>` supersedes it. Automatically learned references
  require repeated user-text evidence, and conflicting references never route.
- `/diag tools` reports the registration-time tool schema catalogue. Repaired
  and quarantined external tools are listed by name, issue class, and schema
  hash; raw schemas and values are never printed. Quarantined tools are not
  sent to models and cannot execute.

## Local TUI slash commands

These commands depend on local TUI state and are not sent through IM channels.

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

- `/memory` presents the human-oriented memory index; its subcommands inspect,
  correct, pin, forget, and audit canonical memories.
- `/skills bind <name>` assigns the current task one default logical Skill;
  `/skills unbind` releases it. `/skills candidates` lists inactive curator
  proposals, and `candidate`, `promote`, `reject`, and `rollback` manage an
  explicit version hash. Candidate and previous bodies are never added to an
  ordinary agent prompt.
- `/search` is the single look-back entry point: `/search current` opens this
  conversation with complete diffs, a query finds past working sessions, and
  bare `/search` lists recent ones. `/copy` copies the last assistant answer.
- `/capture` turns the last completed turn into an eval-case draft.

## Documentation policy

The executable command dispatch and
`internal/gateway/command/registry.go` are the behavioral sources of truth.
Tests require every registered command usage to appear in both this page and
the Chinese reference.
