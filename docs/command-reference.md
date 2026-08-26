# SelfMind Command Reference

[中文](command-reference.zh-CN.md)

This page is the complete user-facing command index. Run `selfmind --help`,
`selfmind <command> --help`, or `/help` for the help bundled with the installed
binary.

## Global and lifecycle commands

```text
selfmind [--config PATH] [--resume SESSION_ID] [--add-dir DIR]...
selfmind --version
selfmind setup [--non-interactive] [--skip-model] [--skip-gateway] [--check-model]
selfmind update [check] [--channel latest|next] [--force] [--no-restart]
selfmind uninstall --prepare [--purge-data --yes]
selfmind feedback [--out FILE|--send] [--repo OWNER/REPO] [--include-crash] <message>
```

- Running `selfmind` opens the TUI. On the first interactive launch, an
  incomplete installation opens the same guided setup on Linux and macOS
  before the TUI starts. Cancelling setup exits cleanly. Non-interactive
  launches never prompt and instead print the exact `setup` command to run. `--config`
  selects another configuration file and `--resume` restores a prior TUI
  session. Repeatable `--add-dir` grants that CLI invocation access to another
  local directory without modifying the registered workspace.
- `setup` creates missing configuration and confirms two visible routes: the
  primary model for interactive work and the background model for maintenance.
  It then confirms one project workspace, its trust boundary, the approval
  mode, and background operation. The fast path has two confirmations. Both
  models receive bounded live probes before setup continues, and the installed
  daemon probes the same routes again from its own environment. Shell-only API
  credentials accepted by the person are copied to SelfMind's private auth
  store, never to an operating-system service definition. The selected
  workspace cannot default silently to `/` or the user's home directory.
  launchd on macOS and per-user systemd on Linux are implementation details;
  the user-facing choice is simply managed background operation or on-demand
  startup. A first successful non-command TUI task completes the first-use
  receipt. The skip flags remain available for controlled automation.
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

- `send` dispatches a message without opening the TUI. `--async` returns after
  acceptance. `MODE` is one of `on-request`, `read-only`, `auto-edit`,
  `full-auto`, or `smart`. When no preference has been saved, the default is
  `smart`; without a configured triage model it safely falls back to asking.
- `--add-dir` is repeatable and available only to an authenticated local CLI
  connected to the loopback gateway. Relative paths resolve in the launching
  shell. Each path is canonicalized, bounded to eight entries, frozen into the
  queued run, and included in project-instruction discovery and filesystem
  conflict scheduling. It expands the run's filesystem scope but never grants
  workspace trust, bypasses approval/sandbox policy, or changes the durable
  workspace. A running turn cannot change its additional-root set; start a new
  run to use a different set. Use `--` before message text that literally
  contains `--add-dir`.
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

Detailed forms:

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

- `docs check` validates the source documentation contract: UTF-8, complete
  manifest coverage, local links, language-pair source hashes, document size,
  and active-plan lifecycle. `docs index` regenerates `docs/README.md` from
  `docs/manifest.yaml`; it does not accept a stale translation automatically.
- `selfcheck` always runs the documentation contract. Combining `--skip-go`
  and `--skip-eval` is therefore a valid docs-only release check.

- `config doctor` reports missing or stale configuration without changing it;
  `config upgrade` adds supported defaults while preserving existing values.
- `selfmind model` is the only CLI model command. It opens the same Model
  Manager as bare `/model` in the TUI. Passing a subcommand is rejected instead
  of exposing a second mutation path.
- The manager shows Main, Background, optional overrides for
  `fast_classifier`, `memory_extract`, `background_review`, `skill_curator`,
  `semantic_recall`, and `summarizer`, plus change/recovery status. Unset role
  overrides visibly use Background. Main is the only route allowed to own a
  complete user-visible turn.
- One session may edit Main, Background, and several roles. Every completed
  selection is probed automatically in the daemon environment; there is no
  separate verify command. A failed probe keeps the draft editable and does
  not change YAML. Missing API-key credentials can be entered without terminal
  echo and are written to the private auth store only after a successful probe.
  The final review submits one non-secret atomic snapshot and schedules one
  safe restart.
- Existing reasoning, service-tier, context, and YAML-only transport fields are
  preserved when compatible. Unknown compatibility resets only the affected
  selectable option to `auto` with a notice. The first Main selection may
  initialize an empty Background slot; later Main changes never overwrite it.
- An accepted online change schedules a safe daemon restart. The current run
  retains its frozen route; new work received during the drain is queued for
  the next daemon. Startup probes the candidate again and commits it only after
  the Agent and `/health` are healthy. Deterministic model incompatibility
  restores the last running routes; infrastructure or unknown failure enters
  `recovery_required`. Retry/restore choices appear inside Change status; the
  bounded non-secret history and running/configured/pending views remain
  internal parts of the manager rather than separate commands. Providers with
  a multi-turn thinking/tool contract, such as DeepSeek V4, also replay one
  no-op tool result during automatic validation and verify the final answer;
  the probe therefore consumes a small amount of provider quota.
- `prompt` manages the operator-owned workspace next to the active config file
  (normally `~/.selfmind/prompts/`); no `config.yaml` keys are required.
  `list`/`status` compares the disk snapshot with the running daemon's loaded
  hash and build, so equal customization hashes cannot hide built-in prompt
  changes that still need a restart. `show` displays the static built-in role
  contract and local file without exposing runtime memory or project context.
  `edit` creates a marked-section
  template for `agent`, `memory_extract`, `background_review`, `skill_curator`,
  `summarizer`, or `semantic_recall`; Markdown headings inside a marked section
  are preserved as custom content. Editing an existing markerless file first
  converts it to the marked grammar and writes a timestamped `.legacy-*`
  backup. Exact marker examples inside Markdown code fences remain content.
