# Status History

Dated delivery and validation records moved out of
[`STATUS.md`](STATUS.md) on 2026-09-24 so that file stays a current-state
snapshot. The text is unchanged. Versions, case counts, and schema numbers
describe the moment each record was written; current state, open gates, and
priorities live only in `STATUS.md`.

## Pre-Run recovery write failures (2026-10-11)

Fault injection reproduced two gaps after the previous batch: returning a
confirmed non-dispatch claim could fail and strand its started/unbound row,
and a queued worker could fail before Run insertion yet silently settle done
without a saved answer. Exact-token cleanup now retries through the production
watch-worker's existing drain path. Pre-Run failures retain the original input
and notify their originating session instead of assuming a finalizer ran.

Independent review added scheduler-wiring, source-fairness and failed-notice
regressions. A bounded pass holds the failed source and admits others; the store
also holds a source behind an unbound started claim. The caller receives a
startup refusal after the slot is released. Failed notice enqueue retries the
existing keyed outbox without an execution slot. User channel messages are
recorded after Run insertion so startup retries cannot duplicate them.

A competing exact continuation and a failed claim return are also distinguished
from a human wait. The unstarted correction remains retryable instead of
settling done; after recovery it follows the same resolved lineage once. A
changed token exits as stale ownership without settling another worker's row.
An ambiguous continuation transfers input through one control-store transaction
for its pending choice, scoped question event, exact-token queue handoff and
prepared outbox parts. A failed write rolls all of these back and retains the
queued request. Restart cannot duplicate the choice or lose its notification
intent. CLI replay reaches only the source session; a preferred IM endpoint uses
explicit `/choose` without widening bare-reply authority. Sending rechecks the
exact choice and supersedes answered or expired questions.

Production-tick fault tests cover capacity one and two, concurrent creation and
return-write faults, same-source order, expired live claims, bound/cancelled and
reassigned rows, full-capacity notification recovery and CLI audience filtering.
Focused race tests passed three repetitions, including choice write, event,
handoff and outbox failures, exact ownership changes, pending/answered/expired
cross-endpoint notices and CLI replay. Independent Spec and Standards reviews
passed after their fault probes were reproduced and repaired. Fast and full
selfcheck passed all Go tests, 52 documents and all 116 offline release cases,
with no CI deferrals; the corpus is unchanged.

The local working-tree package
`v0.1.0-beta.28-local.concurrency-closure.13`, built at
`2026-10-11T03:30:09Z`, passed npm install and isolated daemon lifecycle smoke.
It was installed globally and the launchd daemon restarted safely; its binary
SHA-256 matches the staged build. The production queue counts and both historical
uncertain effect claims were preserved, with no active or queued work at restart.
Schema remains v26. Real IM acceptance, provider-independent performance
improvement and sustained throughput remain unproven.

## Queue refusals and durable recovery notices (2026-10-11)

Two independently reproduced boundaries are fixed without a new queue, schema
or permission policy. A synchronous pre-worker refusal returns the exact
unbound claim to its original input for a later periodic drain. Started
workers retain their finalizer; no immediate retry loop or second input is
created. The regression recovers the same row and executes it once.

Obsolete-continuation events project to the existing background notice on
both live delivery and durable replay, even before settlement succeeds. The
CLI deduplicates their event IDs without changing its active Run. A late
client may skip an already-saved event, so presence alone no longer suppresses
the one keyed outbox fallback. Injected event, outbox and settlement failures,
late attachment, repeated drains, HTTP replay and stranger isolation exercise
these boundaries. Tests use an isolated fake IM sender, not real messages.

The first full-package pass exposed an existing test false positive: a
four-digit sensitive marker matched a random hexadecimal Skill reference.
The fixture and leak assertion now use a complete marker; the other text and
input-ID leak assertions are retained. Runtime projection is unchanged.

Final full selfcheck passed all Go tests, 52 documents and 116 offline cases
with no CI deferrals. Focused race regressions passed three runs; independent
Spec and Standards reviews found no remaining actionable issue in this batch.
These are mechanical production-stream and queue tests; the release corpus
remains unchanged. The macOS arm64 packed artifact passed isolated install,
start, restart/persistence and stop smoke. Local npm and launchd now run
`v0.1.0-beta.28-local.concurrency-closure.12`, matching that binary's SHA256.
Restart occurred with zero active or queued work; both historical uncertain
claims remain unchanged. Installed `.11` packages are backed up for rollback.
Real IM acceptance and the 24-hour soak remain open.

