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
- Avoid context bloat without claiming a prefix is complete. Oversized main
  instructions use explicit paging and should move optional detail into linked
  resources.

## Roots And Precedence

Skill discovery flows through `internal/tools/skill_service.go`.

Default visible roots, in priority order:

1. Workspace `.selfmind/skills` at the typed execution-scope root. Direct local
   callers without a run scope may discover it from the cwd and nearby parents.
2. Workspace `.agents/skills` for Codex-compatible repo skills.
3. Workspace `skills/` for lightweight project-local skills.
4. Optional environment roots from `SELFMIND_SKILLS_ROOTS`.
5. Optional writable environment root from `SELFMIND_SKILLS_DIR`.
6. Control-tenant root `~/.selfmind/<control-tenant>/skills`.

Repository development agents may also use `.agents/skills` for workflows that
must never enter SelfMind's product runtime. A directory-form Skill containing
`.selfmind-developer-only` is omitted from SelfMind list, search, and invocation.
The marker applies only to that Skill directory;
coding-agent discovery is unchanged. This keeps one tracked Agent Skills source
without publishing development operations to SelfMind users. Agent-specific
directories may contain only thin entrypoints that redirect to the canonical
`.agents/skills` body; they must not fork the instructions.

The first matching skill name wins for list/view/slash invocation. Discovery,
candidate issuance, view, and activation share this resolver; a model-visible
name is never used as a second independent identity lookup.
Typed run scope is also the hard ancestor boundary: discovery never walks above
the registered workspace into a user's home-level `.selfmind/skills` and
relabels those control assets as workspace Skills.

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

The daemon resolves one immutable asset base from `evolution.skills_dir` (or
`~/.selfmind` by default) and injects it into every skill, learning-audit, and
post-run maintenance path. Evaluation uses a temporary base. Resolving a read
path never creates `skills/` or `learning/`; only an actual write creates the
directory it owns. The thin TUI resolves `/skills`, `/curator`, and
`/skill-name` through the daemon management surface, so a custom storage base
cannot fall back to the TUI process HOME. Background curation fails closed when
that injected storage is unavailable; it never manufactures a second default.

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
curator cannot archive them immediately after migration. Archived assets under
`.archive` are migrated as archived assets rather than being mistaken for an
empty partition.

After migration, `selfmind maintenance cleanup-person-partitions` previews
filesystem partitions whose person id no longer exists in `control.db`.
`--apply` is accepted only while the gateway is stopped and moves those
partitions into recoverable quarantine; known persons are always protected.

## Invocation Surfaces

- `skills_list`: budgeted metadata only. In an authenticated run it issues a
  durable `candidate_ref` for every returned package identity.
- `skill_view`: inspect the main body by named section or bounded byte page, or
  one linked file under `references/`, `templates/`, `scripts/`, or `assets/`;
  inspection is not execution attribution. An active activation reads its
  pinned package bytes from `control.db`, not a newly changed filesystem copy.
- `skill_select`: activate one server-resolved candidate package for the current
  work unit. Model selection requires `candidate_ref`; omission is reserved for
  the related task's explicit/default binding.
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

During the compatibility window, per-Skill `skill:<name>` dispatch entries stay
registered with `exposure=hidden`: explicit legacy dispatch remains possible,
but those entries never enter the provider catalog and are not deferred.
`/skill-name` already resolves through one generic typed daemon path and creates
the same activation receipt as model selection and task binding. The hidden
registrations can be removed only after the shadow window confirms no remaining
caller depends on them.

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

Selection identity is server-issued and work-unit-scoped. A `candidate_ref`
maps durably to `skill_key`, main version, package hash, and description hash,
so it survives worker changes, daemon restart, resume, and endpoint changes.
Description drift invalidates the routing decision and requires a refreshed
list. Package drift with an unchanged description may be re-delivered once
before any side effect; a second drift fails closed and ordinary planning
continues. Issued refs remain resolvable until the work unit becomes terminal,
then the terminal transaction deletes them.

Terminal cleanup is the normal retention path, but it is not the only recovery
path. Doctor reports refs owned by terminal or missing work units separately
from live refs and points to `selfmind maintenance prune-skill-candidate-refs`.
That command is dry-run by default; `--apply` deletes only terminal/orphan refs
in one control-store transaction and a second Doctor run verifies the result.
It never expires a ref owned by a non-terminal work unit.

Contract-v1 activations also freeze `package_hash`, a resource manifest, and a
delivery receipt (`delivered_main`, hash, and byte count). Historical contract-0
rows keep their legacy meaning. New activations never re-read a changed main or
resource from disk. Only `name` and `activation_id` repeat in the prompt;
control-plane hashes and paths remain in tool results, events, and durable rows.

## Context Budget

Direct, bound, and model-selected invocation use one `ActiveSkill` runtime
slice. Its slice budget is
`clamp(floor(context_length_tokens * 3%), 512, 2048)` tokens with a separate
8192 UTF-8 byte ceiling; the delivery builder reserves the prompt envelope and
linked-file names, strips metadata-only YAML front matter, and freezes the
remaining main bytes once at activation. The runtime
bundle retains its existing 8 KiB non-Skill allowance and adds the larger of
the active-main or candidate-catalog slice. Active instructions outrank
recall/memory and are protected byte-for-byte through normal compaction and
provider-window recovery.

