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
selfmind eval compare before.md.json after.md.json # advisory cost/quality comparison
selfmind eval canary init /tmp/my-new-canary # create private synthetic live fixtures; no calls
selfmind eval capture [latest]     # promote the last recorded turn into an eval case
selfmind eval clean [--yes]        # remove historic eval residue (control.db rows + on-disk eval-* dirs)
```

`eval run` writes JSONL logs under `evalruns/YYYY-MM-DD/` by default. The
directory is intentionally ignored by git.

Cases may opt into `approval_mode: smart`. The harness then installs the same
cheap-role approval judge as the daemon and preserves smart mode for external
watch continuations. Existing cases default to full-auto. No person answers
an eval approval, so in every mode an unanswered ask, such as a full-auto
`network:shared` escalation, fails after a short settle instead of parking
until the turn deadline. An
`assert_state` predicate with `on: approval_triage`, `status: approved`, and
`count_gte: 1` proves that the model funnel ran; `on: approval` with
`count_lte: 0` detects unexpected human asks. Triage counts cover the isolated
case person's turns, not the operator's account. Each turn may also declare
`assert_state`, checked immediately after that foreground turn, so a later
success cannot hide an effect performed before confirmation. New cassettes
version their plan-ID binding and prefer the newest plan evidence, keeping
old tool-result aging from shifting current step IDs. Existing cassettes retain
their original binding contract.

Tool-error limits keep all failures and refusals visible. A case may additionally
set `max_invoked_tool_errors: 0` to reject failed or uncertain tool attempts while
allowing bounded preparation recovery under `max_tool_errors`. Only runtime facts
with both `invoked: false` and `effect_state: not_dispatched` qualify as proven
non-dispatch; missing, untyped or unknown-effect facts remain failures. No error
message, model name or scenario-specific code grants an exception.

Optional requests deliberately canceled by their caller record `caller_canceled`
and replay a cancellation, never a successful response. The marker requires both
a canceled caller context and a cancellation error; unrelated provider errors,
including timeouts, remain failures. Conflicting output/error markers are invalid.
This keeps overlapped recall recordable without weakening the provider-failure gate.

Natural-task efficiency checks use the same input, fixture and route before and
after a change, without instructing the model which tool sequence to use.
Report task correctness, verification, tool failures, Main calls and elapsed time
separately; count refusals and retain failed provider samples. An isolated summary
checks recorded success, a human wait and stale local evidence without mutation.
A small live sample establishes protocol coverage, not sustained speed or parity.

## Efficiency evidence and live canaries

Scorecards keep every repeat sample, including startup errors. Repeated record mode keeps separate `<case>-sN`
sessions in the configured VCR directory; no attempt overwrites its predecessor.
Select an intact sample explicitly when promoting a canonical release cassette.
Aggregate
`RunResult` counters and duration are totals; sample medians are calculated from
`samples`, never from the last successful run. Markdown has a private JSON
sidecar (`<report.md>.json`) with each sample's checks and evidence path.
`eval report` and scorecards use the same efficiency projection. Lifecycle means
`update_plan` and `finish_run`; `verify` is reported separately. Main usage counts
include failed provider requests, exclude proven local deferrals, and separate
other/unknown roles. Missing historical usage stays unknown.

Finish first-pass measures the first tool receipt for each identified Run that
attempts finish, not successful durable delivery or task correctness. Closing
model cost starts with that first attempt's generating call and includes later
recovery on the same Run; it is not a claim that all closing work is waste.
Missing Run identity or timing is not inferred from the last three calls. Judge
latency is not added again to enclosing tool intervals.

`eval compare <before> <after>` reads JSON sidecars, raw JSONL, or directories of
JSONL (not both originals and sidecars). It retains incomplete and unmatched
samples. Sidecars also retain failures before a JSONL log could be created; use
them for sampled experiments. Live latency deltas require equal sample counts and matching fixture hashes, route, reasoning,
model/sandbox policy and execution mode; build identities may differ. Unknown
provenance and replay show no live speed claim. This is advisory output, not a
new selfcheck or release gate. Policy hashes cover model selections and sandbox
configuration, not every host/environment difference: hold those constant too.

`eval canary init <new-directory>` writes five synthetic, isolated fixtures:
cross-project history, a code repair with existing tests, a negative investigation,
a tool-free answer, and a confirmation/continuation with a pre-confirmation
no-write assertion. Each requests three samples on the configured route in
smart mode; task assertions determine quality and tool errors remain separate
friction metrics rather than being hidden by the final success; no tool sequence or minimum round count is taught. Generation makes
no provider call. Explicit `scorecard <directory> --live --out before.md` incurs
provider usage; run both builds against the identical fixture directory and
configuration, alternating order when collecting repeated comparisons. Three
samples are smoke evidence, not a tail-latency or sustained-quality estimate.
These starter fixtures do not yet prove long Git histories, real IM, or recovery
under sustained load. Keep them outside the committed replay corpus.

Replay reports distinguish its configured route from the actual recording
sources; a playback configured for Qwen never makes a legacy tape a Qwen sample.
New recordings carry advisory `recorded_with` metadata: time, resolved model/role,
build and available schema/prefix hashes. Legacy provenance stays unknown; file
mtime is not treated as a recording date. Fingerprint drift does not fail replay.
Caller-canceled optional work retains cancellation semantics rather than being
limited by a fixed allowed count. Actual provider failures remain forbidden in
release recordings.

## Data Isolation (default)

Each turn's outcome assertions read that turn's returned state. An absent
verification verdict does not inherit a preceding turn's `passed` or `failed`.
When a case waits for a watcher continuation, its final assertions use that
child's actual outcome, including absence of verification. Adopted evidence
cases also assert the source reference in the durable final outcome, so a
historical verdict alone cannot satisfy the continuation gate.
Plan-ID placeholders support legacy UUID and compact Run-namespaced identities
in both directions; shortening references does not require re-recording unrelated
cases. The adoption case also requires status plus a complete final body without
duplicated summary arguments on both turns.
The changed-criterion case allows a genuine negative check but separately
rejects invalid-input and stale-reference errors in durable tool events; an
expected failed check cannot hide a contract-repair loop.

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
Artifact and external-effect claim references are rebound from the current
request during recording and replay, like Run identities. An identity absent
from the current request is never invented or granted replay authority. Corpus checks reject literal recording-time artifact
IDs; artifact-read cases must also assert zero tool errors so an answer in a
cassette cannot hide a failed read.

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

3. **Review the isolated draft** before execution. Capture preserves private
   directory/file permissions (0700/0600), refuses overwrites, and sets
   `capture_review_required: true`. Reconstruct `setup` and its ground truth, then
   clear that flag. Raw recordings do not restore files, remote state or approvals,
   and replay really executes tools. Fill in
   `assert_state` (e.g. a file that must exist, a task that must continue) and/or
   `expect`. The recorded model output is the negative example; you pin the
   desired terminal state.

4. **Promote it.** Replay the reviewed draft, then move the reviewed YAML into
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

`expect.verification_state` checks the legacy execution/check projection.
`expect.condition_state` checks Main's evidence-bound conclusion (`met`,
`not_met`, `unknown`, or `unassessed`). These are not interchangeable: a
zero-exit observation can establish a negative condition. Each final turn or
watcher child owns both projections; an earlier turn's verdict cannot satisfy
the later assertion. Pair condition assertions with durable Run/Plan state and
unchanged-file checks. The investigation case completes with a negative finding;
the changed-required-criterion case retains that negative condition and blocks
completion. Failed methods and wrong references remain observable errors.

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

For deterministic exact-Run controls, `setup.task.parked_runs` may seed a
resumable Run and `seeded_run_ordinal: 1` appends its generated ID to that
turn's `input`. The ordinal refers to the setup list, not a recent-work UI
position; this permits owner and stranger probes without a hardcoded Run ID.
A parked Run may also carry `unresolved_effect` (`summary`, `exit_code`, and
an optional `target`, by default the person-wide unknown target). Before the
Run parks, the harness claims the target, records the dispatch with that
redacted summary, and records a return with that exit status through the
production store, so the claim is left uncertain. `seeded_effect_ordinal: 1`
appends that claim's ID to a turn's `input`, whatever state it has reached.

A turn may set `wait_for_external_watches: true` in an isolated workspace/data
case. After a real `waiting_external` handoff the harness starts the production
watch worker and waits for its exact-parent Main continuation to finish. Detached
provider calls share the case's VCR/telemetry context; real file and process tools
still execute. The isolated identity uses the same full-auto preference as its
foreground eval turns, with the safety floor intact. The final state assertions
inspect the continuation Run and original Thread events. This proves execution
closure, not external IM delivery; approval and restart mechanics have separate
Go coverage. Other cases do not start a watch worker.

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

Event assertions may use `payload_fields` to match exact string values by dotted
JSON paths (for example `tool: update_plan` and `args.mode: patch`). JSON-encoded
object strings are decoded while traversing, so whitespace/key order do not
change a tool-argument assertion. Missing, malformed or non-string values do not
match; all fields must match the same event. Count bounds still apply.

## Multi-Run Runtime And Endpoints

A case runs one Run on one agent worker unless it declares `runtime`. The
harness then builds that runtime through `app.InitGateway` and the production
worker pool, with fixtures in place of operator services:

```yaml
requires:
  commands: [go]            # mcp_stubs builds the stub from the source checkout