## Prompt prefixes and continuation recovery (2026-10-10)

Tool contracts and freshly read project instructions now precede selected Run
state in the same system message. A regression first failed on the old order,
then retained an 8,635-byte prefix across changed Run IDs and user corrections;
changed project instructions invalidate that prefix immediately. Roles,
permissions, required-context budgets and paired tool history are unchanged.
This is a prefix-layout improvement, not evidence of a provider cache hit rate
or a full resumed-ledger cache guarantee.

Foreign-effect notices distinguish unknown target scope from a named conflict
and point to existing settlement controls. Unknown claims remain unresolved.
A missing current verification now offers explicit prior-check adoption when
the existing provenance/input validator finds it legal. Main supplies the
judgment and reason; explanation alone never adopts evidence. Changed inputs,
criteria or scope and unknown mutations cannot offer that option.

Full selfcheck passed all Go tests, 52 documents and 116 offline cases. Focused
race regressions passed three runs. The macOS arm64 npm package passed isolated
install and daemon start/restart/persistence/stop smoke. No new release case
was added; existing cases gained scope and source-evidence assertions.

Gemini's first live continuation case passed in 50.3s with nine tool calls and
zero errors. Qwen's first attempt took 113.8s and repeated verification after
omitting explicit adoption. After recovery feedback improved, a 133.4s attempt
kept one check and completed, but failed the zero-error gate through an invalid
cancellation and a post-finish tool attempt. These are retained negative
behavioral evidence, not a clean cross-model or speed acceptance claim.
The final Gemini recording passed in 65.4s with eight tool calls and zero
errors; its updated cassette replays offline in 340ms and passes corpus
validation. Local npm and launchd now run
`v0.1.0-beta.28-local.concurrency-closure.11`; the installed binary matches the
smoke-tested build. Restart occurred with no active or queued Run. The two
historical unknown-target claims remain unchanged, and the prior package
archives were retained for rollback.

## Exact queued continuations and mandatory approvals (2026-10-10)

An obsolete exact daemon wakeup is now a terminal scheduling refusal, rather
than a capacity retry or new work. The unbound row retires after an idempotent
work event and configured outbox receipt; notice failures retain the input.
Claim-token and Run-binding checks protect later attempts. The parent-claim
race at atomic child creation uses the same path. Human decisions and
unfinished Runs remain intact, and person-authored replies still follow their
owned lineage. Historical queue classes use their existing exact trigger keys.

Known foreign uncertain effects skip automatic triage and reusable grants,
joining the mandatory one-time human decision. Current restrictions and the
hard floor still apply; dispatch checks again for a new conflict. No claim is
settled by age or approval, no model/scenario branch or schema was introduced.
The existing bound-observation replay adds zero-human-ask assertions.

Full selfcheck passed all Go tests, 52 documents and 116 offline cases, with
no CI deferrals. Focused race regressions passed three runs; additional human
rejection cases pass in read-only, smart and full-auto modes. The macOS arm64
npm package passed isolated install, start, authenticated status, persistence
across restart and stop smoke. This does not replace live IM, sustained
throughput, cross-platform runtime or daily-driver effectiveness gates.

## Frozen verification windows and corpus consolidation (2026-10-10)

Continuation references, completion and freshness now share the existing Work
Unit evidence window. A later check in another unit cannot replace an earlier
proof or hide changes to its inputs. Every effective check is revalidated;
older journal references outside the window remain unusable. Open-window saved
observations and legacy bindings still work. Standalone verification replacement
keeps its original criterion, target and cwd even when a Plan exists, unless Main
explicitly selects an owned step. No schema or model-specific policy was added.