When the instruction body exceeds its activation budget, the model receives a
bounded `[PAGED SKILL MAIN]` index, never a silent prefix presented as complete.
`skill_view(section=...)` returns one exact level-two section;
`offset_bytes`/`limit_bytes` provides UTF-8-safe pages up to 8 KiB. Linked
resources remain lazy and immutable under the activation package hash.

When no binding is active, candidate catalog token budget is
`floor(context_length_tokens * 2%)`, with a separate 8000 UTF-8 byte ceiling.
Unknown model metadata explicitly falls back to 512 tokens and 2048 bytes for
both Skill surfaces. Allocation first reserves a minimum identity line in
deterministic rank order, gives all included descriptions a fair short baseline,
then completes higher-ranked descriptions before lower-ranked ones. It omits
entries only when even existence lines do not fit. Every render reports total,
included, full, shortened, omitted, bytes, tokens, and both budgets. A bound
task receives no tenant-wide directory dump.
Candidate lookup uses deterministic, metadata-only BM25F ranking over `name`
and `description`; it never loads full Skill bodies. ASCII words and CJK
bigrams are both supported. Rare corpus terms receive more weight than common
terms, name matches are weighted above description matches, and field-length
normalization prevents long descriptions from winning through incidental
matches. Multi-token CJK queries require at least two matching query terms
unless the canonical Skill name is explicitly present, which prevents one
incidental bigram from nominating an unrelated Skill. An explicit canonical Skill name
wins before score; scope is only a tie-break after a real text match, so
unrelated workspace Skills are not offered merely because they are nearby. The
same scorer backs bounded `skills_list` searches; search responses report total
matches and allocation. Candidate metadata and refs are refreshed whenever the
plan enters a new work unit. The synchronous `update_plan` result includes that
unit's canonical byte/token-bounded `skill_catalog`, so the model can select by
the new unit's valid ref immediately without repeating full descriptions in an
unbounded JSON array.

Every Skill surface consumes one resolved `RuntimeContextBudget`; rendering,
telemetry, curation validation, Doctor, and HTTP reporting must not reconstruct
fallback constants independently. CREATE validation runs the same delivery
builder as activation after sorting the proposed resource paths, and may publish
automatically only when the exact byte and token result is `full`. The budget
must include the envelope and resource-manifest reserve. A bundle has one
aggregate budget derived from the executing agent and allocates it fairly and
deterministically across members; it does not grant every member a full active
Skill budget. Client-reported context budgets come from the same gateway/agent
budget used for the request. Worker pools currently require homogeneous budgets
and must assert that invariant until per-worker budgets travel with checkout.

Best practice for new skills:

- Keep `SKILL.md` lean and procedural.
- Move detailed examples, schemas, and large references into linked files.
- Reference linked files from `SKILL.md` with clear "when to read" guidance.
- Avoid duplicating the same content in both `SKILL.md` and references.

## Governance

Legacy Background Review and Reflection paths do not create or rewrite active
Skills. The durable cohort-driven curator is the sole automatic proposal
authority. It creates an immutable candidate first. Three independent,
comparable, verified successes may publish a new writable, unpinned,
`agent-created` Skill even when the learned procedure used ordinary built-in
write or in-turn shell tools. Publishing an instruction asset grants no tool,
filesystem, network, credential, shell, or approval authority; every later
execution still traverses normal scope, safety, and approval policy. Procedures
that used external-origin tools, explicit network/delete classes, delegated
execution, or durable external watchers remain candidates and require explicit
management. Manual, catalog-installed, bundled, pinned, workspace read-only,
and external Skills are never rewritten automatically.

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

Durable activations and terminal work-unit outcomes are also the canonical
source for Skill call, completion, fallback, and failure statistics. The
`.usage.json` sidecar remains an inventory-recency hint only. Legacy
`skill_metrics` rows may be displayed as historical data but must not silently
drive curator, degradation, archive, or ranking decisions after their writer is
retired. Removing a legacy middleware is complete only when every user-visible
stats consumer has migrated or is explicitly labeled historical.

The Skill curator runs only when a bounded comparable creation cohort or a
verified repair incident is ready. The creation gate requires three independent
verified successes for the same person, workspace and environment, plus up to
two relevant negative observations.
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
For CREATE, the curator proposes a package: a short canonical `SKILL.md` main
within the current delivery source budget plus optional non-empty resources
under `references/`. Package size remains bounded at 32 KiB. Promotion writes
the main and resources atomically and verifies their package hash. PATCH keeps
the active package resources immutable and may change only its declared main
sections; a reviewed future package update is required to change resources.
Curator authorization and activation use the same exact delivery calculation;
there is no extra source-length allowance. A PATCH against a currently full
main must remain full under the current resource manifest. A PATCH against an
already paged legacy main may proceed only when instruction-body bytes and
estimated tokens do not grow, resources are identical, and every unrelated
section remains byte-identical. Shrinking a legacy package or moving material
into resources is a separate reviewed package-compaction workflow, not a narrow
incident repair.
Only explicit `passed` creation observations qualify for automatic publication.
`not_applicable` evidence may still nominate an immutable candidate but requires
explicit management; an empty verification state does not nominate automatic
publication. Trusted, argument-redacted registry metadata is captured at tool
start and refreshed at completion with the actual execution boundary before it
is frozen into the curation digest. This includes call-specific network,
delete, delegated-execution, and dangerous classes, including an actual host
sandbox fallback or out-of-workspace target. Historical observations without
that metadata retain a local read-only publication boundary; historical web
search/extract evidence is not grandfathered past the network restriction.
Nested `batch_read` items carry the same metadata as ordinary calls.

