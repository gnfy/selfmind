# Skill Architecture

SelfMind treats skills as durable procedural context, not as executable tool
plugins. A skill can guide the agent, expose references, templates, scripts, and
assets, but commands from a skill must still be run through normal tools and
safety checks.

## Goals

- Support user-global skills, workspace skills, and Codex-compatible
  `.agents/skills` without duplicating loader logic.
- Keep skill discovery progressive: compact metadata first, full `SKILL.md`
  only when explicitly viewed or invoked.
- Preserve multi-endpoint behavior. CLI, HTTP, IM, scheduled jobs, and
  background review use the same control-tenant skill service and audit log.
- Avoid context bloat. Large skill content is truncated safely on direct
  invocation and should point to linked files for details.

## Roots And Precedence

Skill discovery flows through `internal/tools/skill_service.go`.

Default visible roots, in priority order:

1. Workspace `.selfmind/skills` from the current directory and nearby parents.
2. Workspace `.agents/skills` for Codex-compatible repo skills.
3. Workspace `skills/` for lightweight project-local skills.
4. Optional environment roots from `SELFMIND_SKILLS_ROOTS`.
5. Optional writable environment root from `SELFMIND_SKILLS_DIR`.
6. Control-tenant root `~/.selfmind/<control-tenant>/skills`.

The first matching skill name wins for list/view/slash invocation. Registry
reload registers lower-priority roots first, then higher-priority roots, so the
runtime tool registry sees the same winner.

By default, mutations write to the user root. Set
`SELFMIND_SKILLS_WRITE_SCOPE=workspace` only when a writable workspace
`.selfmind/skills` root should receive new skills.

## Read And Write Boundaries

Each discovered skill carries:

- `scope`: `workspace`, `user`, or `external`.
- `root`: the containing root.
- `writable`: whether mutation tools may modify it.
- `source`: lifecycle provenance such as `manual`, `agent-created`, or
  `catalog-installed`.
- `state`: `active`, `stale`, `disabled`, or `archived`.

Read-only skills can be listed, viewed, invoked, bundled, and audited. They
cannot be edited, patched, archived, deleted, or have support files changed.
Mutation tools must return a clear read-only error instead of copying or
replacing silently.

Disable/enable is user-scoped lifecycle metadata. Disabling a read-only skill
does not edit the skill file; it writes tenant usage state and makes slash or
bundle invocation skip that skill until it is enabled again.

User-visible usage state, pins, views, and skill audit records stay under the
control-tenant partition, so all accounts belonging to the same local control
plane see the same durable procedural assets. Person memory/session data does
not move into that partition, and workspace trust still follows the execution
scope rather than the skill storage owner.

## Invocation Identity And Migration

Every native tool receives a typed invocation scope. Skill storage resolves
from `ControlTenantID`; workspace discovery, trust, approval, and process
control resolve from `WorkspaceID`, `RunID`, `LeaseID`, and
`ExecutionScopeKey`; person memory/session assets resolve from `PersonID`.
`_tenant_id` remains a compatibility view only and must not become the source
of truth for new tools.

Historic builds could write agent-created skills and skill audit rows under a
`person_*` partition. Read-only discovery keeps those assets visible for one
compatibility window, while new mutations always target the control tenant.
Use `selfmind maintenance migrate-skills` to preview the migration and add
`--apply` only after reviewing conflicts. The control copy wins; identical
content is deduplicated; conflicting person copies remain untouched. Migrated
agent-created skills carry provenance and a governance grace period so the
curator cannot archive them immediately after migration.

## Invocation Surfaces

- `skills_list`: compact metadata only.
- `skill_view`: inspect full `SKILL.md` or one linked file under `references/`,
  `templates/`, `scripts/`, or `assets/`; inspection is not execution
  attribution.
- `skill_select`: activate one resolved Skill version for the current work
  unit. Omitting its name resolves the related task's explicit/default binding.
- `skill_fallback`: end that activation, record a negative guard, remove its
  instructions, and continue the same work unit with ordinary planning.
- `/skill-name`: direct user invocation, bundle-first then skill.
- `skill_bundle`: groups multiple skills into one slash command.
- `skill_manage`: mutation and hot reload.
- `skill_catalog`: install and audit.
- `/skills bind <name>` and `/skills unbind`: explicitly change the current
  task's one default logical Skill.
