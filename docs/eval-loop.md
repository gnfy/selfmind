# SelfMind Eval Loop

SelfMind uses a local-first eval loop to test agent behavior without needing a
large production dataset. Eval cases run through the same gateway, task, tool,
stream, and model-provider path as normal CLI or IM requests.

## Goals

- Catch regressions in provider compatibility, tool calling, workspace
  resolution, continuation, output formatting, and long-task behavior.
- Keep fixtures small and representative of real daily development work.
- Store structured JSONL run logs locally for comparison and failure triage.

## Commands

```bash
selfmind eval list [path]
selfmind eval run evalcases/daily-dev/chat_basic.yaml
selfmind eval run --suite daily-dev --provider kimi-coding --model kimi-for-coding
selfmind eval report evalruns/2026-06-25
selfmind selfcheck                 # build + test + offline-replay eval gate (no quota)
selfmind eval repair [case-or-dir] # re-run failures, print a repair brief (apply stays manual)
selfmind eval scorecard [dir]      # per-scenario daily-driver readiness report
selfmind eval capture [latest]     # promote the last recorded turn into an eval case
selfmind eval clean [--yes]        # remove historic eval residue from the configured control.db
```

`eval run` writes JSONL logs under `evalruns/YYYY-MM-DD/` by default. The
directory is intentionally ignored by git.

## Data Isolation (default)

Every eval run — record and replay alike — uses a **throwaway temp data dir**
(fresh `control.db` + memory) by default. Eval identities (`eval-<case-id>`
persons/accounts), tasks, runs, events, and current-task pointers are created
in that temp dir and deleted when the case finishes, so recording sessions can
never pollute the user's real `~/.selfmind` data. Two related knobs:

- **Workspace isolation stays scenario-driven.** Cases with `setup`,
  `assert_state`, or `workspace: isolated` also get a scratch workspace seeded
  from `setup.files`; plain cases (e.g. `workspace: "."`) still probe their
  declared workspace path — only the durable data dir is isolated.
- **`shared_data: true`** is the explicit opt-out: the case runs against the
  configured data dir. Almost nothing should need it — each case creates its
  own eval identity, so there is no shared state to inherit. It cannot be
  combined with `setup`, `assert_state`, or `workspace: isolated`.

VCR cassettes are unaffected: they live under `.vcr/<case-id>/` (or
`SELFMIND_EVAL_VCR_DIR`) keyed by case ID, independent of the data dir.

The runner also guarantees **run finalization**: eval turns are synchronous,
and after each case the harness sweeps the runs it created (scoped to its
tenant + persons) and forces any run still `running` to `interrupted`, so eval
can never leave phantom running rows behind. A forced sweep is surfaced as a
`run_finalization` check message in the JSONL log.

Historic residue from before isolation-by-default can be removed with:

```bash
selfmind gateway stop        # never run two processes against one control.db
selfmind eval clean          # dry-run: prints what would be deleted, with counts
selfmind eval clean --yes    # actually delete
```

`eval clean` selects persons whose ONLY accounts have platform `eval` (a
person with even one real binding is never touched) and removes their
accounts, workspaces, tasks, runs, events, handoffs, artifacts, channel
messages, approvals, notifications, outbound messages, current-task/workspace
pointers, and any `eval-*` tenants left empty.

## Daily feedback loop: flight recorder + capture

The fastest way to grow the suite is to record your real usage and promote the
turns that misbehaved — turning everyday friction into permanent regression
tests, for free.

1. **Turn on the flight recorder** (records each real turn's model interaction
   into a bounded, auto-pruned dir; no extra model cost — it saves what already
   streamed). Configure it in `config.yaml`:

   ```yaml
   flight_recorder:
     enabled: true
     # dir: ~/.selfmind/flight   # optional
     # keep: 20                  # optional, most-recent turns kept
   ```

   Env vars override the YAML when set (`SELFMIND_FLIGHT_RECORDER=1`,
   `SELFMIND_FLIGHT_DIR`, `SELFMIND_FLIGHT_KEEP`).