runtime:
  max_active_work_runs: 2   # gateway.max_active_work_runs, 1-3
  workers: 2                # defaults to the capacity and may not be lower
  cron: true                # every worker offers the cron tool; no job runs
  mcp_stubs:                # stdio servers backed by internal/tools/mcptest
    - name: stub
```

The capacity sets both the process registry and the durable admission check,
as in the daemon. Without `cron` the harness builds no cron scheduler; with it
the scheduler store exists but its loop never starts. MCP stubs are added to
any configured servers. Every worker registers a stub's tools as
`mcp_<name>_<tool>_<hash>`, and all workers share one stub process. Building
the stub needs the Go tool and the SelfMind checkout, so such a case declares
`requires.commands: [go]`. The build has a two-minute limit, and in strict
offline replay it may use only the module cache on disk (`GOPROXY=off`). When
the environment cannot supply the stub (no Go tool or checkout, modules or a
toolchain it cannot fetch, or the time limit), the case is UNAVAILABLE with the
reason. A stub that does not compile is a regression and fails the case.

Turns can come from the case person's other endpoints:

```yaml
turns:
  - input: "Draft the release notes for the parser"
    platform: cli          # the sending endpoint; default: the eval account
    channel: session-a     # for the CLI, one terminal session
    cwd: parser            # CLI working directory inside the isolated workspace
    hold_worker: true      # submit without waiting; the Run keeps its worker
  - input: "/stop"
    platform: cli
    channel: session-a
    turn_run_ordinal: 1    # appends the id of the Run that turn 1 started