- `/skills candidates|candidate|promote|reject|rollback`: explicit candidate
  and version management. The backing management tool is hidden from models.

Legacy dynamic `skill:<name>` tool registrations are compatibility shims only.
They return an instruction-only message and must not execute code blocks from
`SKILL.md`.

## Work Units, Bindings, And Versions

A run may contain several top-level work units, but each work unit/execution
lane may activate at most one Skill. `update_plan` supplies stable work-unit
projections for genuinely multi-step or multi-task work. Inspect/edit/verify
steps for one objective remain one unit; only the first step, an explicit
`work_unit` marker, a returned stable `work_unit_id`, or a deterministic related
task begins another unit. The runtime returns assigned IDs and later complete
plan snapshots must echo them, so wording changes and reordering cannot retarget
historical evidence by array position. Moving to another work unit expires the
prior Skill body; a fallback also prevents selecting a replacement Skill in the
same unit. Parent and delegated execution lanes keep their own activation
bodies rather than merging them into one prompt.

Each unit records its own start/finish event cursors. A plan transition to
`completed` closes the unit and its live activation, then derives verification
from that cursor window's durable evidence. Run finalization only fills units
that are still pending or active. It never copies a later unit's failure over an
already completed unit, and duration is taken from the unit window rather than
dividing total run time evenly. A run that ends at an approval, clarification,
external wait, or queued finalization closes its unresolved unit and activation
as `parked`: this is terminal for that run but neither completion nor fallback.
A clean parked unit remains audit evidence, cannot create task affinity, and
the parked status itself does not advance success/failure counters. Independent
tool-failure or user-correction evidence still follows the ordinary negative
evidence policy. A completed unit before the wait retains its own positive
evidence.

A task may bind one logical `skill_key`. This is an affinity, not authority and
not a permanent pin to one content hash. Only deterministic attachments such
as an explicit task id, `/resume`, or a confirmed continuation inherit the
binding. A weak display pre-label never loads it. Resolution rechecks root,
scope, source, relative path, trust, state, and precedence; a mismatch or
unavailable Skill returns to ordinary planning instead of silently loading a
same-named replacement.

Each activation fixes one immutable version hash. `skill_versions` retains
`candidate`, `active`, `previous`, and `rejected` states while the active file
remains compatible with the existing Skill ecosystem. Candidate and previous
bodies stay outside foreground context. Promotion verifies the written file
hash before changing the active database projection; rollback writes a stored
previous body and affects only future activations.

## Context Budget

Direct or bound invocation injects the chosen `SKILL.md` body plus linked-file
names in a separate `ActiveSkill` runtime-context slice. Its target budget is
4 KiB and hard limit is 8 KiB, both inside the existing composer total rather
than added on top. If the body is too large, SelfMind appends an explicit
truncation note and the agent should use `skill_view(name, file_path)` for the
necessary linked files. Candidate metadata is capped at three entries when no
binding is active; a bound task receives no tenant-wide directory dump.
Candidate lookup is deterministic and lexical. ASCII words and CJK bigrams are
both supported; scope is only a tie-break after a real text match, so unrelated
workspace Skills are not offered merely because they are nearby. Candidate
metadata is refreshed whenever the plan enters a new work unit.

Best practice for new skills:

- Keep `SKILL.md` lean and procedural.
- Move detailed examples, schemas, and large references into linked files.
- Reference linked files from `SKILL.md` with clear "when to read" guidance.
- Avoid duplicating the same content in both `SKILL.md` and references.

## Governance

Legacy Background Review and Reflection paths do not create or rewrite active
Skills. The durable cohort-driven curator is the sole automatic proposal
authority. It creates an immutable candidate first and may publish only a
verified, repeated, read-only cohort to a writable, unpinned, `agent-created`
Skill. Local writes, shell/network activity, external effects, manual,
catalog-installed, bundled, pinned, workspace read-only, and external Skills
remain candidates or require explicit user management.

Mutation authority comes only from typed invocation scope and fails closed when
the scope is absent or unknown. `candidate_only` can create an immutable
candidate but cannot promote, bind, edit, or publish it; `direct_active` is
reserved for explicit management and the risk-gated promotion path. Model JSON
arguments cannot manufacture either authority.

