# SelfMind Implementation Status

> Current-state snapshot and the repository's only priority list. Product
> direction and acceptance scenarios live in
> [`identity-continuity.md`](identity-continuity.md). Domain mechanics live in
> the generated [`README.md`](README.md) index. Code and tests remain the source
> of truth.

**Snapshot:** 2026-08-27

## Release Health

- `GOWORK=off go build ./...`: passing at the snapshot.
- `GOWORK=off go test ./...`: passing at the snapshot.
- Release corpus: 56 reviewed YAML cases. Model-backed cases carry committed cassettes; deterministic cases
  declare `model_required: false`.
- `selfmind selfcheck` is the release gate. It always checks the documentation
  contract, then build/test and provider-offline eval according to profile.
- Linux and macOS x64/arm64 are packaged targets. Native Windows remains
  unsupported; WSL is the supported Windows route.

## Product Gates

| Gate | State | Evidence still required |
| --- | --- | --- |
| Personal daily-driver | Partial | Continue real coding/operations use and close regressions from daily run reviews. |
| Phase-1 continuity | Partial | CLI-to-IM approval now has process-level presence, detached-immediate/T1 escalation, cross-endpoint resolution, parked answerability, and daemon-restart continuation recovery. Repeat the full live scenarios across real IM transports and stranger isolation. |
| npm beta distribution | Partial | Clean tagged release through GitHub Actions plus install/update/daemon-restart verification on Linux and macOS. |
| SaaS / enterprise | Deferred | No implementation until maintainers approve a dedicated strategy decision and its evidence gates. |

## Capability Map

