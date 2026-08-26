# Skills Architecture

This document describes the current SelfMind Skills contract. Accepted design
changes that are not yet fully implemented live in
[`skill-learning-and-repair.md`](skill-learning-and-repair.md). A target in that
decision must not be presented as current runtime capability until
`docs/STATUS.md` and the implementation agree.

## Purpose and Maturity

SelfMind treats a Skill as a durable instruction asset, not an executable
plugin. A package may contain a `SKILL.md`, references, templates, scripts, and
assets, but every resulting action still traverses ordinary tools,
`ExecutionScope`, sandbox policy, budgets, and approvals.

| Area | Maturity |
| --- | --- |
| Discovery, package identity, activation, paging, and version pinning | Current contract |
| Task affinity, work-unit attribution, candidate versions, promotion, rollback, and failure guards | Current contract |
| Automatic creation and narrow repair from work-unit evidence | Implemented; production-evidence gate remains open |
| Workspace-first placement, tiered repair evidence, quarantine, and staleness metadata | Implemented; sustained runtime evidence remains open |
| Recipe compilation, real shadow comparison, canary rollout, and Fast Path | Not a current Skill capability |

The public model interface stays small. Installed Skills do not create one
provider tool each.

## Canonical Terms

The word "candidate" previously referred to two different concepts. Use the
following terms in new documentation and code:

| Term | Meaning |
| --- | --- |
| Logical Skill | Stable identity across content versions |
| Skill package | One immutable main body plus its linked resource manifest and bytes |
| Active version | The version available for future activation |
| Version candidate | An unpublished CREATE or PATCH proposal |
| Selection reference | A server-issued, work-unit-scoped reference to an already discoverable package; the current wire field remains `candidate_ref` for compatibility |
| Activation | One work unit loading one fixed Skill package version |
| Workflow observation | Immutable projection of one terminal work unit and its evidence |
| Skill incident | A failure attributable to an active Skill version and observed tool call |
| Recovery proof | Evidence that ordinary planning recovered the same work unit and passed verification |
| Failure guard | Version- and failure-specific instruction to avoid repeating a known bad path |

A version candidate is not callable. A selection reference does not grant
publication or execution authority.

## Deep Modules and Interfaces

Skills are implemented behind four modules with narrow interfaces:

1. **Catalog module:** discovers packages, resolves precedence, ranks bounded
   metadata, issues selection references, and reads package sections/resources.
2. **Runtime module:** activates one resolved package for one work unit, freezes
   its delivery receipt, and handles fallback or terminal completion.
3. **Lifecycle module:** creates immutable version candidates and performs
   explicit or policy-authorized promote, reject, rollback, bind, and unbind
   operations. Its mutation interface is hidden from models.
4. **Curator adapter:** consumes one frozen evidence digest after a run and
   returns `CREATE`, `PATCH`, or `SKIP`. It is the sole automatic proposal
   authority and never writes active content directly.

Callers use the same catalog and runtime modules across CLI, IM, cron, HTTP, and
background work. Channel adapters do not resolve roots, versions, bindings, or
mutation authority themselves.

## Discovery, Roots, and Trust

Discovery flows through the shared Skill service. Visible roots, in precedence
order, are:

1. Workspace `.selfmind/skills` at the typed execution-scope root.
2. Workspace `.agents/skills` for Agent Skills-compatible repository guidance.
3. Workspace `skills/`.
4. The control-managed learned-Skill root for the authenticated logical
   `WorkspaceID`. It lives under the SelfMind asset base, not in the repository.
5. Read-only roots from `SELFMIND_SKILLS_ROOTS`.
6. The optional writable root from `SELFMIND_SKILLS_DIR`.
7. The control-tenant user root under the configured SelfMind asset base.

Typed execution scope is the hard workspace ancestor boundary. Discovery does
not walk above it and relabel home-level assets as workspace Skills. The first
matching canonical name wins; listing, selection-reference issuance, viewing,
and activation use the same resolver.

A directory Skill containing `.selfmind-developer-only` is invisible to the
SelfMind product runtime. Repository coding agents may still discover it.
Agent-specific compatibility entrypoints must redirect to the canonical body
rather than copy it.

Every discovered Skill records scope, root, writable state, provenance, and
lifecycle state. Read-only Skills may be viewed and activated but not mutated.
Disabling a read-only Skill writes control-tenant usage state rather than
editing its package.

Asset ownership and execution authority are independent:

- `ControlTenantID` owns Skills, versions, usage, and audit records.
- `PersonID` owns person memory and session data.
- `WorkspaceID`, `RunID`, `LeaseID`, and `ExecutionScopeKey` authorize runtime
  effects.

`_tenant_id` is a compatibility view, not the source of truth for new tools.
Resolving a read path never creates a directory. Only an authorized mutation
creates its owned storage path.

