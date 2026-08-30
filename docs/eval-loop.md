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
selfmind eval run evalcases/daily-dev/project_context_agents_md.yaml
selfmind eval run --suite daily-dev --provider kimi-coding --model kimi-for-coding
selfmind eval report evalruns/2026-06-25
selfmind selfcheck                 # docs + build + test + offline-replay eval gate (no quota)
selfmind selfcheck --skip-go --skip-eval # documentation-only release gate
selfmind eval repair [case-or-dir] # re-run failures, print a repair brief (apply stays manual)
selfmind eval scorecard [dir]      # per-scenario daily-driver readiness report
selfmind eval capture [latest]     # promote the last recorded turn into an eval case
selfmind eval clean [--yes]        # remove historic eval residue (control.db rows + on-disk eval-* dirs)
```

`eval run` writes JSONL logs under `evalruns/YYYY-MM-DD/` by default. The
directory is intentionally ignored by git.

## Data Isolation (default)

Every eval run — record and replay alike — uses a **throwaway temp data dir**
(fresh `control.db` + memory) by default. Eval identities (`eval-<case-id>`
persons/accounts), tasks, runs, events, and current-task pointers are created
in that temp dir and deleted when the case finishes, so recording sessions can
never pollute the user's real `~/.selfmind` data. The isolated root also owns a
minimal, credential-free model configuration and its readiness receipt; replay
therefore neither reads nor writes the operator's adjacent `model-state.json`.
Two related knobs:

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
Offline replay establishes readiness only for that throwaway model state so the
production gateway can reach the cassette provider boundary. A missing or
misordered cassette still fails closed and never falls through to a live
provider. Existing workspace paths are canonicalized before registration and
VCR placeholder expansion, so filesystem aliases such as macOS `/var` and
`/private/var` cannot make an in-scope replayed tool call appear out of scope.

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
pointers, person-scoped workflow/Skill projections, and any `eval-*` tenants
left empty. Skill versions and failure guards are also removed when their
non-default eval tenant becomes empty; shared/default-tenant assets are never
selected merely because an eval-only person exists there.

It also removes on-disk residue: per-case `eval-<case>-<nanos>` tenant
directories that historic runs minted under the config home (skills base
default) and the data dir (per-tenant memory stores). The scan is strictly
verifiable, never a generalized recursive delete — a directory qualifies only
when its name matches the eval tenant pattern, it is a direct child of one of
those two roots, and it contains only known eval artifacts (memory store files
and/or a skills subtree). Anything unrecognized is reported and skipped. New
runs no longer leave these directories behind: the harness overrides the
skills dir into its throwaway temp dir and sweeps its own tenant dir on close.

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

   This writes a private draft to `evaldrafts/captured/` and copies that turn's
   cassette into `.vcr-drafts/<case-id>/`. Both roots are gitignored: a captured
   prompt is not release evidence until somebody reviews it. Capture rejects a
   flight cassette containing a provider failure; rerun the turn successfully
   instead of promoting a timeout or transport error as fixed model behavior.

3. **Edit the draft** to encode *what should have happened* — fill in
   `assert_state` (e.g. a file that must exist, a task that must continue) and/or
   `expect`. The recorded model output is the negative example; you pin the
   desired terminal state.

4. **Promote it.** Replay the draft directly, then move the reviewed YAML into
   the appropriate `evalcases/<suite>/` directory and its cassette directory
   into `.vcr/<case-id>/`. Only then does `selfmind selfcheck` treat it as
   release evidence. A model-backed promoted case without its cassette fails
   the gate immediately.

What this catches well: UX / harness / behavior regressions (mojibake, raw-JSON
or tool-XML leaks, broken continuation, wrong workspace, missing tool events,
empty output, file/task terminal state) — these are deterministic under the
recorded model outputs. Pure model-quality issues ("the answer was wrong") are
documented by the captured case but only go green after a prompt change + a
re-record, so treat those as a benchmark / model-routing signal (see the
`dayinlife` scenario-5 provider-resilience probe).

Live eval runs do not participate in flight recording. Only an explicit eval
VCR `record` or `replay` mode assigns the case-id session; this keeps eval
timeouts from creating orphan `~/.selfmind/flight/<case-id>` directories.

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
  min_progress_updates: 1
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

`min_progress_updates` counts visible assistant prose that is immediately
followed by a tool call. It does not count kernel-generated `agent.thinking`
activity, so it can guard the actual pre-tool narration message path.

`setup.skills` seeds managed `agent-created` Skills into the isolated control
tenant before tool registration. This is distinct from placing a workspace
Skill under `setup.files`; it lets a regression case prove hidden compatibility
registration and provider-catalog exclusion on the real startup/message path.

```yaml
setup:
  skills:
    - name: release-check
      description: Inspect the exact release version
      content: |-
        Read release.txt with a read-only tool and report its exact value.
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