```

`platform` sends the turn from that endpoint, such as `cli`, `weixin` or
`telegram`. The case user on any platform is the case person's own account
and sees the person's work there. A different `platform_user_id` stays a
stranger unless `setup.accounts` binds it. `cwd` (CLI turns only) creates the
directory if needed, and the gateway binds the workspace from it as it does
for the terminal client. Two sessions in different directories therefore do
not share a write lock.

`hold_worker` makes overlap deterministic without a model. The harness submits
the turn and waits until its Run has leased an agent worker, then runs the
next turn while the Run keeps that worker. The Run makes no model call and ends
only when it is stopped, normally by a later `/stop` with `turn_run_ordinal`.
That cancellation is the expected outcome, not an error, and the case status
stays that of its last foreground turn. A held turn fails as
`turn-N:hold_leased_worker` when its Run never leased a worker. It fails as
`turn-N:hold_stopped` unless a stop ended the hold and the Run is stored as
cancelled; the turn deadline and the run watchdog's stall cancellation do not
count. A hold the case never stops is released when the case ends and its Run
is cancelled, so no automatic recovery Run follows it. A held turn's
`assert_state` is checked as soon as its Run holds the worker. Held Runs count
toward `require_no_run` and the runtime counts, but the subject of case-level
`run` and `task` assertions is the last foreground turn's.

Model-backed turns still run one at a time. Concurrent model calls would
interleave the case's single cassette ordinal stream, so overlap evidence
comes from `hold_worker`.

Multi-Run IM routing reads state that a case seeds through the production
store:

```yaml
setup:
  accounts:                # further accounts of the case person
    - platform: weixin
      platform_user_id: wx-second-phone
  task:
    parked_runs:
      - input: "Compile the nightly report"
  turn_choices:            # a pending multi-Run choice on one channel and account
    - platform: weixin     # account platform/user; default: the eval account
      channel: wx-chat-1
      request: "Also attach the error summary"   # what choosing a number submits
      options:             # keys 1..n in list order
        - action: resume   # steer and resume name a parked Run
          parked_run: 1
          label: "Resume the nightly report"   # optional shown text
        - action: new
  sent_messages:           # a delivered IM message and its reply edges
    - platform: weixin
      channel: wx-chat-1
      message_id: wx-msg-approval-1   # the platform id a native reply names
      approval_id: approval_missing   # literal; may name a missing approval
      content: "Approve the export?"  # optional delivered text