## Model and User Surfaces

- `skills_list` returns bounded metadata and issues selection references during
  an authenticated work unit.
- `skill_view` reads a named section, a bounded UTF-8 page, or one linked
  resource. Viewing does not count as activation.
- `skill_select` activates one server-resolved package. Model selection requires
  the current work unit's selection reference.
- `skill_fallback` ends the activation, records attributable negative evidence
  when available, removes the Skill instructions, and resumes ordinary
  planning in the same work unit.
- `/skill-name` performs explicit user invocation through the same activation
  path.
- `skill_bundle` shares one aggregate context budget across its members.
- `/skills bind` and `/skills unbind` manage one task affinity.
- Candidate, promotion, rejection, rollback, and binding mutations traverse the
  hidden lifecycle-management interface.

Legacy `skill:<name>` dispatch addresses remain registered with hidden exposure
as a rollback surface. They do not enter provider catalogs and must gain no new
callers. Removal requires explicit telemetry evidence and a release decision.

## Work Units, Binding, and Activation

A run may contain multiple work units, but one execution lane may activate at
most one Skill per work unit. A fallback does not select a second Skill in the
same unit. A later independent work unit may select another Skill.

Each activation fixes logical identity, version hash, package hash, resource
manifest, and delivery receipt. Existing activations never observe a later file
edit or promotion; a promoted version affects future activations only.

A task binding stores one logical `skill_key` as affinity, not authority.
Explicit task ids, `/resume`, and deterministic continuation may inherit it. A
weak display pre-label does not. Resolution rechecks root, scope, source, path,
trust, state, and precedence before every new activation. Failure returns to
ordinary planning rather than silently loading a same-named replacement.

Selection references bind work-unit identity to the selected package and
description hashes. Description drift invalidates routing. Package drift with
an unchanged description may be re-delivered once before a side effect; repeat
drift fails closed. Terminal work-unit cleanup deletes issued references.
Doctor and the dry-run-first maintenance command handle terminal or orphan rows
that escaped normal cleanup.

Terminal work units retain their own event cursors, outcome, verification,
duration, activation, and evidence. Parked work is terminal for that run but is
not success. One unit's later failure is not attributed to an already completed
unit.

## Context Delivery

Direct, bound, and model-selected activation use one `ActiveSkill` context
slice. Its default budget is:

`clamp(floor(context_length_tokens * 3%), 512, 2048)` tokens, with a separate
8192-byte UTF-8 ceiling.

The delivery builder reserves the envelope and linked-resource names, strips
metadata-only front matter, and freezes the remaining main bytes. Oversized
mains receive an explicit paged index, never a prefix presented as complete.
`skill_view` provides exact section or bounded-page access, and linked resources
remain lazy and immutable under the package hash.

When no binding is active, the metadata catalog receives 2% of the model
context with an 8000-byte ceiling. Unknown model metadata falls back to 512
tokens and 2048 bytes. Allocation is deterministic and reports included,
shortened, omitted, byte, token, and budget counts. A bound task receives no
tenant-wide directory dump.

Candidate ranking is metadata-only and supports ASCII tokens and CJK bigrams.
Canonical name matches win before score; scope breaks ties only after a real
text match. Ranking never loads full package bodies.

Rendering, activation, bundles, curation validation, Doctor, and HTTP reporting
consume the executing agent's resolved `RuntimeContextBudget`. They must not
reconstruct independent constants.

## Versions and Lifecycle

Logical Skills retain immutable `candidate`, `active`, `previous`, `rejected`,
and `quarantined` versions. Promotion verifies the written package identity
before the database projection changes. A quarantined exact package is omitted
from selection references and cannot be reconciled back to active merely
because its file remains present. Rollback restores a stored previous package
and affects future activations only.

The lifecycle module enforces mutation authority:

- `candidate_only` may create an immutable proposal but cannot promote, bind,
  edit, or publish it.
- `direct_active` is reserved for explicit management and the reviewed
  policy-authorized promotion path.
- Model arguments cannot manufacture either authority.

Manual, catalog-installed, bundled, pinned, external, workspace read-only, and
otherwise protected Skills are never rewritten automatically.

## Automatic Creation

Terminal work units become immutable workflow observations. Task/run events
remain the source of truth. A new automatic Skill proposal requires a bounded
cohort with at least three independent runs for the same person, workspace,
environment, and comparable workflow. Each success path must contain procedural
tool evidence; tool-free success cannot teach a procedure.

Automatic publication additionally requires every success observation to have
passed structured verification and to use attributable built-in tool metadata.
External-origin tools, MCP tools, network/delete/dangerous classes, delegation,
Skill management, and external watchers block automatic publication. They may
still produce a version candidate for explicit review.