Every model-backed case under `evalcases/` requires a committed cassette. This
is derived from `model_required` (default `true`), not from whether somebody
remembered to set an optional flag. Missing evidence is always a gate failure.

Deterministic gateway/control-plane cases that cannot reach a model declare
`model_required: false`; they execute offline without a cassette. Replay still
fails closed if such a case unexpectedly reaches the provider path.

`require_cassette: true` is retained for the smaller set of north-star cases
that must also run in `local-fast`: it prevents measured-slow/profile caps from
excluding the case. It does not make other model-backed cases optional.

```yaml
id: continuity_resume
require_cassette: true
```

## Selfcheck Profiles And Ownership

`selfmind selfcheck` replays provider responses strictly offline
(`SELFMIND_EVAL_VCR=replay` + `SELFMIND_EVAL_OFFLINE=1`). Replayed tool calls
still execute against the current host toolchain. Cases that depend on host
commands declare them explicitly:

```yaml
requires:
  commands: [node, npm]
```

An unowned missing tool fails with exit code `2`. When a case is explicitly
owned by CI for the current platform, a local machine missing that tool reports
`CI-DEFERRED` and excludes only that case. This is not standalone release
approval: the matching GitHub Actions job must pass. Profiles divide ownership
without changing cassette semantics:

- `local-full` (default) replays every release case the local host can prove and
  reports any explicit CI delegation. Run it before pushing.
- `local-fast` / `--fast` skips measured `slow: true` cases for edit loops.
- `ci` runs only cases whose value specifically depends on a clean checkout,
  credentialless host, another platform, concurrency, or timing.

```yaml
ci:
  required: true
  reason: cross_platform
  platforms: [linux, darwin]
```

Valid reasons are `clean_checkout`, `cross_platform`, `credentialless`,
`concurrency`, and `timing`. A CI-owned case must have a cassette; missing one
fails by identity, so CI does not rely on a numeric case-count proxy.

A release requires both `selfmind selfcheck` and the Linux/macOS Actions jobs.
Linux CI replays the complete `local-full` corpus from a clean, credentialless
checkout; macOS additionally runs the cases explicitly owned by its native
platform. Release tags repeat the complete Linux gate for the exact publish
SHA. CI also runs race-sensitive runtime tests and packages, installs, and
launches the npm distribution on both platforms before publication. Local
success cannot substitute for those checks.

Before replay begins, selfcheck prints one bounded coverage line per suite:
valid cases, recorded cassettes, providerless cases, selected/runnable cases,
CI-deferred cases, and missing evidence. Missing model cassettes are named and
fail. This makes a green run state exactly what it proved.

Three exit codes keep the gate honest:

- `0`: all selected checks passed.
- `1`: a build, test, or behavioral assertion regressed.
- `2`: arguments or the verification environment are unavailable, including a
  missing Go toolchain, repository root, eval directory, or required command.

`SELFMIND_EVAL_MIN_CASES=N` remains a compatibility guard for custom local
automation, but the repository CI uses explicit case ownership instead.

### Cassette hygiene

Four properties are enforced by `internal/kernel/llm/vcr_corpus_test.go`, so a
regression shows up in `go test ./...` rather than as a CI-only mystery:

