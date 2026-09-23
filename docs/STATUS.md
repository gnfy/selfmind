# SelfMind Implementation Status

> Current-state snapshot and the repository's only priority list. Product
> direction and acceptance scenarios live in
> [`identity-continuity.md`](identity-continuity.md). Domain mechanics live in
> the generated [`README.md`](README.md) index. Code and tests remain the source
> of truth.

**Snapshot:** 2026-09-03

Daily-driver closure update (2026-09-18): artifact readback carries trusted
read-only registration facts into continuation. Verification corrections can
inherit the recorded obligation; explicit plan-step bindings attribute failures
to the correct criterion. Unresolved dependency paths retain conservative
invalidation. Completion failures expose exact blocking evidence. Approval
review rejects incomplete action input and requires attributed restriction
citations for new provider-backed denials. Response usage includes input/cache
accounting, with historical gaps kept visible separately from Main/maintenance.
Two new real-provider cases cover readback-before-resume and method correction;
four live approval probes cover changed wording/targets and opposing constraints.
Full selfcheck passed all 81 cases, with focused race coverage and an isolated
packed-npm daemon lifecycle smoke. Local installation and the managed daemon run
`v0.1.0-beta.26-local.model-recovery.7`; real multi-day and cross-model behavior
remain observation gates. Historical unfinished Runs are not rewritten.

## Release Health

- `GOWORK=off go build ./...`: passing at the snapshot.
- `GOWORK=off go test ./...`: passing at the snapshot.
- Release corpus: 81 reviewed YAML cases; model-backed ones carry committed cassettes, deterministic ones
  declare `model_required: false`. One pins the containment release, which unit tests cannot see.
- `selfmind selfcheck` is the release gate. It always checks the documentation
  contract, then build/test and provider-offline eval according to profile.
- Pull requests run the fast offline corpus and core Linux/macOS checks. Main
  CI runs the complete offline corpus, focused race tests, and package smoke in
  parallel for the exact merge SHA; superseded-run cancellation applies only to
  pull requests so every main SHA keeps its CI evidence. Documentation-only
  changes skip the platform, package, and race jobs (fail-open classifier)
  while the core build/test gate and offline corpus still run; each CI package
  smoke builds only the platform it installs, and the release workflow stages
  and verifies all four. Prerelease publication reuses that exact
  successful main result; stable releases repeat the full source gate. Release
  tags are created or verified only after packed-artifact install plus isolated
  daemon start/health, authenticated status/tasks, restart persistence, and
  stop smoke on Linux x64 and macOS arm64; native coverage for the other
  packaged architectures remains release evidence to add.
- Linux and macOS x64/arm64 are packaged targets. Native Windows remains
  unsupported; WSL is the supported Windows route.

## Product Gates

| Gate | State | Evidence still required |
| --- | --- | --- |
| Personal daily-driver | Partial | Continue real coding/operations use and close regressions from daily run reviews. |
| Phase-1 continuity | Partial | CLI-to-IM approval has process-level presence, detached-immediate/T1 escalation, parked answerability, and daemon-restart continuation recovery. Natural language now reaches one audited Main path: active input durably steers; idle input can progressively search/inspect/select person-scoped work; a validated same-domain resume is claimed in the same turn before any effect (one Main Run, no queue) while a workspace, execution-root, or checkpoint mismatch transfers to a correctly scoped exact-parent child with its durable inherited plan established before Main starts, and explicit bound-endpoint delivery overrides survive restart. Repeat the full live scenarios across real IM transports, restart, correction, and stranger isolation. |
| npm beta distribution | Partial | Clean tagged release through GitHub Actions plus install/update/daemon-restart verification on Linux and macOS. |
| SaaS / enterprise | Deferred | No implementation until maintainers approve a dedicated strategy decision and its evidence gates. |

External observation closure update (2026-09-14): new versioned receipts require
whole-value scalar matching, retain already-observed group members, and restore
Main with normal scoped tools and approvals after a condition match. Incomplete
groups cannot silently hand off, and abandoned incomplete contracts surface as
check failures. Historical receipts retain their original authority. Terminal
syntax is checked before approval; daily reports include current wait-group
backlog and missing-member counts. Real-provider/replay coverage now resumes Main
through the production worker to create and verify the deliverable. Versioned
approval intent preserves user prohibitions without parsing system instructions
as user bans. `/diag learning` exposes bounded workspace evidence and gate reasons;
learning and daily diagnostics explain memory shadow mode and scheduler state.
Approved host observations retain only the capability proven by their preflight,
bound to the frozen command and watcher deadline. Validation (2026-09-15): full
Go tests and all 72 local-full cases passed. An isolated real-provider daemon
preserved one ready and one pending prerequisite across process restart, resumed
Main exactly once, and produced a deliverable with two passing verification
checks. Installed locally as `v0.1.0-beta.26-local.closure.2`; managed restart,
schema v14 health, and learning/memory diagnostics passed. Live IM delivery and
sustained daily-driver observation remain required.

