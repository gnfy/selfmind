# Skill Learning, Automatic Promotion, and Repair

## Decision Status

This is the accepted design for SelfMind's automatic Skill learning and
self-repair. The core P0-P3 mechanics are implemented; sustained production
validation and a user-global promotion route remain open. It is a design
decision, not a second active implementation plan; `docs/STATUS.md` and the
registered active plan continue to control priority and capability claims.

The decision refines the current contract in
[`skills-architecture.md`](skills-architecture.md). When this document names a
target not yet present in the runtime, it says so explicitly.

## Problem

SelfMind must be able to turn repeated successful work into a reusable Skill
without requiring the user to curate every procedure manually. Published Skills
also become stale: tools change, validation interfaces evolve, fields move, and
preconditions drift. A stale Skill must not trap the agent in repeated failure,
but one transient error or one accidental workaround must not rewrite durable
instructions for every future task.

Three concepts need separate evidence and state:

1. Learning a new reusable procedure.
2. Recovering the current task after an active Skill fails.
3. Publishing a repaired version for future activations.

The system must automate all three where evidence is strong while preserving
fallback, scope, rollback, and user authority.

## Decision Summary

1. A Skill remains an instruction asset. Publication never grants execution or
   approval authority.
2. Automatic learning always materializes an immutable version candidate before
   promotion.
3. A new low-risk Skill may auto-promote only after at least three independent,
   comparable, explicitly verified work units establish a stable procedure.
4. Learned Skills are workspace-scoped by default in a control-managed root
   outside the repository. User-global publication requires compatible evidence
   from more than one workspace or explicit user intent; that widening route is
   not yet implemented.
5. An activation pins one version. No foreground run observes an in-place
   rewrite.
6. A Skill incident causes immediate same-turn fallback to ordinary planning.
   Recovery of the user's task has priority over repairing the Skill.
7. Repair always creates a child version candidate with the failed active
   version as its exact parent. The old version is never edited in place.
8. Deterministic interface drift may auto-promote after one attributable
   incident plus verified same-work-unit recovery. Semantic drift requires
   repeated independent recovery evidence or explicit review.
9. Automatic repair is narrow. Broad procedure replacement, resource changes,
   scope expansion, or new effect classes remain candidates for user review.
10. Performance optimization and Fast Path evolution are separate from Skill
    correctness and cannot use these promotion rules.

## Domain Model

### Logical Skill

A durable identity representing one reusable procedure. It has at most one
active version and may have candidate, previous, rejected, and quarantined
versions.

### Version Candidate

An immutable proposed package. A candidate is inspectable and rejectable but is
not available to normal selection, task binding, or activation.

### Publication

The atomic transition that makes a validated candidate the active version for
future activations. Publication is not execution.

### Activation

One work unit loading one immutable package version and delivery receipt.

### Skill Incident

A version-specific failure with an observed tool call, closed error category,
failed step, environment identity, and failure signature. A final-answer claim
or generic task failure is not an incident.

### Recovery Proof

Structured evidence that ordinary planning completed the same work unit after
fallback and passed verification independent of the failed Skill instructions.

### Repair Candidate

A PATCH version candidate whose parent is the exact failed active version and
whose changed sections are attributable to the incident and recovery.

### Failure Guard

A version-, environment-, and failure-specific durable guard that prevents
known-bad instructions from being repeated while preserving ordinary planning.

## Module Design

The public model interface remains the existing small catalog/runtime surface:
list metadata, inspect content, activate one selected package, and fall back.
Promotion and rollback remain hidden management operations.

The learning implementation belongs behind one deep post-run curation module.
Its interface consumes a frozen `SkillEvidenceDigest` and returns exactly one
decision:

- `SKIP`: evidence is insufficient, heterogeneous, unsafe, or irrelevant.
- `CREATE`: propose a new logical Skill and initial package.
- `PATCH`: propose a child version for one existing logical Skill.

Candidate creation, deterministic validation, policy authorization, atomic
publication, and event recording remain outside the curator model. The model
proposes content; it does not decide its own authority.

No second extractor, synchronous foreground curator call, or channel-specific
learning path is introduced.

## New-Skill Evidence

"Three successes" is a noise floor, not the complete publication rule. A cohort
is eligible only when all of the following hold.

### Independent

- At least three distinct run ids and terminal work units.
- Retries, replay, daemon recovery, duplicate delivery, and repeated projection
  of one work unit count once.
- Parked, cancelled, external-watch finalization, and tool-free observations do
  not count as success-path evidence.

### Comparable

- Same person and logical workspace scope.
- Compatible environment fingerprint and available tool contracts.
- Compatible normalized goal features and workflow family.
- For repair evidence, the exact same `skill_key@version`.
- Similarity may nominate a cohort but never authorizes grouping or promotion.

### Verified

- Outcome is terminal success.
- Verification state is explicitly `passed`.
- Verification references structured tool results or artifacts.
- The verification criterion comes from the task, user, detected project
  contract, or typed external contract, not solely from the Skill being judged.