The authenticated `/v1/dispatch` management surface installs `direct_active`
only for `skill_manage` and `skill_lifecycle_manage`. All `/skills` mutations,
including archive, traverse that daemon-owned tool path; thin clients do not
move Skill files directly or bypass registry reload.

All durable mutations must write tenant learning records through the shared
audit helpers. Do not add channel-specific or tool-specific history files.

## Workflow Profiling and Safe Evolution

Skill use is attributed only by a durable `skill.activated` record/event from
`skill_select` or a trusted task binding. `skill.viewed`, selection, activation,
completion, and fallback are distinct. Terminal work units become immutable
workflow observations with their own outcome, verification, tool families,
Skill version, cost, duration, and correction/failure evidence. They are
derived data; task/run events remain the source of truth.

The Skill curator runs only when a bounded comparable cohort is ready. The
initial gate requires three independent verified successes for the same person,
workspace and environment, plus up to two relevant negative observations.
Success observations with no normalized tool sequence cannot nominate a
curator proposal because they contain no procedural evidence. External-watch
finalization runs remain audit data and are excluded from nomination, including
when an ordinary foreground run later anchors a similar cohort.
Deterministic nomination compares normalized goal features and tool families;
for an existing Skill it also requires the exact `skill_key@version`. This
allows paraphrased and CJK tasks to meet again without grouping unrelated
versions. The curator remains the semantic gate: similarity can nominate a
bounded cohort but cannot authorize a merge. Its proposal is frozen in the durable
maintenance job before application, so crash recovery cannot ask the model to
invent a different candidate. The required candidate sections are
Applicability, Inputs, Preconditions, Procedure, Failure Guards, Recovery, and
Verification. Correctness and verification outrank context/turn savings.
Only explicit `passed` and `not_applicable` observations qualify for automatic
read-only promotion; an empty verification state does not.

For an attributable non-transient defect, `skill_fallback` records only a
negative guard for the failing version and normalized input shape; transient,
network/provider, environment-drift, cancellation, approval, and unknown
incidents do not become durable guards. A guard never records or executes an
unverified replacement command. A later exact match skips the Skill and uses
ordinary planning. Repeated matches suspend the task binding. Promoting a
repair resolves guards for the replaced version.

Repeated local read-only workflows may also create a `batch_read` fast-path
candidate. This optimization consumes true activation/version evidence but is
separate from the Skill lifecycle. The
candidate lifecycle is `candidate -> shadow -> eligible -> enabled`; promotion
requires repeated observations and bounded shadow evidence. `batch_read`
accepts at most eight `read_file`, `search_files`, or `ls_r` operations. It
cannot invoke shell commands, writes, credentials, network actions, arbitrary
Python, or another batch. Each inner item still traverses the normal dispatcher
and execution scope, consumes the ordinary per-turn tool budget, emits durable
evidence, and requests ordinary-tool
fallback on failure.

Parked runs are still profiled for cost and diagnostics, but they do not advance
the batch candidate observation count or its shadow success/failure denominator.

An enabled candidate is recommended only to the same task continuation. Any
failed batch item degrades the candidate immediately and stores a deterministic
repair proposal; it is no longer recommended until reviewed or re-observed.
Manual and pinned skills are never rewritten by this mechanism. The model still
owns the plan and every write/action decision.

## Catalog Provenance

Catalog installs must preserve durable install provenance:

- Store install records under `~/.selfmind/<control-tenant>/skills/.catalog/lock.json`
  and mark usage source as `catalog-installed`.
- Reject same-name directory or legacy `.md` collisions by default; only
  overwrite when the user explicitly passes `--force`.
- A forced reinstall must move the previous copy into
  `~/.selfmind/<control-tenant>/skills/.catalog/backups/` before writing the new one.
  Never silently replace a user-installed or hand-written skill.

## Adding New Skill Features

When adding a new skill-related feature:

1. Use `SkillRootsForTenant` or `WritableSkillRootForTenant`; do not hand-roll
   `~/.selfmind/.../skills` paths.
2. Preserve scope and writable checks on mutations.
3. Keep list output compact and human-readable.
4. Keep full content loading explicit.
5. Update tests for user, workspace, read-only, and slash/bundle behavior.
6. Update this document and `AGENTS.md` if the development contract changes.