Approval and liveness correction (2026-09-15): version-2 approval snapshots
preserve attributed human requests, preceding proposals, and ordered corrections
across same-scope resumes. The judge evaluates accepted scope without inventing
new grants; model reasons are visible in approval summaries. Model decisions
are reused only for identical actions, human evidence, and environment within a
Run, instead of granting a command class. Active `/status`
uses the exact Run's plan and progress, not an earlier watcher handoff. Foreground
Unix cancellation kills the managed process group and bounds output draining,
fixing pipe descendants delaying a 90-second timeout for several minutes.
Full Go checks, all 72 local-full evals, focused race checks, and an isolated
real-provider process-restart scenario passed, including retained authorization
evidence, active progress, and a verified deliverable. Installed locally as
`v0.1.0-beta.26-local.closure.4`; managed restart and schema v14 health passed.
Real approval-frequency improvement remains a daily-driver observation gate.

Semantic approval correction (2026-09-15): new version-3 snapshots retain
complete bounded human quotations and stop deriving keyword prohibitions.
Smart mode reviews effect authorization even for scoped writes; unrelated
exclusions do not block accepted work, and applicable refusals remain refusals.
Enforced containment is the documented exception again (2026-09-20): reviewing
contained calls too made layer 2 unreachable, because every gateway run carries
model authorization — the live funnel reported zero contained releases against
33 judged decisions in a day while the judge escalated calls its own rationale
called read-only. A call the runtime already proves harmless now runs without a
judge call; dangerous operations, explicit denies, unclassified external
effects, write tools, and every uncontained execution keep their review.
Historical policy evidence is not widened. Direct claims and watcher resumes
preserve exact-parent authorization, including claims made after Run start.
A refused continuation pauses the turn before further dispatch; selection
lookup no longer depends on the progress-event tail. The new real-model smart
case checks preparation without writes, partial acceptance, exact continuation,
watcher recovery, verified delivery, zero human asks, and positive judge audit.
Unanswered approvals pause before further dispatch. Cheap-role review uses
structured JSON and bounded low reasoning; invalid output still asks the human.
Full Go/doc checks, all 73 local-full evals, focused race checks, and the real
configured smart flow passed (18 tools, zero errors and human asks), including
watcher recovery. Installed as `v0.1.0-beta.26-local.closure.5`; managed restart,
healthy heartbeat, zero active Runs, and schema v14 were verified.

Current P0/P1 implementation retains versioned approval response diagnostics
and refuses incomplete provider output without inventing authorization. Bound
verification corrections preserve criterion, target, scope, and failure history;
Run/work-unit verification shares one projection. Run-lifecycle, steering, and human-wait schemas now
reject unknown fields recursively before dispatch instead of silently dropping
them; plan-bound checks always retain the server-issued step identity, and the
latest exact-obligation attempt determines current state without erasing prior
failures. The public verify call no longer asks Main to copy that runtime id;
flat arguments are canonical while the published nested shape is normalized at
one compatibility boundary. Corrected methods may declare their actual inputs,
and later work units receive their own step association without changing the criterion or target. Unique exact plan text can recover a stale provider alias while
ambiguous or changed steps still fail closed. Invocation middleware retains
actual exit codes and evidence references through one typed result pipeline;
unknown process exit status cannot become zero, and failed outcome persistence
retains observed output and requires observation before retry. Fault-injection
coverage spans dispatch claims, partial effects, result storage, and notification
after commit. New dispatch interfaces use Run scope; storage migration remains
with the active Run + Work Journal plan. Plan progress preserves existing
acceptance requirements, changed criteria return to Main for review, and missing
final answers retain known blockers. Main owns explicit user-takeover semantics;
necessary unfinished work without handoff evidence stays unfinished. Three new
real-provider cases cover corrected checks and both handoff outcomes, including
an exact continuation of the prior waiting plan. Approval-frequency improvement
and the cause of the previously unclassified production parse failures still
need live observation; 24 protocol probes did not reproduce those failures.
Validation: full Go/doc checks, all 81 local-full evals, and focused race checks
passed. A real smart-mode flow passed with 22 tools, zero tool errors, and zero
human asks. The current local package matches the tested binary; managed restart,
healthy heartbeat, zero active Runs, and schema v16 passed.