2. **Use SelfMind normally.** When a turn misbehaves (wrong continuation, ugly
   output, didn't use a tool, created the wrong file), capture it:

   - In the TUI: `/capture continuation should keep the same task`
   - Or from a shell: `selfmind eval capture latest --title "..."`

   This writes a draft case to `evalcases/captured/` and copies that turn's
   cassette into `.vcr/<case-id>/` so it replays offline.

3. **Edit the draft** to encode *what should have happened* — fill in
   `assert_state` (e.g. a file that must exist, a task that must continue) and/or
   `expect`. The recorded model output is the negative example; you pin the
   desired terminal state.

4. **Lock it in.** `selfmind selfcheck` now replays your captured cases offline
   (zero quota) before every change, so the things that annoyed you can't
   silently regress.

What this catches well: UX / harness / behavior regressions (mojibake, raw-JSON
or tool-XML leaks, broken continuation, wrong workspace, missing tool events,
empty output, file/task terminal state) — these are deterministic under the
recorded model outputs. Pure model-quality issues ("the answer was wrong") are
documented by the captured case but only go green after a prompt change + a
re-record, so treat those as a benchmark / model-routing signal (see the
`dayinlife` scenario-5 provider-resilience probe).

Flight recording and capture only cover real user-facing turns; internal
background/delegation turns are skipped. Clipboard/cloud caveats from
`docs/phase1-modules.md` apply.

## Case Format

```yaml
id: codebase_analysis_001
title: "inspect the current codebase"
suite: daily-dev
workspace: "."
channel: cli

turns:
  - input: "分析一下当前 selfmind 项目的代码结构。"

expect:
  status: completed
  require_tool_events: true
  min_tool_calls: 1
  max_tool_calls: 8
  max_tool_errors: 3
  max_duration_seconds: 300
  contains:
    - "selfmind"
  must_not_contain:
    - "<tool>"

checks:
  no_mojibake: true
  no_raw_json_leak: true
  no_tool_xml_leak: true
  no_empty_response: true
  no_provider_stack_dump: true
  tool_failure_should_recover: true
  workspace_should_match: true
  context_not_exceeded: true
```

Use multi-turn cases to test `continue`, `可以`, and `按方案做` behavior:

```yaml
turns:
  - input: "先给方案，不要改代码。"
  - input: "可以，继续。"

expect:
  require_same_task: true
  require_continuation: true
```

Turns may override the case channel when a scenario needs to simulate a user
continuing the same task from another surface:

```yaml
channel: cli
turns:
  - input: "先给一个方案，不要改代码。"
    channel: cli
  - input: "继续第一步。"
    channel: weixin
```

Turns may also override `platform_user_id` to simulate a *different* platform
user mid-case. This is the identity-isolation probe: person-scoped task/run
state must not leak to another account:

```yaml
turns:
  - input: "开始一个任务。"
  - input: "/status"
    channel: weixin
    platform_user_id: "eval-stranger"   # a different person; must see nothing
```

`require_cassette: true` marks a case as mandatory for the offline gate:
`selfmind selfcheck` FAILS (instead of skipping) when no cassette is recorded
for the case ID. Use it for north-star scenarios that must never silently drop
out of CI. Optional cases (the default) are still skipped when unrecorded, so
the gate keeps growing as cassettes are recorded.

```yaml
id: continuity_resume
require_cassette: true
```

## Selfcheck Gate Floor

`selfmind selfcheck` runs the eval phase in strict offline replay
(`SELFMIND_EVAL_VCR=replay` + `SELFMIND_EVAL_OFFLINE=1`). Two mechanisms keep
the gate honest:

- `require_cassette: true` cases fail selfcheck when their cassette is missing
  (see above).
- `SELFMIND_EVAL_MIN_CASES=N` fails selfcheck when fewer than N cases were
  actually replayed (skipped cases do not count). CI sets it in
  `.github/workflows/ci.yml` so deleted cassettes or renamed case IDs cannot
  quietly degrade the gate to verifying ~0 cases. Unset (the local default)
  keeps the grow-as-you-record behavior.

## Continuity Suite

`evalcases/continuity/` covers the cross-endpoint north-star scenarios: a task
started from the CLI must be visible via `/status` from an IM channel, resumable
with a bare `继续` from another channel (`require_same_task` +
`require_continuation`), and invisible to a different platform user (per-turn
`platform_user_id` override + `must_not_contain` on the first user's task
title). All three cases set `require_cassette: true`; see
`evalcases/continuity/README.md` for the one-command recording instructions
(`SELFMIND_EVAL_VCR=record selfmind eval run evalcases/continuity`) — the
recorded `.vcr/continuity_*` directories must be committed.

## Architecture

- `internal/eval/case.go` parses YAML fixtures.
- `internal/eval/runner.go` builds the normal SelfMind runtime and calls
  `httpapi.Server.ProcessMessage`.
- `internal/eval/recorder.go` observes `llm.StreamEvent` through
  `httpapi.WithStreamObserver` and writes JSONL.
- `internal/eval/checks.go` runs deterministic quality checks.
- `internal/eval/state_oracle.go` evaluates `assert_state` world-state
  predicates against `control.db` and the workspace after a run.
- `internal/eval/scenario.go` drives multi-turn scenario cases.
- `internal/eval/capture.go` promotes a flight-recorded real turn into a
  replayable eval case (`/capture`, `selfmind eval capture`).
- `internal/eval/report.go` summarizes JSONL logs.
- `internal/cliapp/eval_commands.go` exposes the CLI.

Eval must not create a parallel agent implementation. It should exercise the
same path a user sees in CLI or IM. When adding new product behavior, add or
update eval cases first, then keep implementation fixes grounded in failed
events.

## P0/P1 Coverage

Use `evalcases/p0-p1-5/` as the small regression suite for the current
agent-first contract:

- direct identity/model questions should complete without tool calls;
- small code snippets should produce clean, readable output;
- short approval/continuation turns should reuse the active task;
- codebase inspection should emit visible tool progress;
- command or environment diagnosis should first identify the project ecosystem,
  manifests, scripts, CI files, runtime versions, and workspace boundaries
  before recommending any command or override. Fixtures should cover this as a
  language-agnostic behavior, not as a Go-only workaround.

`eval report` now aggregates first-token latency and tool call/error totals so
streaming responsiveness and tool visibility regressions are easier to spot.

## Failure Categories

The runner currently classifies common failures as:

- `provider_auth`
- `provider_transport`
- `tool_schema`
- `context_overflow`
- `workspace_scope`
- `command_failed`
- `timeout`
- `unknown`

Prefer adding precise categories over broad string matching when a new recurring
failure appears.

## Privacy

Eval logs are local by default. They record content previews and hashes. Full
assistant output is stored only when a case or command enables
`record_content`.