- User correction, fallback, or unresolved contradictory evidence blocks clean
  automatic publication.

### Procedural

- Every success path contains attributable tool evidence.
- Stable common steps are extracted; runs are never concatenated.
- Variable values become inputs, preconditions, or bounded parameters.
- Raw logs, credentials, tokens, absolute user paths, and session-specific
  artifacts are excluded.
- The main body remains independently actionable under the real production
  delivery budget.

## Automatic Publication Policy

Publication is eligible when the new-Skill evidence gate passes and every
observed tool is a trusted built-in with durable origin and operation-class
metadata. Publishing instructions does not pre-approve their later actions, so
built-in local write or shell-assisted procedures may be learned when their
actual effects were verified and did not enter a prohibited class.

Automatic publication is blocked when evidence includes:

- external-origin or MCP tools;
- network, delete, dangerous, or delegated effects;
- external watchers;
- Skill lifecycle or mutation tools;
- missing tool provenance;
- environment-specific constants that cannot be parameterized;
- a name collision or changed active parent;
- a package that fails canonical shape, safety, or exact delivery validation.

Blocked proposals remain immutable candidates for explicit management. They do
not silently disappear or enter foreground context.

## Scope and Reuse

The default scope for an automatically learned Skill is the logical workspace
that supplied its evidence. This prevents one repository's validation fields,
paths, commands, or conventions from leaking into unrelated work.

A learned Skill may become user-global only when either:

1. The user explicitly requests global reuse; or
2. At least two independent workspaces provide compatible verified cohorts and
   the extracted procedure contains no workspace-specific path, dependency, or
   policy.

Promotion between scopes creates a new logical package with provenance linking
the source evidence. It does not move or overwrite the workspace Skill.

Repairs inherit the active Skill's scope. Repair can narrow applicability but
cannot expand workspace, tenant, tool, network, credential, or approval scope.

## Foreground Failure and Recovery

The foreground loop prioritizes task completion:

```text
active version v1
  -> attributable failure
  -> skill_fallback + failure guard
  -> remove v1 instructions from the work unit
  -> ordinary diagnosis and planning
  -> verified recovery or honest failure
```

Fallback never activates a second Skill in the same work unit. Ordinary tools
remain available under the same execution scope and approval policy. A rejected
or unanswered approval, cancellation, provider outage, and environment failure
are not silently relabelled as Skill defects.

The recovery path records the failed call, category, normalized input shape,
failed section or step, diagnostic evidence, replacement procedure, and final
verification. It stores bounded references and hashes rather than raw secrets or
unbounded outputs.

## Repair Evidence Classes

Repair thresholds depend on what changed.

| Class | Example | Automatic action |
| --- | --- | --- |
| Deterministic interface drift | Field renamed, typed schema changed, command flag removed | One attributable incident plus verified same-work-unit recovery may publish a narrow repair |
| Stable precondition drift | Required manifest moved or deterministic prerequisite changed | One incident may repair when the new precondition is workspace-scoped and directly verified |
| Semantic behavior drift | Field still exists but meaning or acceptable value changed | Require three independent comparable recoveries or explicit user promotion |
| Not applicable | Skill selected for the wrong task/environment | Narrow applicability or binding guard; do not rewrite procedure from one case |
| Transient/environmental | Network outage, provider failure, missing permission, cancellation, approval denial | No Skill repair |
| Performance-only | Procedure works but uses extra reads or turns | No Skill repair; evaluate separately as workflow optimization |

The daemon freezes this class from the declared defect category plus observed
tool evidence. Candidate materialization and automatic publication use separate
gates, so one semantic recovery can produce a reviewable candidate without
claiming that the three-run publication threshold is satisfied.

## Repair Candidate Contract

An automatic repair candidate must:

- target the exact active logical Skill and parent version hash;
- originate from a daemon-observed incident and independent recovery proof;
- preserve the Skill name and scope;
- change one to three canonical level-two sections;
- include the section containing the failed step;
- preserve unrelated bytes and all linked resources;
- preserve or reduce delivery bytes/tokens for already paged legacy mains;
- add no credentials, absolute user paths, raw logs, or new execution authority;
- pass the same package, delivery, and hash validation as ordinary promotion.

If the repair needs more than three sections, resource changes, a new tool
origin, broader applicability, or changed effect classes, the system creates a
candidate and stops before publication.

Promotion uses optimistic concurrency. If active content changed after the
candidate was created, publication fails closed. Existing activations retain
their pinned package. The replaced version becomes previous and remains
available for rollback.

## Guards, Suspension, and Rollback

One attributable incident creates a guard for the exact version, environment,
failure signature, and failed step. A later exact match skips the known-bad path
and uses ordinary planning without waiting for another failed execution.

Repeated matching incidents suspend the affected task binding. Cross-task or
cross-workspace suspension of the entire active version requires repeated hard
evidence; one environment-specific failure must not disable a globally valid
Skill.