Natural-language continuation uses the ordinary Main turn for every phrasing;
explicit commands and structured replies retain deterministic execution binding.
External-watch completion, compensation, and reconciliation share the aggregate
wait-group verdict, respect live queue claims, and pass bounded exact-parent
observation targets and results through durable runtime context. Resumed turns
also receive current Run plan IDs, acceptance criteria, verification flags, and
bounded user corrections from the exact resume lineage. Watch finalization
claims that lineage before plan inheritance. Stable step IDs preserve work-unit
attribution without repeated internal IDs; plans update at meaningful progress.
Execution evidence is paged independently of transcript volume, and answer-only
file mentions no longer create changed-file metadata or file artifacts.
Default tool guidance identifies `verify` as the executable verification evidence
entrypoint.
Completion evidence update (2026-09-17): `finish_run(done)` and finalization use
the same Run evidence verdict. New version-2 checks may declare local input
dependencies; unrelated outputs preserve them, related partial writes invalidate
them, replacements inherit omitted declarations, and corrected methods may
declare their actual inputs. Unavailable or malformed evidence blocks completion.
`/resume` shows each exact Run's own outcome and next step, with distinct labels for
user decisions and incomplete verification. Historical Runs are not rewritten.
Default gateway restart carries a server-enforced safe-boundary contract; older
daemons defer without interrupting Runs and remain available for human input.
Two real-provider recordings cover independent report output and rechecking changed
inputs after a work unit closes. Cross-model and sustained live coverage remain open.
Validation: full selfcheck passed all 81 local-full cases, including refreshed live recordings
for continuation, watcher, smart approval, and verification. The local npm CLI and
active daemon run `v0.1.0-beta.26-local.model-recovery.7` after a safe restart.
Its isolated packaged lifecycle passed on schema v16 with no active production Run. Live status reports Main and
Background reasoning from resolved model metadata; cross-model live evidence is not claimed.
Natural in-place Skill selection still needs live quality observation: a release
lookup completed with unnecessary calls and invalid Skill references. Explicit
resume separately covers restoration of an already bound Skill.

## Capability Map