| Area | State | Current boundary |
| --- | --- | --- |
| Daemon gateway | Done | CLI, IM, cron, and HTTP use one daemon-owned runtime, queue, auth manager, and control database. |
| Identity and continuity | Done | Person identity spans bound endpoints; transcripts stay local; tasks, runs, approvals, handoffs, and memory are durable. |
| Agent loop | Done | Native tools, structured outcomes, bounded elastic budgets, cancellation, retry classification, planning, evidence-derived verification, and an operator-owned prompt workspace are implemented. Prompt files are startup-frozen, strictly validated, semantically hashed, revision-pinned for durable maintenance, and cannot remove locked quality/safety contracts; an invalid active workspace degrades visibly to the matching last-known-good snapshot or built-in defaults without taking CLI, IM, cron, and HTTP agent work offline. An always-on foreground delivery and evidence floor also covers tool-free direct answers. Tool guidance is derived from each role's actual capabilities; background review uses a bounded memory/session surface; delegated workers preserve scoped parent evidence without inheriting parent lifecycle or loop state; and compaction preserves verification, failed attempts, waits, identifiers, and files behind an untrusted-data fence. Legacy files migrate recoverably on edit, current revision caches self-repair, and restored historical revisions can explicitly resume paused work. |
| Execution engine | Partial | Typed scopes, environment snapshots, sandbox policy, durable watcher execution, and tool profiles exist. Authenticated local CLI runs support repeatable, invocation-local `--add-dir` roots that are frozen across queue/recovery, included in scoped tools and project context, and conflict-scheduled by overlapping physical paths without changing workspace trust. Linux isolation is strongest; macOS uses approval-controlled host execution. |
| Worker scheduling | Partial | Durable queue and worker-pool seams exist; personal edition intentionally defaults to one active run per person while multi-run ownership remains deferred. |
| Provider runtime | Done | Main is the sole foreground authority; Background is the default for six stable maintenance roles, each with an optional explicit override. `selfmind model` and bare TUI `/model` open one Model Manager that keeps a multi-route draft, automatically validates every completed selection, acquires missing API keys without placing them in YAML, reviews once, and applies one generation-checked daemon transaction. Its verified running snapshot is the sole Model Readiness authority; onboarding migrates legacy receipts once and resumes runtime repair without repeating model prompts or probes. Changes preserve compatible tuning, stage YAML writes until an uncancellable safe boundary, never interrupt an active run, queue new work across restart, require real post-start `/health`, automatically roll back only model-attributable failures, and expose retry/restore for infrastructure failures. Protocol adapters, typed quirks, generic request extras, live/cache/stale discovery, manual model IDs, metadata, and auth refresh remain implemented. |
| Provider cost visibility | Done | OpenAI-compatible and Responses cache usage is normalized; role/VCR wrappers preserve adapter request prefix/block fingerprints and report explicit unsupported states without storing prompt content; `/diag context` distinguishes total provider requests from prompt-only assembly, while `selfmind usage` and `selfmind report daily` provide paged local execution/token trends, schema share, and approval-continuation attribution. Provider pricing remains external. |
| Context lifecycle | Done | Person work spine, bounded composer slices, project instructions, deterministic workspace-knowledge indexing, artifacts, recall, and compaction are integrated. |
| Memory | Partial | Canonical governance, pin/correct/forget, transient filtering, FTS-safe lexical/CJK retrieval, JSON-fenced and narrowly bounded multilingual query expansion, access tracking, audits, output-overlap recall telemetry, and per-run intake disposition counts exist. Governance due state is durable per person, catches up overdue work after startup, retries foreground deferral promptly, distinguishes bounded partial progress from a complete scan, and exposes remaining backlog plus report age/scheduler reasons. These signals are diagnostic rather than proof of causal use; quality, reuse, and duplicate rates still need sustained measurement. |
| Tasks | Done | Inbox, lifecycle fields, paging/search, pin/archive/rename/merge, retention, governed Task References, audited attach policies, explicit dry-run legacy-reference migration, and asynchronous post-run labeling exist. Semantic references never grant workspace or prior-run authority; ticket-shaped work keys are display hints only. |
| Background maintenance | Done | Debounced bounded batches, immutable replay jobs, restart-safe retry exhaustion, shared retry policy, stable semantic roles with a shared auxiliary floor, provider/contract circuit identity, diagnostics, migration tools, and dispatch-time reasoning/output bounds exist. |
| Skills | Partial | Runtime discovery uses a budgeted metadata catalog and server-issued candidate refs; provider catalogs contain no per-Skill tools. Model, slash, and binding paths converge on one immutable package activation with context-proportional main delivery, explicit section/resource paging, compaction protection, active/candidate/previous/quarantined versions, and Doctor receipt checks. Automatically learned Skills default to a control-managed logical-workspace root outside the repository and are not discoverable from another workspace. Curator authorization uses the exact production delivery builder, paged legacy repairs are non-growing, bundles share one executing-agent budget, and `/skills stats` derives from durable activations/work-unit outcomes. The cassette-backed local-full release gate is green; sustained production and installed-binary/daemon evidence remain open. |
| Safe self-evolution | Partial | Terminal work-unit observations, neutral parked waits, comparable cohorts, frozen curator package proposals, environment-bound failure guards, evidence snapshots, quarantine, and compatible-previous rollback checks exist. Ordinary workflow success is observation only and cannot increment shadow matches, revive degraded candidates, or enable `batch_read`; runtime advice requires a separately verified comparison contract that the current profiler does not create. Three independent, comparable, verified work units may publish a workspace-scoped Skill when their procedures use eligible built-in tools, without granting execution authority. Repairs combine declared and daemon-observed categories: deterministic interface drift may publish after one verified recovery, workspace-scoped stable preconditions after one, semantic drift after three independent recoveries, and not-applicable/transient evidence cannot auto-publish. Schema v5 persists dependency/environment fingerprints and last verification time for bounded review nominations. User-global widening and sustained real-workflow validation remain open. |
| Tool safety | Partial | Safety floor, smart approval, typed invocation scope, grants, hash-bound trusted observation scripts, secret redaction, schema governance, sandbox/host profiles, and model-safe typed failure envelopes exist. External MCP tools use the official Go SDK over stdio or Streamable HTTP, with paginated discovery, live catalogue updates, collision-safe names, health diagnostics, internal-argument filtering, and per-schema quarantine. Direct/deferred/hidden native-tool filtering and work-unit-local monotonic activation through `tool_search` are implemented; automatic external deferral has no guessed name-hash cohort and remains code-gated pending a reviewed seven-day usage/fingerprint baseline. Unclassified MCP calls fail closed to once-only human approval in every mode. Human asks use one server-issued menu across CLI/IM: proceed once, optional run-local reuse with the exact proposed rule visible, and deny; sensitive asks are once/deny only. Unanswered asks park without rejection, later approval resumes through an exact-action one-shot capability below the current safety floor, and current live/parked backlog age is visible. Historical broader grants remain listable and revocable. External tool diversity remains an ongoing compatibility surface. |
| External watchers | Partial | Durable registration accepts only proven read-only observations, performs bounded slow-command preflight, freezes environment/auth identity, and automatically hands a successful registration off as `waiting_external` without another model turn. Polling survives restart without holding the person's active run; terminal writeback is a separate idempotent background finalization with distinct agent/external outcomes, concise TUI state, person-scoped numbered `/watchers` controls, and delivery-confirmed stable-ID notifications. Keep validating provider-specific terminal behavior and live delivery. |
| IM delivery | Partial | Weixin and other adapters share durable outbound state, delivery diagnostics, session refresh classification, bounded catch-up, preferred-channel routing, desk-first/phone-first approval surfaces, and idempotent resolution follow-ups. Old `pending_session` final results can be replaced by one exact-platform-account-and-channel recap; only confirmed recap delivery dismisses the exact summarized rows, while explicit no-send dismissal remains available. Live platform behavior remains an external dependency. |
| TUI | Done | Daemon event stream, call-id-routed and semantically colored tool cells with terminal cleanup, Codex-style queued approval decisions with explicit cancel and cross-endpoint resolution, typed transient notices, bottom plan panel, pagers, persistent input history, resume transcript, build-fingerprint detection, and the sole interactive Model Manager exist. |
| Distribution and updates | Partial | npm platform packages, launcher, resumable runtime/first-use setup, unified `selfmind update` notices, equal-version package refresh, feedback, and per-user macOS launchd/Linux systemd service management exist. Managed readiness uses a non-secret service generation plus running job, version, configuration identity, and effective-route fingerprint; replacement drains active work without force, waits for runtime ownership release, and performs at most one proven-safe bootstrap retry. Compatible active Gateways remain usable as Runtime Degraded rather than being force-killed or falsely reported healthy. `control.db` has an explicit compatibility version, verified pre-migration backups, historical-state invariants, a restore command, and strict post-restart build/schema health. Public beta still requires released-version upgrade fixtures plus Linux/macOS rollback evidence. |