- A prompt section containing `default` inherits the built-in behavior. Only
  explicitly replaceable foreground sections accept `off`; quality,
  verification, response-schema, governance, tool, and safety floors cannot be
  disabled. Unknown section markers (and unknown reserved headings in legacy
  markerless files), invalid UTF-8, symlinks or group/world-writable permissions
  on the prompt root, nested directories, or files, and over-limit content fail
  validation. At startup, an invalid active workspace
  produces a persistent degraded finding and the daemon uses the matching
  last-known-good snapshot, or built-in defaults when none exists, so agent
  endpoints remain available. Security checks are not bypassed on filesystems
  that report broad write permissions. `reset` creates a timestamped backup.
  `apply` validates before restarting a running daemon; an explicit
  `gateway restart` also validates before stopping the current process. There
  is no hot reload. `test` deterministically verifies the active section
  composition and makes no model call.
- `usage` is the model-free, person-scoped 24-hour execution and token report
  (the same data path as `/report daily --since 24h`). It includes provider
  input, cache read/miss, output and reasoning totals, native tool-schema share,
  tool/approval activity, and current maintenance/delivery health when those
  signals are available. It reports tokens, not estimated currency.
- `report daily` produces a model-free quality and cost summary for the current
  person. It combines run outcomes, tool and approval counts, provider usage,
  cache statistics, maintenance health, delivery status, and recall adoption
  signals for a bounded window (24 hours by default, up to 30 days). Event
  pages are scanned up to the documented diagnostic ceiling; if more rows
  exist, the report labels every affected aggregate as a lower bound instead
  of presenting a silently truncated total.
- `doctor` reports only current problems by default, together with a concrete
  recovery or inspection command. Findings carry stable category tags such as
  `[CONFIG]`, `[PROMPT]`, `[TRUST]`, `[DELIVERY]`, and `[SKILLS]`; every command
  is gathered into one numbered `Next actions` section. Interactive terminals use the TUI
  warning, error, info, success, text, and muted palette. Pipes, redirection,
  `NO_COLOR`, `CLICOLOR=0`, and `TERM=dumb` remain plain text. Healthy
  subsystems and historical runs, errors, events, activity, and logs stay
  hidden. `--verbose` prints the full redacted diagnostic bundle; `--out FILE`
  always writes that full bundle for support or offline inspection.
  `--probe-models` performs live provider probes and may consume provider quota.
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
- `gateway service install` creates a per-user launchd LaunchAgent on macOS or
  a per-user systemd unit on Linux. `gateway start`, `stop`, `status`, and
  `restart` then operate on that stable service; `restart --drain` lets the
  active turn reach a safe boundary before the service relaunches the daemon.
  Neither personal service needs administrator access. An active system-wide
  Linux `selfmind.service` is reported as a conflict instead of being stopped
  or replaced by personal setup.
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

`maintenance replay` requeues retry-exhausted post-run jobs from each durable
key's latest analyzer generation. Work parked as `blocked_prompt_revision` is a
separate, operator-triggered scope: the command reports how much is parked, and
`--prompt-revision` requeues it. Restore the missing content-addressed prompt
revision first; replaying while it is still unavailable returns the job to the
same visible blocked state without a model call, and resets its attempt count
and recorded reason. Older generations remain immutable history; use a small
`--limit` as a canary before replaying a backlog.

`maintenance cleanup-person-partitions` compares direct `person_*` filesystem
partitions with every person in `control.db`. It is a dry-run by default. Apply
mode requires the gateway to be stopped and moves reviewed orphans into a
timestamped `.orphaned-person-partitions` quarantine under the same root; it
does not delete them. The default root and control database come from the same
loaded configuration. If `--root` selects a different asset tree, `--data-dir`
must explicitly identify the authoritative `control.db`; the command never
opens or creates a guessed empty database.

`maintenance prune-skill-candidate-refs` previews only refs whose owning work
unit is terminal or missing. `--apply` deletes exactly that transactional
selection, excludes live work units, then reruns the preview as verification.
Use the owner rows in the dry-run output together with
`selfmind doctor --verbose`; never delete candidate refs directly from SQLite.

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
- `restore-control` is the explicit recovery path for a migration backup under
  the selected data directory's `backups/` folder. Stop the gateway first. It
  requires `--yes`, verifies the SQLite snapshot before replacement, and keeps
  the failed database beside the restored `control.db` for diagnosis.

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
/notify <platform|auto|desk-first|phone-first>
/new [title]
/resume [n|task_id]  (bare = pick from recent tasks)
/workspace [n|id]  (bare = list; alias: /ws)
/workspaces  (same as bare /workspace or /ws)
```

- Approval requests contain their authoritative choices. Ordinary requests show
  `once`, one optional `run`-local reuse choice, and `deny`; sensitive requests
  show only `once` and `deny`. New prompts never mint task/person-wide grants.
- `/notify <platform|auto>` selects the preferred IM destination.
  `/notify desk-first` keeps young CLI-origin approvals in the attached TUI and
  escalates after T1; `/notify phone-first` mirrors them to IM immediately.
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
- `/diag context` starts with the latest provider request estimate, including
  native tool schemas, and labels the separately assembled prompt subtotal as
  excluding native schemas. Fingerprint-capable providers show prefix coverage;
  unsupported or broken forwarding is explicit rather than an empty field.
- `/diag delivery recover stale-results` sends one bounded recap for old
  `pending_session` final results in the current IM peer. Confirmed delivery
  dismisses exactly the summarized rows. `/diag delivery dismiss stale-results`
  closes those old final results without sending. Neither command touches
  approvals, clarifications, `sent_unconfirmed`, or another channel.

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