- **No machine absolute paths.** Recording rewrites the workspace prefix to
  `{{SELFMIND_VCR_WORKSPACE}}` and replay expands it to the current run's
  workspace. Cassettes recorded before that mechanism existed carried the
  recording machine's paths verbatim, so their case passed locally and failed
  in CI for weeks with every replayed tool call pointing at a missing
  directory.
- **Recorded provider failures do not grow.** A failed call MUST still be
  recorded — replay is ordinal, so a hole desynchronizes every later call in
  the case — but a committed failure cassette replays that failure forever and
  the call it stands for is never verified again. Recording one now prints a
  loud warning, and the corpus test ratchets the known set. Clearing an entry
  requires a live re-record.
- **Valid, contiguous recordings.** Every numbered file is valid JSON; each
  case starts at `0000.json` with no ordinal gaps; a directory with no numbered
  cassette is rejected.

Strict replay also fails when a provider call has no VCR session. Eval YAML is
decoded with known-field validation, so a misspelled assertion cannot be
silently ignored. The harness may clean up a leaked running run to keep its
isolated database reusable, but that cleanup is a failing case result rather
than evidence of successful finalization.

### Tiers: `--fast` and the full gate

Cases whose measured host-tool replay dominates the edit loop carry
`slow: true`. `selfmind selfcheck --fast` skips only those non-mandatory cases
and prints the count; a `require_cassette` north-star case is never skipped.
The full gate still runs every locally provable release case before pushing.

Do NOT filter on `max_duration_seconds` instead. It is an author-chosen ceiling
with little relation to cost: `continuity_resume` declares 420s and replays in
one second, while the genuinely slow case declares 540s.

### Recording a `workspace: "."` case

Those cases run their tools against the repository itself, so record them from
a clean checkout of the commit you are going to push — `git archive HEAD | tar
-x -C <dir>` — and never from a dirty tree. Recorded against a tree carrying
uncommitted files, the model's answers describe files CI does not have, the
replayed tool calls return something else, and the case fails on CI while
passing locally. Recording also gets its own turn budget (a floor well above
`max_duration_seconds`, which is a replay assertion) because live runs pay full
model latency — measured at roughly seven times replay.

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

## Skill Lifecycle Suite

`evalcases/skill-lifecycle/` protects the production message path for bounded
Skill context and work-unit activation. Its cassette-backed cases prove that a
task-bound Skill survives a CLI-to-IM continuation without injecting the Skill
directory, one run can switch between two independently selected Skills, and
unbound work receives a budgeted full candidate catalog containing a seeded
agent-created Skill without model-visible per-Skill provider tools. Catalog
assertions cover included/shortened/omitted allocation; active delivery
assertions cover the immutable receipt. The suite
reads durable `skill.activated`, `plan.updated`, and `context.selected` events
rather than inferring behavior from the final prose.

Work-unit identifiers and work-unit-scoped Skill candidate refs are allocated
from the current control state, so their cassette forms are request-relative
(`{{SELFMIND_VCR_WORK_UNIT_N}}` and `{{SELFMIND_VCR_SKILL_REF_N}}`). Replay maps
those placeholders from the current request messages; raw recording-time UUIDs
or `candidate_ref` values are invalid release evidence. This normalization is
VCR-only and does not weaken production scope validation.

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

## Corpus maintenance

`evalcases/` is the active release corpus, not an archive or brainstorming
folder. A case belongs there only when it protects a distinct user-visible or
stateful boundary with deterministic assertions. Prefer Go tests for pure
algorithm/storage mechanics; use an eval when the production message path,
model/tool contract, cross-endpoint rendering, or final world state matters.

When two cases protect the same boundary, keep the stronger one. Delete stale
or weak duplicates together with their cassette directory. The corpus guard
rejects duplicate IDs, missing model cassettes, providerless cases carrying
cassettes, and orphan cassette directories, while the VCR guard rejects invalid
JSON, ordinal gaps, recorded provider failures, empty directories, and machine
absolute paths.

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