```

A turn's `native_reply_message_id` then resolves, on that channel only, to the
seeded message's `parked_run`, `approval_id` or `clarify_id` edge. A seed's
account must be the case user on some platform or a `setup.accounts` entry.
Without a `label`, an option shows `Resume <run>`, `Update <run>` or `This is
new work`; without `content`, a message names its `message_id`. The choice
stores the request exactly as the gateway does, with the case's approval mode
and workspace.

A turn may also arrive as stored text instead of the person:

```yaml
setup:
  task:
    parked_runs:
      - input: "Deploy the service to staging"
  approvals:               # a pending approval the parked Run waits on
    - parked_run: 1
      command: "rm -rf build"
turns:
  - input: "/approve"
    scheduled: true        # fired as a job of the turn's account
```

`scheduled: true` fires the input as the prompt of a scheduled job through the
daemon's cron executor, the path a job's stored text takes when it fires; the
turn's answer is what the executor delivers. The job belongs to the turn's
`platform` and `platform_user_id` account and delivers to its direct chat, so
a scheduled turn carries no channel, reply or approval edge, roots, cwd, or
held worker. A seeded approval is a terminal command the parked Run asked
about; it is parked with its Run, so answering it resumes that Run.

`assert_state` counts these runtime facts with `count_gte`, `count_lte` or
`exists`. Each needs one that can fail: counts are never negative, and
`count_gte: 0` alone is refused. A case also fails to load when a count names a
status or target that cannot match. Run statuses are `running`, `done`,
`completed`, `failed`, `cancelled`, `interrupted`, `waiting_user`,
`waiting_external`, `verification_partial` and `blocked`; queue statuses are
`queued`, `started`, `done`, `failed` and `cancelled`. An `mcp:` target or an
`mcp_server_starts` target must name a `runtime.mcp_stubs` server exactly as
declared, and a tool of the stub (`echo`, `rendezvous` or `record`, a writing
tool whose annotations falsely claim `readOnlyHint`). Any other tool
target must be a tool some worker registers, or `cron`; an unknown name fails
the assertion instead of counting zero.

| `on` | Counts |
| --- | --- |
| `workers` | agent workers built for the case |
| `worker_tool` + `target` | workers offering the tool: `cron` or `mcp:<server>/<tool>` |
| `worker_catalogs` | distinct worker catalogues; `count_lte: 1` means identical |
| `mcp_server_starts` + `target` | processes started for that MCP stub |
| `worker_leases` | Runs that leased an agent worker |
| `leased_workers` | distinct workers those Runs used |
| `worker_overlap` | the most Runs that held workers at the same time |
| `worker_tool_calls` + `target` | distinct workers on which a Run completed the tool |
| `runs` + optional `status` | the person's Runs |
| `queued` + optional `status` | the person's queue rows, in any status by default |
| `turn_choices` + optional `status` | the person's seeded or shown choices: `pending` or `resolved` |

```yaml
assert_state:
  - on: worker_overlap
    count_gte: 2
  - on: worker_tool
    target: mcp:stub/echo
    count_gte: 2
  - on: turn_choices
    status: pending
    count_gte: 1