`skill_fallback` accepts a closed defect taxonomy: `stale_precondition`,
`invalid_procedure`, `missing_failure_guard`, `verification_mismatch`, and
`schema_changed`. Unknown categories, including transient/provider,
environment, cancellation, and approval failures, cannot create a durable
guard or authorize repair. Automatic repair additionally requires a compatible
daemon-classified tool failure from the same active work unit; model text alone
never supplies that evidence. `failed_tool_call_id`, when supplied, must match
an actually failed call. When omitted, the most recent observed failure is used,
and successful diagnostic reads between the failure and fallback do not erase
the attribution. The compatibility map is deliberately category-specific: for
example, interface drift may support a schema/precondition repair, while a
generic not-found failure cannot support an unrelated verification repair.
A guard never records or executes an unverified replacement command. A later
exact match skips the Skill and uses ordinary planning. Repeated matches suspend
the task binding. Promoting a repair resolves guards for the replaced version.

When the ordinary planner subsequently completes that same work unit with
explicitly passed verification and no recovery-tool failure, the incident may
nominate one repair immediately. Automatic repair is limited to writable,
unpinned, `agent-created` Skills whose active version was created by the curator
and has the canonical section topology. Ineligible manual or noncanonical
Skills are skipped before any curator model call. The curator must declare one
to three changed level-two sections and include the incident's failed section
(a non-heading step identifier maps to Procedure); deterministic validation
rejects front-matter, topology, or undeclared-section drift before an immutable
PATCH candidate can replace the active version. Cohorts with no attributable
incident no longer rewrite an existing Skill merely to speculate about
performance.

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

## Diagnostics And Regression Contract

`selfmind doctor` obtains the provider-visible catalog from the same adapter
normalization and wire builder used by the real transport, preserving both
source and final wire names. It reports invalid provider-wire names and any
remaining provider-visible per-Skill tools; `--probe-models` sends the real
primary catalog through a bounded non-dispatching request. The remaining
generic wire catalog has a 48 KiB regression budget (roughly 12K estimated
tokens);
crossing it is an actionable cost warning, while an invalid wire name remains
a fatal contract error. Per-call telemetry reports consistent schema token
estimates, `dynamic_skill_tools`, and bounded candidate source/root samples.
The durable Skill presentation
section independently recomputes activation delivery hashes/byte counts,
resource receipts, candidate drift limits, and terminal-ref cleanup. Broken
delivery/resource receipts are fatal. Doctor prints location, expected and
observed values, likely cause, owning component, safe repair commands, and
verification commands. Exit status is 0 for no fatal finding, 1 for a fatal
finding/live probe failure, and 2 when Doctor cannot complete the diagnosis.
Existing external or read-only descriptions over the managed 1024-character /
4096-byte authoring ceiling remain usable but receive an exact owner-file
warning; newly managed writes fail validation. Historical contract-0
activations are reported but not rewritten.

Catalog diagnostics use distinct denominators: `registered_active`, `hidden`,
and `provider_visible`. A registered hidden compatibility schema is never
reported as provider-visible. Doctor findings are actionable contracts, not
yellow counters: each issue includes a stable code, severity, exact owning row
or file, expected and observed values, likely cause, a safe dry-run or repair
command when one exists, and a verification command. In particular, terminal
candidate-ref leaks point to the transactional prune command; oversized legacy
mains point to paging plus the package-compaction workflow; stale candidate
identity returns the structured `candidate_stale` code and tells the caller to
refresh `skills_list`. Static invariants that cannot be repaired safely at
runtime point to the owning component and focused Go test instead of suggesting
an unsafe database edit.

The hidden `skill:<name>` dispatch path is an explicit rollback compatibility
surface during its shadow window, not an accidental dead path. While retained,
it keeps direct-dispatch and `skill.activated` event coverage and has documented
removal criteria. It is removed only after telemetry proves there are no
callers. The obsolete metrics middleware/store/pruner lifecycle is independent:
it is retired as soon as every user-visible stats consumer uses durable
activations and work-unit outcomes, without waiting on rollback-channel usage.

Go invariant tests cover provider catalog names/count/cost, candidate
allocation, package identity, paging, immutable delivery (including an
idempotent retry after resource-manifest drift), compaction, candidate ref
lifetime, schema migration, and Doctor severity. The cassette-backed
`skill-lifecycle` eval suite exercises the production message path with Skills
present and asserts zero provider-visible dynamic Skill tools.

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
