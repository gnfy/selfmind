# SelfMind Implementation Status

> Current-state snapshot and the repository's only priority list. Product
> direction and acceptance scenarios live in
> [`identity-continuity.md`](identity-continuity.md). Domain mechanics live in
> the generated [`README.md`](README.md) index. Code and tests remain the source
> of truth.

**Snapshot:** 2026-08-21

## Release Health

- `GOWORK=off go build ./...`: passing at the snapshot.
- `GOWORK=off go test ./...`: passing at the snapshot.
- Release corpus: 54 reviewed YAML cases. Model-backed cases carry committed cassettes; deterministic cases
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
| Execution engine | Partial | Typed scopes, environment snapshots, sandbox policy, durable watcher execution, and tool profiles exist. Linux isolation is strongest; macOS uses approval-controlled host execution. |
| Worker scheduling | Partial | Durable queue and worker-pool seams exist; personal edition intentionally defaults to one active run per person while multi-run ownership remains deferred. |
| Provider runtime | Done | Primary/auxiliary routing, primary-defaulted local auxiliary onboarding, explicit role overrides, protocol adapters, typed quirks, generic request extras, model metadata, auth refresh, and live contract probes exist. |
| Provider cost visibility | Done | OpenAI-compatible and Responses cache usage is normalized; role/VCR wrappers preserve adapter request prefix/block fingerprints and report explicit unsupported states without storing prompt content; `/diag context` distinguishes total provider requests from prompt-only assembly, while `selfmind usage` and `selfmind report daily` provide paged local execution/token trends, schema share, and approval-continuation attribution. Provider pricing remains external. |
| Context lifecycle | Done | Person work spine, bounded composer slices, project instructions, deterministic workspace-knowledge indexing, artifacts, recall, and compaction are integrated. |
| Memory | Partial | Canonical governance, pin/correct/forget, transient filtering, FTS-safe lexical/CJK retrieval, JSON-fenced and narrowly bounded multilingual query expansion, access tracking, audits, output-overlap recall telemetry, and per-run intake disposition counts exist. Governance due state is durable per person, catches up overdue work after startup, retries foreground deferral promptly, distinguishes bounded partial progress from a complete scan, and exposes remaining backlog plus report age/scheduler reasons. These signals are diagnostic rather than proof of causal use; quality, reuse, and duplicate rates still need sustained measurement. |
| Tasks | Done | Inbox, lifecycle fields, paging/search, pin/archive/rename/merge, retention, governed Task References, audited attach policies, explicit dry-run legacy-reference migration, and asynchronous post-run labeling exist. Semantic references never grant workspace or prior-run authority; ticket-shaped work keys are display hints only. |
| Background maintenance | Done | Debounced bounded batches, immutable replay jobs, restart-safe retry exhaustion, shared retry policy, stable semantic roles with a shared auxiliary floor, provider/contract circuit identity, diagnostics, migration tools, and dispatch-time reasoning/output bounds exist. |
| Skills | Done | Runtime discovery, one-Skill-per-work-unit activation, deterministic task binding, active/candidate/previous versions, bounded context, explicit candidate management, catalog/bundles, tenant ownership, injected storage isolation, side-effect-free reads, archived-asset migration, recoverable orphan quarantine, and learning audit exist. |
| Safe self-evolution | Partial | Terminal work-unit observations, neutral parked waits, comparable cohorts, frozen curator proposals, failure guards, multi-version rollback, and same-task read-only `batch_read` candidates exist. Empty-procedure and external-watch evidence cannot nominate curation. Three repeated explicitly passed built-in procedures may publish writable, unpinned agent-created Skills without granting execution authority; actual host execution and network/delete/external/delegated effects remain candidates. Directly attributable daemon-observed failures followed by verified same-unit recovery can publish deterministic narrow-section repairs only for canonical curator-managed active versions, with call-id attribution, closed category mapping, and visible lifecycle events. Real repeated workflows still need production validation. |
| Tool safety | Partial | Safety floor, smart approval, typed invocation scope, grants, hash-bound trusted observation scripts, secret redaction, schema governance, sandbox/host profiles, and model-safe typed failure envelopes exist. External MCP tools use the official Go SDK over stdio or Streamable HTTP, with paginated discovery, live catalogue updates, collision-safe names, health diagnostics, internal-argument filtering, and per-schema quarantine. Direct/deferred/hidden native-tool filtering and work-unit-local monotonic activation through `tool_search` are implemented; automatic external deferral has no guessed name-hash cohort and remains code-gated pending a reviewed seven-day usage/fingerprint baseline. Unclassified MCP calls fail closed to once-only human approval in every mode. Human asks use one server-issued menu across CLI/IM: proceed once, optional run-local reuse with the exact proposed rule visible, and deny; sensitive asks are once/deny only. Unanswered asks park without rejection, later approval resumes through an exact-action one-shot capability below the current safety floor, and current live/parked backlog age is visible. Historical broader grants remain listable and revocable. External tool diversity remains an ongoing compatibility surface. |
| External watchers | Partial | Durable registration accepts only proven read-only observations, performs bounded slow-command preflight, freezes environment/auth identity, and automatically hands a successful registration off as `waiting_external` without another model turn. Polling survives restart without holding the person's active run; terminal writeback is a separate idempotent background finalization with distinct agent/external outcomes, concise TUI state, person-scoped numbered `/watchers` controls, and delivery-confirmed stable-ID notifications. Keep validating provider-specific terminal behavior and live delivery. |
| IM delivery | Partial | Weixin and other adapters share durable outbound state, delivery diagnostics, session refresh classification, bounded catch-up, preferred-channel routing, desk-first/phone-first approval surfaces, and idempotent resolution follow-ups. Old `pending_session` final results can be replaced by one exact-platform-account-and-channel recap; only confirmed recap delivery dismisses the exact summarized rows, while explicit no-send dismissal remains available. Live platform behavior remains an external dependency. |
| TUI | Done | Daemon event stream, call-id-routed and semantically colored tool cells with terminal cleanup, Codex-style queued approval decisions with explicit cancel and cross-endpoint resolution, typed transient notices, bottom plan panel, pagers, persistent input history, resume transcript, and build-fingerprint detection exist. |
| Distribution and updates | Partial | npm platform packages, launcher, setup, unified `selfmind update` notices, equal-version package refresh, feedback, service management, and macOS launchd support exist. `control.db` has an explicit compatibility version, verified pre-migration backups, historical-state invariants, a restore command, and strict post-restart build/schema health. Public beta still requires released-version upgrade fixtures plus Linux/macOS rollback evidence. |