```

Worker facts come from a lease hook on the gateway
(`router.Gateway.SetWorkerLeaseHook`), which the daemon does not install.
`worker_tool_calls` counts only completed calls. An unmarked MCP stub tool is
an unclassified external tool, which asks a person in every approval mode, and
no person answers in eval. A case that must call one first marks the exact
tool read-only with a `/tools readonly <server>/<tool> on` turn; otherwise it
shows MCP tools as offered and proves calls with internal tools such as `cron`.
The stub's `record` tool writes, so a case must not mark it.
As in the daemon, the primary worker's engine executes durable background
reviews (`Server.SkillReviewer`), so a review that a worker queued runs in the
case's `wait_for_maintenance` pass.

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
Pull requests run `local-fast` for quick feedback. Main CI runs `local-full`
from a clean, credentialless checkout, plus focused race tests and package
smoke as parallel jobs; macOS additionally runs the cases explicitly owned by
its native platform. Prerelease publication requires that exact main SHA's CI
run to have succeeded instead of replaying the same corpus a third time. Stable
releases repeat the complete source gate. Local success cannot substitute for
those checks.

Before replay begins, selfcheck prints one bounded coverage line per suite:
valid cases, recorded cassettes, providerless cases, selected/runnable cases,
CI-deferred cases, and missing evidence. Missing model cassettes are named and
fail. This makes a green run state exactly what it proved.

Three exit codes keep the gate honest:

- `0`: all selected checks passed.
- `1`: a build, test, or behavioral assertion regressed.
- `2`: arguments or the verification environment are unavailable, including a
  missing Go toolchain, repository root, eval directory, or required command,
  or an MCP stub a case cannot build.

`SELFMIND_EVAL_MIN_CASES=N` remains a compatibility guard for custom local
automation, but the repository CI uses explicit case ownership instead.

### Per-user state stays out of the gate

Developers run the gate where their daemon lives, so nothing it starts may
touch their `~/.selfmind`, caches, or config. `go build` and `go test` run with
a throwaway HOME (XDG directories below it, `SELF_CONFIG` cleared) at a stable
per-checkout path, so Go's test cache still recognizes unchanged packages;
`GOCACHE`, `GOMODCACHE`, `GOPATH` and `GOENV` keep the values `go env` reports
for the real environment, so nothing is rebuilt or re-downloaded. That path is
used only under an exclusive lock, in a sticky parent owned by root or the
user, and when nothing but the user's own directory is already there; a
second selfcheck of the same checkout, or anything planted at the path, gets a
HOME of its own instead and loses only the cache benefit. If tests
leave a `.selfmind*` tree or SelfMind's cache directory
(`Library/Caches/selfmind`, `.cache/selfmind`) in that HOME, the gate fails and
names the paths. Replay runs under its own throwaway HOME and reads the explicit
or default config only when that file exists, using built-in defaults
otherwise; `selfmind selfcheck` and `selfmind docs` never create
`~/.selfmind/config.yaml`.

A test package that reaches HOME-derived paths isolates its whole binary with
`func TestMain(m *testing.M) { testhome.Main(m) }` from
`internal/testutil/testhome`. HOME, the XDG directories and `SELF_CONFIG` then
point at a scratch home made for that binary — outside the system temp
directory, so execution runtime roots under HOME still work — which is removed
at exit. `t.Setenv("HOME", ...)` still works, and a helper process that
re-executes the same binary keeps the environment it was started with.

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
one second.

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

Durable plan step IDs use `{{SELFMIND_VCR_PLAN_STEP_N}}` under the same
request-aware mapping. Each replay uses the current Run's issued step IDs;
recording-run IDs must never be copied into a new Run or committed cassette.

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
- `internal/eval/multirun.go`, `held_turns.go`, `worker_leases.go`,
  `im_seeds.go` and `runtime_facts.go` build the declared runtime, hold Runs on
  workers, seed endpoint state and evaluate the runtime assertions.
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
