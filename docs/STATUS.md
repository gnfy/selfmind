# SelfMind Implementation Status

> Current-state snapshot and the repository's only priority list. Product
> direction and acceptance scenarios live in
> [`identity-continuity.md`](identity-continuity.md). Domain mechanics live in
> the generated [`README.md`](README.md) index. Code and tests remain the source
> of truth.

**Snapshot:** 2026-08-17

## Release Health

- `GOWORK=off go build ./...`: passing at the snapshot.
- `GOWORK=off go test ./...`: passing at the snapshot.
- Release corpus: 52 reviewed YAML cases. Local full replay currently proves
  all 52. Model-backed cases carry committed cassettes; deterministic cases
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
| Agent loop | Done | Native tools, structured outcomes, bounded elastic budgets, cancellation, retry classification, planning, and evidence-derived verification are implemented. |
| Execution engine | Partial | Typed scopes, environment snapshots, sandbox policy, durable watcher execution, and tool profiles exist. Linux isolation is strongest; macOS uses approval-controlled host execution. |
| Worker scheduling | Partial | Durable queue and worker-pool seams exist; personal edition intentionally defaults to one active run per person while multi-run ownership remains deferred. |
| Provider runtime | Done | Primary/auxiliary roles, explicit role overrides, protocol adapters, typed quirks, generic request extras, model metadata, auth refresh, and live contract probes exist. |
| Provider cost visibility | Done | OpenAI-compatible and Responses cache usage is normalized; adapter-level request prefix/block fingerprints diagnose cache drift without storing prompt content; `selfmind usage` and `selfmind report daily` provide local execution/cost trends, including approval-continuation attribution. Provider pricing remains external. |
| Context lifecycle | Done | Person work spine, bounded composer slices, project instructions, deterministic workspace-knowledge indexing, artifacts, recall, and compaction are integrated. |
| Memory | Partial | Canonical governance, pin/correct/forget, transient filtering, lexical/CJK retrieval, access tracking, audits, output-overlap recall telemetry, and per-run intake disposition counts exist. These signals are diagnostic rather than proof of causal use; quality, reuse, and duplicate rates still need sustained measurement. |
| Tasks | Done | Inbox, lifecycle fields, paging/search, pin/archive/rename/merge, retention, governed Task References, audited attach policies, explicit dry-run legacy-reference migration, and asynchronous post-run labeling exist. Semantic references never grant workspace or prior-run authority; ticket-shaped work keys are display hints only. |
| Background maintenance | Done | Debounced bounded batches, immutable replay jobs, restart-safe retry exhaustion, shared retry policy, provider/contract circuit identity, fallback roles, diagnostics, migration tools, and dispatch-time reasoning/output bounds exist. |
| Skills | Done | Runtime discovery, one-Skill-per-work-unit activation, deterministic task binding, active/candidate/previous versions, bounded context, explicit candidate management, catalog/bundles, tenant ownership, learning audit, and migration exist. |
| Safe self-evolution | Partial | Terminal work-unit observations, neutral parked waits, comparable cohorts, frozen curator proposals, failure guards, rollback, and same-task read-only `batch_read` candidates exist. Empty-procedure and external-watch evidence cannot nominate curation. Repeated verified read-only cohorts may publish only unpinned agent-created Skills; write/shell/network/external-effect candidates require explicit management. Real repeated workflows still need production validation. |
| Tool safety | Partial | Safety floor, smart approval, typed invocation scope, grants, hash-bound trusted observation scripts, secret redaction, schema governance, sandbox/host profiles, and failure envelopes exist. Human asks use one server-issued menu across CLI/IM: proceed once, optional run-local reuse with the exact proposed rule visible, and deny; sensitive asks are once/deny only. Unanswered asks park without rejection, later approval resumes through an exact-action one-shot capability below the current safety floor, and current live/parked backlog age is visible. Historical broader grants remain listable and revocable. External tool diversity remains an ongoing compatibility surface. |
| External watchers | Partial | Durable registration, bounded slow-command preflight, environment/auth snapshots, restart recovery, idempotent finalization, separate agent/external outcomes, person-scoped numbered `/watchers` controls, and delivery-confirmed stable-ID notifications exist. Keep validating provider-specific terminal behavior and live delivery. |
| IM delivery | Partial | Weixin and other adapters share durable outbound state, delivery diagnostics, session refresh classification, bounded catch-up, preferred-channel routing, desk-first/phone-first approval surfaces, and idempotent resolution follow-ups. Live platform behavior remains an external dependency. |
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
   switches expire old bodies, comparable cohorts produce narrow candidates,
   read-only promotion preserves verification, guards prevent repeated bad
   steps, and fallback still completes through ordinary planning. Continue
   validating shadow truth and turn savings for enabled read-only batches.
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
- Full multi-run foreground/background concurrency and remote Runner execution
  remain design seams only. Do not infer that they are shipped from queue or
  execution-envelope plumbing.
- Self-evolution can publish only repeated, verified read-only procedures to
  writable, unpinned agent-created Skills. It does not rewrite protected
  Skills, approve capabilities, choose workspaces, or authorize writes,
  credentials, network access, shell execution, or external effects. Those
  candidates remain inactive until explicit user management.

## Plan Lifecycle

- Active plan: none. `docs/plans/document-governance.md` is archived after its
  acceptance gate passed on 2026-08-12.
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