| Area | State | Current boundary |
| --- | --- | --- |
| Daemon gateway | Done | CLI, IM, cron, and HTTP use one daemon-owned runtime, queue, auth manager, and control database. |
| Identity and continuity | Done | Person identity spans bound endpoints; transcripts stay local; Threads, Runs, approvals, handoffs, and memory are durable. |
| Agent loop | Done | Native tools, structured outcomes, bounded elastic budgets, cancellation, retry classification, durable versioned Run plans, strategy-aware failure recovery, evidence-derived verification, and an operator-owned prompt workspace are implemented. New Runs use recovery contract v1: server-issued plan-step ids survive reorder/update, the control projection—not the in-memory UI cache—guards successful completion, tool effects correlate with plan/version/strategy/environment and hashed result evidence, and loop checkpoints reference that durable state. The injected recovery policy permits one diagnostic correction, refuses exact repeats and exhausted mutation attempts before dispatch while allowing distinct proven read-only diagnostic probes, requires observation after an unknown effect, and releases its guard only after new evidence or state; explicit `verification_required` steps cannot finish without evidence-derived passed verification. Eligible daemon/provider interruptions enqueue one exact-parent recovery child below new foreground work; uncertain effects enter verification-only mode, whose trusted read-only surface and dispatch guard prevent replayed mutation. Specialist waits retain ownership, recovery children do not recurse, and historical Runs remain capability-inert at version zero. Interruptions that cannot continue automatically expose one person-scoped handoff across immediate CLI/IM results, notifications, `/status`, `/resume`, and HTTP with the original goal, plan state, uncertain effects, attempted strategies, unlock condition, and exact resume path; `gateway.automatic_run_recovery: false` stops both new scheduling and claim-time launch without discarding that evidence. Prompt files are startup-frozen, strictly validated, semantically hashed, revision-pinned for durable maintenance, and cannot remove locked quality/safety contracts; an invalid active workspace degrades visibly to the matching last-known-good snapshot or built-in defaults without taking CLI, IM, cron, and HTTP agent work offline. An always-on foreground delivery and evidence floor also covers tool-free direct answers. Tool guidance is derived from each role's actual capabilities; background review uses a bounded memory/session surface; delegated workers preserve scoped parent evidence without inheriting parent lifecycle or loop state; and compaction preserves verification, failed attempts, waits, identifiers, and files behind an untrusted-data fence. Legacy files migrate recoverably on edit, current revision caches self-repair, and restored historical revisions can explicitly resume paused work. |
| Execution engine | Partial | Typed scopes, environment snapshots, sandbox policy, durable watcher execution, and tool profiles exist. Authenticated local CLI runs support repeatable, invocation-local `--add-dir` roots that are frozen across queue/recovery, included in scoped tools and project context, and conflict-scheduled by overlapping physical paths without changing workspace trust. `ws` is the single workspace verb across CLI, IM, and TUI: selecting binds this session only, `ws default` owns the durable value that IM and scheduled work use, and merely ensuring a directory's workspace no longer moves that default, so concurrent terminals in different projects stop overwriting one person-level pointer. An untrusted workspace is asked about once at startup and can be declined durably. Both official platforms now enforce one sandbox policy — bubblewrap on Linux, seatbelt on macOS — selected per platform and reported through the backend rather than a `GOOS` check, so `ContainmentAssessment.Enforced` and the observation catalog it gates are reachable on macOS for the first time. Strength is not equal: seatbelt has no PID/IPC/UTS namespace and cannot mount, so a plan needing mount-backed tool state (only the AWS SSO token cache in the current catalog) is refused there and degrades to approval-gated host execution, and `$SELFMIND_RUN_TMP` is not `/tmp`. Each invocation now projects immutable snapshot proxy values into its actual network view: isolated calls omit them, shared calls retain remote or reachable loopback routes, and a stale loopback listener is omitted with a non-secret `proxy_mode` in the plan and event. Real approval-volume reduction on macOS remains a daily-driver observation gate. |
| Worker scheduling | Partial | Durable queue and worker-pool seams exist; personal edition intentionally defaults to one active run per person while multi-run ownership remains deferred. |
| Provider runtime | Done | Main is the sole foreground authority; Background may inherit Main, use a separate route, or be explicitly disabled, and supplies six stable maintenance roles with optional overrides. Guided setup and the later Model Manager use the same Background selector and state transitions for those three states. `selfmind model` and bare TUI `/model` open one capability-negotiated Model Manager for built-in overrides, custom connections, Main/Background routes, credentials, and reasoning; persisted choices remain separate from effective provider/model defaults, their source, and supported values, so `auto→high` is visible without forcing a wire parameter. Validated switches keep a bounded, presentation-only MRU of both previous and candidate models plus explicit reasoning values; `d` removes local history without changing routes or hiding live-catalogue models, and validation/restart waits animate with phase elapsed time. YAML uses `providers.<builtin-id>` plus map-shaped `providers.custom.<id>` with only OpenAI-, Anthropic-, or Responses-compatible protocols; built-in defaults stay in code, custom IDs route directly, secrets stay in the auth store, strict validation rejects ambiguous headers and fields, and explicit `config upgrade` backs up and migrates legacy `provider_profiles`. Provider, route, and staged credential changes share one generation-checked daemon transaction and rollback image. Every mutating entrypoint constructs that service with its credential store, and detached restart preflights the exact staged credential before stopping the healthy daemon or committing config. Selection validation covers every route the draft changes and apply reuses passing probes of the identical request from the last ten minutes, transient probe failures retry once, and a restart re-checks only the approval route; distinct physical endpoint validation lanes run concurrently, contracts sharing one endpoint are serialized, duplicate provider wire fingerprints share one result, and foreground validation exercises a complete native-tool loop with exact tool selection where its protocol and reasoning contract allow it or bounded automatic selection otherwise. Provider-required opaque tool-call replay metadata survives that loop without gaining authority. Model Manager exits only after the exact transaction is applied and the replacement daemon is healthy; every entrypoint clears its applying state or reopens actionable recovery, so failed or completed switches cannot leave input frozen. Foreground and per-role background readiness let verified foreground and unrelated maintenance continue when one background override is degraded; explicit `semantic_recall` never silently falls back to Auxiliary, degrades to lexical recall, retries transient failures with bounded backoff, persists recovery, and debounces transient notices while naming the resolved provider/model on sustained failure. Changes preserve compatible tuning, wait for an uncancellable safe boundary without interrupting an active run, queue new work across restart, require real post-start `/health`, automatically roll back only model-attributable failures, and expose retry/restore for infrastructure failures. The shared HTTP transport follows live macOS manual system-proxy changes without fail-open, while Linux uses standard proxy environment or TUN routing; route-aware retry errors and `/diag` expose concrete recovery. Protocol adapters, typed quirks, generic request extras, live/cache/stale discovery, manual model IDs, metadata, and auth refresh remain implemented. |
| Provider cost visibility | Done | OpenAI-compatible and Responses cache usage is normalized; role/VCR wrappers preserve adapter request prefix/block fingerprints and report explicit unsupported states without storing prompt content; `/diag context` distinguishes total provider requests from prompt-only assembly, while `selfmind usage` and `selfmind report daily` provide paged local execution/token trends, schema share, logical-work-chain/resume projections, dispatch phase/effect certainty, and approval attribution without the old non-causal run-level post-failure count. Provider pricing remains external. |
| Context lifecycle | Partial | Person work spine, bounded composer slices, project instructions, deterministic workspace-knowledge indexing, artifacts, recall, and compaction are integrated. Tool-catalogue deferral is live for a reviewed category cohort — Skill authoring/catalog, media, rare execution shapes, and web search leave the direct surface and return through `tool_search`, measured at about a quarter of the provider-facing schema bytes — while asking the person, tool discovery, artifact read-back, retrieval, continuity, and steering are never deferred. Tool results now also carry a cumulative per-turn cap, so several artifact-backed results inside the age window age oldest-first instead of together dominating the request; unspooled bytes are never dropped. Provider preflight now includes native tool arguments and tool schemas; compaction and trimming retain Run instructions, summaries inherit Run cancellation/ownership, and task slices prioritize verification while deduplicating handoff text. Remaining: the catalogue share is about a quarter down, not below the 20% target, and a usage-driven cohort still needs seven days of real evidence. Default context still replays only the most recent 16 work-spine entries; older history stays searchable but is not auto-replayed. |
| Memory | Partial | Person memory is preference-only: cross-endpoint `/remember`/`/forget` are the deterministic primary intake, short natural-language preferences remain eligible for asynchronous analysis without language-specific keyword routing, the post-run analyzer (v4) judges explicitly stated preferences only, and the deterministic apply layer skips environment/project targets and audits (never applies) legacy flat fact arrays — replay-proof for frozen legacy proposals. The per-turn fact extractors and their `auto_extract_*` config keys are removed. Historical environment rows stay readable and archive reversibly via `selfmind maintenance memory-archive-environment`. Canonical governance, pin/correct/forget, transient filtering, FTS-safe lexical/CJK retrieval, JSON-fenced and narrowly bounded multilingual query expansion, access tracking, audits, output-overlap recall telemetry, and per-run intake disposition counts exist. Governance due state is durable per person, catches up overdue work after startup, retries foreground deferral promptly, distinguishes bounded partial progress from a complete scan, and exposes remaining backlog plus report age/scheduler reasons. These signals are diagnostic rather than proof of causal use; preference usefulness, reuse, and duplicate rates still need sustained measurement. |
| Work history | Partial | Schema v16 has one durable Thread aggregate and one Run execution authority: ordinary roots are retained as `interaction + unlisted`, work evidence (plans, side-effect tool rows, approvals/clarifications/watchers, resume edges, next steps) promotes in place while lifecycle and read-only tools never do, and Thread has no lifecycle status or `current_task` pointer. Attention is derived per exact Run from live execution, pending approval/clarification, watchers, and unclaimed resumable outcomes: only the latest Run of a Thread is resumable, `interrupted` counts only with work evidence, same-channel items rank first, and `/status`, the attach digest, task cards, and the compatibility `Task.status` vocabulary (`active|needs_attention|monitoring|resumable`, `done`, `archived`) read that one derivation. A bare `/resume` is the attention listing and owns the ordinal snapshot; the tasks, task, and diag tasks commands are removed, `/search` is a gateway command on every endpoint, and an idle `/stop` dismisses only the exact Run and refuses while a pending approval, clarification, or live watcher exists; explicit resume reverses dismissal or archive. Numbered commands bind endpoint-local snapshots while stable Run ids remain restart-safe; a Thread id resumes only one unambiguous unresolved Run. Search covers complete retained titles, Run inputs, handoffs, and paths, including unlisted and archived history; FTS sessions are keyed by Run (`run:<id>`, replacing `task:<id>`) and recall groups a continued line of work by walking the resume edge at read time, and event ownership is derived from the Run rather than a Thread join. `startRun` takes a RunOwner, so a Run with no Thread executes, parks, stays Attention, and resumes; ordinary root messages still create one, which is the remaining Batch 3 step. Otherwise-new user turns receive at most three same-channel-first, transcript-free Attention hints in the normal Main context, so short confirmations remain discoverable even when semantic recall skips them; Main still commits any relationship through `work_select`. Structured reply edges and `runs.resumes_run_id` (v12 renamed it from `parent_run_id`) remain person/scope validated and unique-index protected; Task holds no authority: approval grants are person-plus-class only (the retired task scope authorizes nothing and smart triage now grants run-local reuse instead of a durable row), a parked approval is claimed on person plus fingerprint plus the exact resume lineage rather than a shared thread, re-enqueued work takes its workspace from the Run, and Attention reads no Thread column — pinning is removed and dismissal on the exact Run is the only hide, with v13 converting every hidden/archived Thread into that bulk dismissal; a validated natural-language RESUME in the same execution domain is claimed atomically at `work_select` time with the parent's plan restored and its resume context returned to Main in the same turn, while a domain or checkpoint mismatch creates a correctly scoped transfer child with inherited durable plan before Main starts. Active natural language is steered, daemon text cannot steer, `semantic_recall` is optional/fail-open, and `fast_classifier` has no continuity authority. `reset-work-history` provides dry-run, live-work refusal, verified backup, and tenant-scoped cleanup that removes in-flight Skill learning evidence and Thread-keyed memory sessions while preserving identity, settings, memory preferences, provider state, grants, and published Skill packages; the v10-to-v11 upgrade keeps legacy kinds and maps hidden labels to unlisted under orphan and resume-edge invariants, and a released-v11 schema fixture proves the v12/v13 steps reach every install: shape adoption may only claim the version its detector recognizes, never the current one. v14 gives the queue and the steering mailbox durable attachment references, so work that parks — or guidance added mid-run — keeps the files it was accepted with instead of persisting only its text; rows written before it decode as no attachments, which is what they had. v16 drops `threads.pinned`: a pin kept a Thread listed and exempt from automatic archival, which let a display flag decide what counts as work — the same mistake v13 corrected when it stopped ranking Attention by Thread columns. Its writer had no production caller, so what remained were three guards that could only pass and a `/diag` counter that could only read zero; `/status`, `/watchers`, and `/diag` now say Work rather than Task. v15 drops the task-reference tables: a reference existed to address a Task by a human-facing name, nothing has written either table since Task stopped being a domain object, and what remained was schema plus two always-true `NOT EXISTS` guards that read as conditions on two DELETE statements which had none. Go/eval gates are implemented; sustained real CLI/IM and restart evidence remains. |
| Background maintenance | Done | Debounced bounded batches, immutable replay jobs, restart-safe retry exhaustion, shared retry policy, stable semantic roles with a shared auxiliary floor, provider/contract circuit identity, diagnostics, migration tools, and dispatch-time output bounds with headroom for each route's configured reasoning exist. Retryable connection failures also retain a credential-free network-route fingerprint, so direct/proxy and local-listener changes release delayed or exhausted learning jobs without replaying unrelated provider, prompt, or policy blocks. Memory governance keeps its due clock durable and caps model-free schedule rescans at five minutes, so host sleep cannot strand overdue work behind a long monotonic timer. |
| Skills | Partial | Runtime discovery uses a budgeted metadata catalog and server-issued candidate refs; provider catalogs contain no per-Skill tools. Exact tool-name terms now deterministically win while added natural-language words rank candidates instead of excluding an exact match, and paged Skill activation exposes `skill_view` automatically. Model, slash, and binding paths converge on one immutable package activation with context-proportional main delivery, explicit section/resource paging, compaction protection, active/candidate/previous/quarantined versions, and Doctor receipt checks. Automatically learned Skills default to a control-managed logical-workspace root outside the repository and are not discoverable from another workspace. Externally authored packages are usable: read-only roots are enumerated by package manifest when one is declared and otherwise scanned recursively within a fixed depth and exclusion set, `~/.agents/skills` is a cross-vendor root below the writable user root, names qualify as `source:name` with the discovery path as last-resort disambiguator, a typed ambiguous name is refused rather than resolved by precedence, and an author's model-invocation opt-out keeps a Skill user-invocable only. Curator authorization uses the exact production delivery builder, paged legacy repairs are non-growing, bundles share one executing-agent budget, and `/skills stats` derives from durable activations/work-unit outcomes. The cassette-backed local-full release gate is green; sustained production and installed-binary/daemon evidence remain open. |
| Safe self-evolution | Partial | Terminal work-unit observations, neutral parked waits, comparable cohorts, frozen curator package proposals, environment-bound failure guards, evidence snapshots, quarantine, and compatible-previous rollback checks exist. Ordinary workflow success is observation only and cannot increment shadow matches, revive degraded candidates, or enable `batch_read`; runtime advice requires a separately verified comparison contract that the current profiler does not create. Three independent, comparable, verified work units may publish a workspace-scoped Skill when their procedures use eligible built-in tools, without granting execution authority. Repairs combine declared and daemon-observed categories: deterministic interface drift may publish after one verified recovery, workspace-scoped stable preconditions after one, semantic drift after three independent recoveries, and not-applicable/transient evidence cannot auto-publish. Schema v5 persists dependency/environment fingerprints and last verification time for bounded review nominations. User-global widening and sustained real-workflow validation remain open. |
| Tool safety | Partial | Safety floor, smart approval, typed invocation scope, grants, hash-bound trusted observation scripts, secret redaction, schema governance, sandbox/host profiles, and model-safe typed failure envelopes exist. Smart triage always uses the resolved model's lowest supported reasoning tier, and model changes validate the production-shaped five-second structured approval contract before activation, recording a proven disabled-reasoning encoding when the model keeps reasoning. `request_permissions` can bundle up to eight statically known built-in effects into one human phase approval; each exact canonical argument set is bound to the current run, workspace, environment generation/fingerprint, principal, credential source, and sandbox, while the live invocation still reruns the hard floor, explicit deny, and network/credential checks. Changed or opaque effects ask again, and arbitrary code, unknown tools, curl-like clients, and forbidden operations cannot enter a bundle. Run-lifecycle, steering, and human-wait controls use closed recursive schemas, reject unknown argument paths before dispatch, and retain open-schema extension fields instead of silently dropping them. Recovery-aware failures may additionally state preparation phase, retryability, effect state, state change, and bounded alternative strategies without exposing raw diagnostics. External MCP tools use the official Go SDK over stdio or Streamable HTTP, with paginated discovery, live catalogue updates, collision-safe names, health diagnostics, internal-argument filtering, and per-schema quarantine. Direct/deferred/hidden native-tool filtering and work-unit-local monotonic activation through `tool_search` are implemented; automatic external deferral has no guessed name-hash cohort and remains code-gated pending a reviewed seven-day usage/fingerprint baseline. Unclassified MCP calls fail closed to once-only human approval in every mode. Human asks use one server-issued menu across CLI/IM: proceed once, optional run-local reuse with the exact proposed rule visible, and deny; sensitive asks are once/deny only. Unanswered asks park without rejection, later approval resumes through an exact-action one-shot capability below the current safety floor, and current live/parked backlog age is visible. Historical broader grants remain listable and revocable. External tool diversity remains an ongoing compatibility surface. |
| External watchers | Partial | Durable registration accepts only proven read-only observations, statically rejects unsupported command/spec shapes before approval, performs its bounded real preflight after authorization, freezes a typed receipt with command hash/environment/adapter/target/deadline/capabilities, and automatically hands a successful registration off as `waiting_external` without another model turn. Spec v3 consumes registry-owned `pending`/`succeeded`/`failed` observations; historical regex specs retain frozen compatibility. Run-local `all`/`any` groups settle through one transactional aggregate verdict and at most one finalization Run. Unsupported registration reports `not_dispatched` plus generic alternative strategies rather than forcing repeated watcher attempts. Polling survives restart without holding the person's active run; terminal writeback is a separate idempotent background finalization with distinct agent/external outcomes, concise TUI state, person-scoped numbered `/watchers` controls, and delivery-confirmed stable-ID notifications. Keep validating provider-specific terminal behavior and live delivery. |
| IM delivery | Partial | Weixin and other adapters share durable outbound state, delivery diagnostics, session refresh classification, bounded catch-up, preferred-channel routing, desk-first/phone-first approval surfaces, and idempotent resolution follow-ups. Old `pending_session` final results can be replaced by one exact-platform-account-and-channel recap; only confirmed recap delivery dismisses the exact summarized rows, while explicit no-send dismissal remains available. Live platform behavior remains an external dependency. |
| TUI | Done | Daemon event stream, call-id-routed and semantically colored tool cells with terminal cleanup, CommonMark/GFM assistant rendering with adaptive narrow-screen tables, and a bounded single-owner process surface exist. Tool calls stay flat and chronological while a fixed semantic gutter makes the hierarchy explicit: commentary is outermost, actions step inward once, and typed evidence steps inward again. A semantic action and target lead, the implementation name and duration remain subordinate metadata, and completed read batches report only their aggregate instead of repeating child targets. Bounded typed evidence summarizes work selection, Skill activation/pages, file reads, command output, and failures without exposing raw protocol JSON or opaque `completed` rows. A resolved semantic theme is injected across transcript, Markdown, Approval, Composer, notices, pagers, session browsing, and Model Manager; `tui.theme` supports `auto`, `dark`, `light`, and `mono`, respects terminal color capability and `NO_COLOR`, keeps mainline prose on the terminal's default foreground, and never paints an Approval or Composer background. The startup identity band and historical/active input use open full-width boundaries without side rails; Main, Background, and explicit role overrides include readable responsibility descriptions, while values wrap losslessly. The Composer grows to at most six rows/one third of the terminal, exposes history and visible-line position, uses payload-free `[Paste #N · size]` / `[Image #N · name]` tokens, and shows width-adaptive `Ctrl+J` newline plus `Ctrl+V` image guidance with a live attachment count. Its painted caret also anchors the terminal's real cursor after every frame, keeping native IME preedit text and candidate windows at the Composer during streaming redraws. An image token is the only attachment state committed to the draft: deleting it detaches the outgoing image without leaving a transcript notice. Action narration uses normal-contrast multilingual text; correlated tools follow it as ordinary timeline cells, unknown phases stay neutral until a boundary, closed Markdown blocks render stably while incomplete tails remain literal, and the measured ten-row cap preserves the Composer and status line. The Dot waiting animation runs one 10 FPS tick chain from structured `thinking` through `model_wait`, reserves one activity row beside a live Plan, refreshes elapsed text once per second, and has zero idle ticks. Codex-style queued approval decisions use a keyboard-owning active-region panel with losslessly wrapped action targets, explicit cancel, and cross-endpoint resolution; typed transient notices, bottom plan panel, pagers, and a single-owner Composer remain intact. Digest and live Plan snapshots share one `run_id`/version/cursor reducer, so stale or foreign events cannot regress visible progress. Context windows retain provenance: built-in/profile estimates render `ctx est`, `/status` names the source, and unavailable metadata remains `ctx ?`. Composer history uses strict empty/boundary navigation, suppresses completion while recalling slash entries, restores rich paste/image drafts within the process, and persists only safe person-local text. Subsequent terminal resizes clean and repaint the bounded inline region so terminal reflow cannot duplicate Composer/status rows; committed history remains native scrollback. Resume transcript, build-fingerprint detection, and the sole interactive Model Manager also exist. Syntax highlighting, user-defined palettes, named theme packs, runtime `/theme`, and committed-history resize reflow remain deferred. |
| Distribution and updates | Partial | npm platform packages, launcher, continuous keyboard-driven model/runtime setup with optional role overrides and explicit Background-to-Main inheritance, resumable first-use progress, unified `selfmind update` notices, equal-version package refresh, feedback, and per-user macOS launchd/Linux systemd service management exist. Managed service definitions preserve only exact credential-free standard proxy variables from the installing shell, including `ALL_PROXY` fallback for Go transports, without adding provider configuration; `selfmind env refresh --restart` safely rewrites and verifies that environment instead of requiring a separate reinstall command. Managed readiness uses a non-secret service generation plus running job, version, configuration identity, and effective-route fingerprint; replacement drains active work without force, waits for runtime ownership release, and performs at most one proven-safe bootstrap retry. Compatible active Gateways remain usable as Runtime Degraded rather than being force-killed or falsely reported healthy. `control.db` has an explicit compatibility version, verified pre-migration backups, historical-state invariants, a restore command, and strict post-restart build/schema health. Public beta still requires released-version upgrade fixtures plus Linux/macOS rollback evidence. |