The duplicate `/diag execution` cases now share their assertions and Linux/macOS
CI ownership. The obsolete pre-label attachment case and cassette are retired;
the release corpus has 116 cases. The standalone-target case asserts the actual
binding, including absence of any Plan-bound observation for that other target,
so a later requested failure cannot mask an incorrect earlier pass. The same
real-provider trace passes the current implementation and fails the former
binding implementation. Gemini and Qwen observations are recorded independently;
this narrow evidence does not establish general model quality. Mechanical tests
cover changed inputs, unrelated writes, multiple effective checks, later failures,
legacy/malformed references, unchanged rejected Plans and standalone replacements.

## Verification continuity and runtime result facts (2026-10-10)

`verify` accepts an exact pending Plan step without a preparatory state change.
Named checks without a step remain standalone; matching wording or the active
position cannot attach them to an unrelated obligation. Replacements keep their
original binding unless Main explicitly chooses a current step. Plans retain
outcome milestones rather than reads, approvals and retry attempts.

Plan inheritance atomically records original check references in the existing
event journal. Repeated waits no longer consume the bounded legacy lineage
search. A saved successful observation can be explicitly adopted after an
interruption before Plan bookkeeping, subject to frozen evidence, unchanged
criteria, execution scope and declared inputs. Unknown changes remain blocking.
Prompt evidence is bounded and prioritizes observations; `work_inspect` pages
the remaining Plan and its source evidence. Historical missing proof is not
retroactively rebound or treated as successful.

Typed context distinguishes external-effect blockers from Plan milestones.
Final results expose unresolved claims associated with their exact lineage or
typed observation/approval references, including a factual availability warning
if selection fails. Another action's approval or a foreign observation does not
settle the original effect. Schema, capacity, model choice and settlement
authority are unchanged.

Validation: full selfcheck passed all 118 offline cases, all Go tests, build and
documentation checks. Focused control, tool, kernel and HTTP race tests passed.
Three new cases and twelve refreshed cases passed live Gemini recordings and
offline replay. Qwen passed the pending-step case on retry; its standalone and
effect cases still exposed protocol/close-out failures, so general live quality
across models is not claimed. Mechanical regressions cover 24 continuations,
saved evidence before close-out, changed criteria/scope/inputs, unrelated writes,
16-step paging, journal-write rollback and foreign effects that remain unresolved.
Actual approval/call reductions need daily-driver observation; real IM and the
24-hour mixed soak remain open.

## Daily-driver closure (2026-09-18)

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

## Product-gate updates (2026-09-14 to 2026-09-20)

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

## Resource-wait and evidence correction (2026-10-03)

An unresolved effect in the same Run no longer creates an automatic wait with
no observation producer. Typed admission feedback requires observation before
retry; other live owners and exactly bound watchers retain normal waiting.
The existing daemon sweep atomically changes stranded waits to blocked results,
preserving uncertain claims and plans. Committed correction events recover a
notification after a crash between commit and delivery. Run-parent projections
include manual resumes and ancestors outside the report window; unavailable
projections are explicit rather than substituted from event counts.

Provider capacity contention has a cancellable 250ms grace before durable
deferral; 429 cooldowns still defer immediately. Permit ownership and role counts
change atomically. Ordinary commands are distinct from criterion-bound checks,
and post-finish tool refusals describe phase availability rather than budget.
There is no schema change, automatic claim release, default capacity increase,
or Main model/reasoning change.

Validation: fast and full selfcheck passed all 95 cases, focused Go race tests
passed, and result-write fault injection preserved the parked state and claim.
Commit replay kept one correction and a recoverable notification. A read-only
production projection matched 29 Runs, 18 chains and 11 committed resume edges.
Two new ordinary-evidence cases recorded successfully on Gemini. A Qwen attempt
read the right output but quoted another value; its log is failed grounding
evidence, not a passing recording. One Qwen two-Run soak overlapped terminal work
by 15 seconds, but later Qwen and Gemini soaks completed both lineages without
the required overlap after capacity deferral. Sustained throughput and actual IM
delivery remain open gates; the latter is deferred at the user's request.

The live soak previously shared the operator configuration directory despite
isolating its data directory. A temporary provider override therefore changed
adjacent model readiness state. The configuration itself was unchanged; a normal
idle restart revalidated the configured Qwen route. The script now copies the
configuration into its temporary runtime with mode 0600, and a file-boundary
test covers adjacent-state isolation. A subsequent isolated startup left the
operator's verified state timestamp and configuration unchanged.
