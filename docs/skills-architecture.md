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
- `skill_view`: full `SKILL.md` or one linked file under `references/`,
  `templates/`, `scripts/`, or `assets/`.
- `/skill-name`: direct user invocation, bundle-first then skill.
- `skill_bundle`: groups multiple skills into one slash command.
- `skill_manage`: mutation and hot reload.
- `skill_catalog`: install and audit.

Legacy dynamic `skill:<name>` tool registrations are compatibility shims only.
They return an instruction-only message and must not execute code blocks from
`SKILL.md`.

## Context Budget

Direct skill invocation injects the chosen `SKILL.md` body plus linked-file
names. The body is capped with a UTF-8 safe byte limit. If the body is too
large, SelfMind appends an explicit truncation note and the agent should use
`skill_view(name, file_path)` for the necessary linked files.

Best practice for new skills:

- Keep `SKILL.md` lean and procedural.
- Move detailed examples, schemas, and large references into linked files.
- Reference linked files from `SKILL.md` with clear "when to read" guidance.
- Avoid duplicating the same content in both `SKILL.md` and references.

## Governance

Background review and curator automation should only modify writable
`agent-created` skills by default. Manual, catalog-installed, bundled, pinned,
workspace read-only, and external skills are protected unless the user
explicitly asks for a mutation through a writable copy.

All durable mutations must write tenant learning records through the shared
audit helpers. Do not add channel-specific or tool-specific history files.

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