`Done` means the capability is implemented and covered at its current personal
edition boundary. `Partial` means usable with a known evidence gap or platform
limitation. It does not mean the area should be redesigned from scratch.

## Highest-Value Next Work

1. **Validate Thread history through installed daily-driver use.** Exercise
   unlisted direct answers, deterministic promotion, exact-Run Attention,
   dismissal/reopen, cross-endpoint resume, daemon restart, and the backed-up
   local history reset. Fix correctness defects before calling schema v11
   released.
2. **Accumulate release evidence on the personal edition.** Use daily-driver
   runs to measure successful completion, interruption/recovery, approval
   latency, watcher finalization, IM delivery, cache usage, and maintenance
   health. Include the cassette-backed Skill lifecycle suite, full selfcheck,
   and installed-binary/daemon verification before treating the presentation
   contract as released. Use `selfmind report daily` as the local baseline and
   fix observed correctness defects before speculative platform work.
3. **Measure memory usefulness, not record count.** Track query-relevant
   canonical recall, injection, reinforcement, supersession, duplicates, and
   user correction. Improve selection/write policy only from those traces.
4. **Validate Skill lifecycle and safe evolution on repeated personal workflows.**
   Confirm that task bindings reduce directory/context cost, work-unit Skill
   switches expire old bodies, comparable cohorts publish narrow procedures,
   ordinary write/Shell publication never bypasses execution policy, verified
   repairs change only attributable sections, guards prevent repeated bad steps,
   quarantine prevents repaired regressions from reactivation, and fallback
   still completes through ordinary planning. Collect evidence before building
   real Fast Path comparison/canary machinery; ordinary observations are not
   shadow evidence.
5. **Prepare the next npm beta only after the full gate passes.** The release
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

- Active plan: `docs/plans/run-centric-work-history.zh-CN.md`, approved by
  the project owner for review on 2026-09-18. It moves work-history authority
  from Task/Thread to Runs and the person-level Work Journal, keeps exact
  execution recovery, and includes context economics in the same delivery.
- Paused plans: `docs/plans/main-turn-work-continuity.md`, approved for review
  on 2026-09-09; `docs/plans/daily-driver-closure.md`, approved for review on
  2026-09-11; and `docs/plans/external-skill-packages.md`, approved for review
  on 2026-09-25. The 2026-09-10 continuity verdict carries its remaining real
  CLI/IM evidence gates into the active run-centric plan. The 2026-09-13
  daily-driver verdict retains all outstanding evidence and acceptance gates
  while paused; resumption is reassessed when the active slot frees.
- Historical plans, including the implemented schema-v11 Thread intermediate
  and the superseded Task Capsule proposal, remain discoverable through
  `docs/README.md` as archived records or decisions. They do not contribute
  priorities.
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