A repaired version that produces a hard attributable regression is quarantined
as an exact package version and omitted from future selection. It does not
regain trust from unrelated ordinary-run success. The previous version is
rollback-eligible only when it is compatible with the current environment and
has independent verification evidence; the eligibility check does not itself
authorize rollback.

## Detecting Silent Staleness

Failure-driven repair cannot detect every stale Skill. A procedure may continue
to run while producing incomplete or inefficient results. SelfMind therefore
tracks, without adding a foreground LLM call:

- last verified activation time;
- environment and relevant observed tool-contract fingerprints;
- verification pass, fallback, correction, and guard rates by exact version;
- applicability misses;
- dependencies declared or observed in Preconditions and Verification.

A changed fingerprint or long-unverified version creates a bounded review
nomination. The current implementation exposes nominations through diagnostics;
it does not schedule an automatic reviewer. A future review may produce `SKIP`
or an immutable candidate, but publication still requires the corresponding
creation or repair evidence gate.

SelfMind does not poll arbitrary external APIs merely to keep Skills fresh.
Typed external tools may expose version metadata during ordinary use; otherwise
drift is discovered through real work or explicit user review.

## Observability

Diagnostics and reports distinguish:

- observations collected;
- cohorts ready;
- candidates created;
- automatic promotion allowed or blocked, with reason;
- activations and verification outcomes by exact version;
- fallback and incident categories;
- recovery attempted and recovery verified;
- repair candidates promoted, rejected, conflicted, quarantined, or rolled
  back;
- workspace-scoped versus user-global learned assets.

No metric may call an ordinary successful run a shadow match. No dashboard may
combine proposal evidence, publication authorization, activation usage, and
verification into one success counter.

## Implementation Status

The registered active plan now owns this work. The P0-P3 core landed on
2026-08-26 with control schema v5. The sections below record the implementation
order and the remaining evidence boundary.

### P0: Correct semantics

- Keep workflow profiling, but default the unrelated Fast Path mode to
  observation-only until true comparison exists.
- Stop ordinary success from advancing shadow counters or reviving degraded
  candidates.
- Remove "evidence-backed Fast Path" wording from runtime advice.
- Separate selection references from version candidates in internal names and
  user-facing documentation while retaining wire compatibility.

### P1: Workspace-first creation

- Carry the selected publication scope in frozen evidence and lifecycle
  commands.
- Default single-workspace cohorts to workspace storage.
- Open: add explicit cross-workspace or user-authorized promotion to user scope.
- Preserve provenance and collision checks across roots.

### P2: Tiered repair

- Persist deterministic-interface, stable-precondition, semantic, not-applicable,
  and transient classifications.
- Apply class-specific repair thresholds.
- Add environment-specific guards, version quarantine, and rollback eligibility
  checks.

### P3: Staleness review and evidence

- Persist relevant dependency fingerprints and last verified time.
- Nominate bounded review without adding a foreground model call.
- Open: validate the full lifecycle over sustained real personal workflows before
  changing thresholds or broadening automatic publication.

Implemented mechanics include observation-only workflow profiling,
control-managed workspace publication, immutable candidate evidence snapshots,
class-specific repair thresholds, environment-bound guards, exact-version
quarantine, compatible-previous rollback eligibility, dependency fingerprints,
last-verified timestamps, and bounded review nominations. Still open are the
explicit/cross-workspace user-global promotion route and sustained daily-driver
and installed-binary evidence.

## Testing Decisions

The highest test seam is the production message path through daemon discovery,
activation, fallback, post-run maintenance, and a later activation. Do not add a
parallel test-only learning path.

Production-path eval cases must prove:

1. Three independent comparable verified work units publish one new Skill.
2. Retry or replay of one work unit cannot satisfy the threshold.
3. A learned workspace Skill is not offered in another workspace.
4. Publication never bypasses ordinary tool safety or approval.
5. A stale Skill falls back, ordinary planning recovers, and only a future
   activation receives the repaired version.
6. Deterministic interface drift can publish one narrow repair.
7. Provider, permission, approval, cancellation, and environment failures do
   not repair the Skill.
8. Semantic drift remains a candidate until repeated evidence or explicit
   promotion exists.
9. Parent-version drift blocks repair promotion.
10. A repaired regression quarantines the version and preserves a safe ordinary
    path.

Go tests cover deterministic cohort comparison, independent-run counting,
classification, eligibility, section-limited patching, package identity,
optimistic promotion, scope isolation, guard matching, rollback, migrations,
and idempotency.

## Out of Scope

- Executable Skill plugins or persistent autonomous scripts.
- Automatic approval, credential access, network access, deletion, or delegated
  execution.
- Ingress LLM classification.
- Cross-person learning or publication.
- Automatic rewriting of manual, pinned, catalog-installed, bundled, external,
  or read-only Skills.
- Recipe compilation, PTC, Fast Path shadow, or canary optimization.
- Background polling of arbitrary external systems for version changes.