`Done` means the capability is implemented and covered at its current personal
edition boundary. `Partial` means usable with a known evidence gap or platform
limitation. It does not mean the area should be redesigned from scratch.

## Highest-Value Next Work

1. **Accumulate release evidence on the personal edition.** Use daily-driver
   runs to measure successful completion, interruption/recovery, approval
   latency, watcher finalization, IM delivery, cache usage, and maintenance
   health. Use `selfmind report daily` as the local baseline and fix observed
   correctness defects before speculative platform work.
2. **Measure memory usefulness, not record count.** Track query-relevant
   canonical recall, injection, reinforcement, supersession, duplicates, and
   user correction. Improve selection/write policy only from those traces.
3. **Validate Skill lifecycle and safe evolution on repeated personal workflows.**
   Confirm that task bindings reduce directory/context cost, work-unit Skill
   switches expire old bodies, comparable cohorts publish narrow procedures,
   ordinary write/Shell publication never bypasses execution policy, verified
   repairs change only attributable sections, guards prevent repeated bad steps,
   and fallback still completes through ordinary planning. Continue validating
   shadow truth and turn savings for enabled read-only batches.
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
  built-in tools to writable, unpinned agent-created Skills, and may publish a
  narrow repair after one attributable incident plus verified same-unit
  recovery. It does not rewrite protected Skills, approve capabilities, choose
  workspaces, or authorize writes, credentials, network access, shell execution,
  or external effects. Network/delete, external-origin, and delegated-effect
  candidates remain inactive until explicit user management.

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