`Done` means the capability is implemented and covered at its current personal
edition boundary. `Partial` means usable with a known evidence gap or platform
limitation. It does not mean the area should be redesigned from scratch.

## Highest-Value Next Work

1. **Accumulate release evidence on the personal edition.** Use daily-driver
   runs to measure successful completion, interruption/recovery, approval
   latency, watcher finalization, IM delivery, cache usage, and maintenance
   health. Include the cassette-backed Skill lifecycle suite, full selfcheck,
   and installed-binary/daemon verification before treating the presentation
   contract as released. Use `selfmind report daily` as the local baseline and
   fix observed correctness defects before speculative platform work.
2. **Measure memory usefulness, not record count.** Track query-relevant
   canonical recall, injection, reinforcement, supersession, duplicates, and
   user correction. Improve selection/write policy only from those traces.
3. **Validate Skill lifecycle and safe evolution on repeated personal workflows.**
   Confirm that task bindings reduce directory/context cost, work-unit Skill
   switches expire old bodies, comparable cohorts publish narrow procedures,
   ordinary write/Shell publication never bypasses execution policy, verified
   repairs change only attributable sections, guards prevent repeated bad steps,
   quarantine prevents repaired regressions from reactivation, and fallback
   still completes through ordinary planning. Collect evidence before building
   real Fast Path comparison/canary machinery; ordinary observations are not
   shadow evidence.
4. **Prepare the next npm beta only after the full gate passes.** The release
   needs a clean Action run, platform package smoke tests, fresh install,
   update, service restart, and rollback evidence.

## Known Limitations

- The personal edition deliberately uses SQLite and one daemon. PostgreSQL,
  remote control plane, Runner protocol, billing, organization seats, and
  enterprise handoff are future decisions, not active backlog.
- macOS does not yet provide Linux-equivalent process isolation. Policy falls
  back to explicit approval-controlled host execution.
- IM delivery depends on external session and platform behavior. A durable
  `sent` record is not always proof that a handset displayed the message;
  diagnostics and bounded catch-up make this visible.
- Approval waits are reachability-aware: a live endpoint or recently healthy
  IM endpoint gets the configured wait, while stale or delivery-failing
  endpoints use a short wait and park the run as `waiting_user`. The current
  personal-edition loop does not yet checkpoint and resume the exact suspended
  tool call across daemon restarts; continuing the task re-evaluates that step.
- Prompt caching is provider/protocol dependent. A stable local prefix does not
  guarantee that every provider creates or bills a cache.
- External MCP/plugin schemas are quarantined when unsafe or ambiguous. Built-in
  schema errors fail startup instead of being silently repaired at request time.
- Remote MCP supports configured headers, bearer tokens, and basic auth, but an
  interactive OAuth login and credential-management flow is not yet exposed.
- Full multi-run foreground/background concurrency and remote Runner execution
  remain design seams only. Do not infer that they are shipped from queue or
  execution-envelope plumbing.
- Self-evolution may publish repeated, verified procedures using trusted
  built-in tools to writable, unpinned workspace-scoped agent-created Skills.
  Repair thresholds depend on the daemon-derived failure class; one generic
  successful workaround is not universal evidence. It does not rewrite
  protected Skills, widen a learned Skill to user-global scope, approve
  capabilities, or authorize writes, credentials, network access, shell
  execution, or external effects. Network/delete, external-origin, and
  delegated-effect candidates remain inactive until explicit user management.

## Plan Lifecycle

- Active plan: `docs/plans/daily-driver-closure.md`, approved by the project
  owner for review on 2026-09-11. It closes observed personal-edition runtime
  reliability, evidence-integrity, memory-liveness, delivery, and tool-cost
  gaps without adding SaaS or Runner scope.
- Historical plans remain discoverable through `docs/README.md` as archived
  records or decisions. They do not contribute priorities.
- `docs/manifest.yaml` is the lifecycle registry. `selfmind docs check` enforces
  complete inventory, UTF-8, local links, translation source hashes, size
  limits, review dates, and the one-active-plan rule.
- `selfmind docs index` regenerates `docs/README.md`; the generated index is not
  edited by hand.

## Update Discipline

Update this file only when one of the capability boundaries, product gates,
known limitations, or priority order changes. Keep implementation history in
git and detailed mechanisms in the owning domain document. Do not append dated
closure reports or new active-plan sections here.