The curator extracts stable common procedure, parameters, preconditions,
failure guards, recovery, and verification. It does not concatenate runs or
copy raw logs, credentials, absolute user paths, or session artifacts. The
canonical main contains Applicability, Inputs, Preconditions, Procedure,
Failure Guards, Recovery, and Verification. Optional detail belongs in lazy
references.

CREATE freezes `publication_scope` with the evidence. A single-workspace cohort
defaults to the control-managed workspace root, which avoids dirtying the
repository and prevents discovery from another logical workspace. Explicit
manual creation retains the user root. User-global automatic promotion still
requires a future explicit-user or compatible cross-workspace evidence path;
the curator does not silently widen scope.

## Failure, Recovery, and Repair

`skill_fallback` uses a closed defect taxonomy. A repair-eligible incident must
match a daemon-observed failed tool call from the same active work unit; model
text alone cannot establish attribution. Provider transients, environment
failures, cancellation, approval outcomes, and unknown categories do not
authorize repair.

The current work unit immediately removes the Skill and returns to ordinary
planning. A matching failure guard prevents the known bad path from being
repeated. If ordinary planning subsequently completes the same work unit with
explicitly passed verification and no recovery-tool failure, the frozen
incident and recovery evidence may nominate one PATCH candidate.

Automatic repair is limited to an active, writable, unpinned,
curator-created `agent-created` Skill with canonical section topology. The
candidate fixes the exact parent version, preserves name and linked resources,
changes one to three declared level-two sections, includes the failed section,
and leaves unrelated bytes unchanged. Concurrent active-version drift blocks
promotion. A successful promotion keeps the replaced version for rollback.

The daemon combines the declared defect category with its observed tool error
to freeze one repair class. Deterministic interface drift may auto-promote after
one verified recovery. Stable precondition drift may do so after one recovery
only for a workspace-scoped Skill. Semantic drift materializes an immutable
candidate after one incident but requires three independent comparable
recoveries with the same failure signature for automatic promotion.
Not-applicable evidence may remain a reviewable applicability candidate but
never auto-promotes; transient/environment evidence cannot create a repair.

Candidate bodies remain immutable while later comparable semantic cohorts are
stored as separate immutable evidence snapshots. An automatic promotion checks
the snapshots attached to the exact version, so replaying one run or presenting
an unattached digest cannot satisfy the threshold.

Failure guards include the environment fingerprint. An attributable regression
of a curator repair quarantines that exact child version. A previous version is
rollback-eligible only when it has independent verification evidence for the
current environment; the eligibility check does not itself authorize rollback.

## Diagnostics and Compatibility

Durable activations and terminal work-unit outcomes are the canonical source
for Skill use, completion, fallback, and failure statistics. Sidecar usage files
are inventory-recency hints only; legacy metric rows are historical.

Doctor previews the actual provider-wire catalog and distinguishes
`registered_active`, `hidden`, and `provider_visible`. Per-Skill compatibility
tools must remain hidden. Doctor validates package and delivery receipts,
candidate drift, context budgets, terminal-reference cleanup, description
ceilings, and historical activation contracts, and provides safe repair and
verification commands when one exists.

Curator versions persist their last verified time, verification environment,
and a fingerprint of observed environment/tool contracts. A changed supplied
dependency fingerprint or a 30-day verification gap creates a bounded review
nomination only; it does not call a foreground model or authorize a patch.

Historical person-partitioned Skill assets remain read-only compatibility
inputs while new writes target the control tenant. Migration and orphan cleanup
are dry-run first; conflicts remain untouched and cleanup uses recoverable
quarantine while the gateway is stopped.

## Workflow Optimization Is Separate

`batch_read` is currently a bounded local read-only batching tool. Its children
still traverse the dispatcher, scope, budget, and durable event path. It is not
a compiled Recipe and does not prove task-level benefit.

The current workflow-profile candidate lifecycle does not execute a candidate
during "shadow" or compare candidate evidence with a baseline. Ordinary runs
advance observation counts only: they do not increment legacy shadow matches,
revive degraded candidates, or authorize runtime advice. Advice requires a
separate verified candidate-versus-baseline comparison contract, which the
current profiler does not create.

Recipe discovery, provenance, real replay/live shadow, canary rollout, and
cross-task optimization require a separate architecture decision and release
evidence. They cannot modify Skill publication or repair rules.

## Change Checklist

When changing Skills:

1. Preserve the catalog, runtime, lifecycle, and curator module seams.
2. Keep model-visible tools generic and bounded.
3. Preserve typed ownership, execution scope, package identity, and mutation
   authority.
4. Test through the production activation path for user-visible behavior.
5. Use Go tests for deterministic ranking, evidence, version, migration, and
   state-machine mechanics.
6. Update `docs/STATUS.md` only after capability or priority actually changes.
7. Do not describe observations, heuristics, or ordinary success as shadow or
   verification evidence.
