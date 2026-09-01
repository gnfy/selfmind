# External Skill Package Adoption

## Purpose

Carry [`docs/adr/0003-consume-external-skill-packages.md`](../adr/0003-consume-external-skill-packages.md)
to completion. Bounded recursive discovery, manifest precedence, the
cross-vendor user root, qualified names, and ambiguity refusal have landed. The
remaining decisions close the package contract, record implicit Skill use as
attribution, and add the `$` completion. The plan delivers all four batches in
dependency order; the order is not interchangeable.

## Ownership

- Owner: SelfMind project owner
- Approver: project owner
- Review date: 2026-09-25
- Status: paused while
  [`agent-execution-recovery.md`](agent-execution-recovery.md) holds the active
  slot. The review date stands; no scope item is withdrawn.

## Batch 1: Close the package contract

1. **`external` as a first-class provenance field.** `SkillRoot` and `SkillInfo`
   carry `first-party` or `external`. External means a root that is neither
   SelfMind-managed nor repository-authored: `SELFMIND_SKILLS_ROOTS`,
   `SELFMIND_SKILLS_DIR`, and `~/.agents/skills`. Workspace roots keep their
   current classification, because repository-authored instructions are already
   governed as untrusted data and relabelling them would be a trust
   reclassification the decision does not authorize. `~/.agents/skills` keeps
   user scope, which describes ownership, and gains external provenance, which
   describes authorship; the qualified-name fallback prefers provenance, so a
   package from that root stops presenting itself as `user:`. Enforcement does
   not change today, because a read-only root already refuses mutation; the
   field replaces per-call-site inference with one place to read.
2. **Model-invocation opt-out.** `SkillDefinition` gains `model_invocation`, with
   the external `disable-model-invocation` key accepted as its inverted alias
   instead of being silently discarded. Only the two model surfaces filter on
   it. Explicit lookup and the slash forms stay available, which is the whole
   point of a user-only entry point.
3. **Unknown front-matter keys become visible.** Parsing still ignores a key it
   does not model, but Doctor reports the owning file and key so an
   author-declared constraint can no longer disappear without a trace.
4. **Resource-manifest exclusions.** The support-file walk and the install
   directory hash apply the same exclusion set discovery uses. The remaining
   exposure is a vendored tree nested inside a support directory, which would
   otherwise enter package identity and make every upstream change register as
   package drift.
5. **Agent definitions stay ignored.** Already true, because `agents/` is not an
   allowed support directory. A test pins it so widening the allowed set cannot
   quietly grant an untrusted asset a way to declare execution authority.

Verification: a package whose support directories contain no excluded
directory hashes identically before and after, proving the exclusion only
removes what never belonged; a package carrying `agents/openai.yaml` keeps that
file out of support files, the resource manifest, and package identity; a Skill
declaring the opt-out is absent from the model catalog and candidate ranking
while both slash forms still resolve it; a package from `~/.agents/skills`
reports external provenance and qualifies as `external:<name>`; Doctor names an
unknown front-matter key and its file.

## Batch 2: Move the mechanism into the contract document

`docs/skills-architecture.md` owns mechanism and must describe what now runs:
recursion depth and the exclusion set, the support-directory rule, manifest
precedence and its read-only-root limit, the one-level manifest lookup, the
updated root table with provenance, qualified names and the split between
precedence-winner resolution for reference-based lookups and ambiguity refusal
for typed ones, the listing label rule, and the model-invocation opt-out.
`docs/STATUS.md` records the capability change, because external packages moved
from wholly unusable to usable. ADR 0003 drops its severable framing for the
`$` completion, adopts these four batches, and records that `/skills stats` is
equally unreachable from the eval harness.

## Batch 3: Attribution

Detection begins by confirming, not assuming, the single tool-dispatch point
that observes tool name, arguments, and invocation scope together.

1. Path attribution matches tool-call path arguments against discovered package
   directories, using one package index built per work unit. Discovery walks the
   filesystem, so it must not run per tool call.
2. Attribution records live in their own control-store table, versioned,
   capability-inert for historical rows, with a backed-up and verified legacy
   database, a released-upgrade fixture test, and refusal of an unsupported
   newer schema.
3. De-duplication is per work unit, keyed by package path. Attribution for a
   Skill activated in the same work unit is suppressed, because reading its own
   resources is that activation's progressive disclosure.
4. Boundaries: invisible to curator cohorts and repair thresholds; updates usage
   recency and local completion ordering but not catalog ranking; not surfaced
   to channels; retained with the work unit rather than deleted with expiring
   selection references; reported by `/skills stats` as its own column.
5. Recency for a read-only root moves into the control store, since a sidecar
   cannot be written there. The listing's last-used value for an external Skill
   therefore changes source.

Verification: two distinct Skills viewed in one work unit yield two records and
one Skill viewed twice yields one; viewing an activated Skill's own resource
yields none; attribution succeeds for a Skill on a read-only root, which is the
reason for the storage choice; the upgrade fixture passes; a test proves cohort
and repair queries cannot observe attribution rows.

## Batch 4: `$` completion

1. Candidates come from the existing metadata-only ranker over the full
   inventory rather than the three-candidate model path. The popup is local UI
   and spends no model budget, so it can show Skills the bounded catalog omits.
   A colliding short name renders qualified, matching the listing rule.
2. A selection writes an existing `/<reference>` slash invocation: the qualified
   name when it is unique, and the discovery path when two roots share both
   scope and source. Batch 1 made both resolvable, so determinism needs no new
   protocol field. A slash command is tokenized on whitespace, so a path
   containing whitespace falls back to the qualified name and its ambiguity
   refusal rather than resolving to the wrong package.
3. IM gains no `$` parsing. A test pins that `$name` in an IM message stays
   ordinary text.
4. Rendering lives in a focused `internal/gateway/cli` module rather than the
   controller.

Verification: the popup offers more candidates than the bounded catalog
contains; a selection resolves to exactly one package with a colliding name
present; an IM message containing `$name` produces no Skill activation.

## Out of scope

Declarative redirect chains remain an accepted decision with no implementation
slot. Nothing needs them yet: the key would be SelfMind-native, so no external
package uses it, and no repository Skill is a thin entry point. Building one
would touch the most sensitive code in this area for a use case that does not
exist. The first Skill that genuinely wants to be an entry point is the trigger.
Imperative redirects stay unsupported and the target package is installed
directly. Cross-agent delegation, marketplace installation, and attribution as
repair evidence are excluded.

## Evidence and gates

`/skills list`, `/skill <name>`, and `/skills stats` are TUI-local surfaces the
eval harness cannot reach, so their rendering and resolution are covered by Go
tests at the same behavioral boundary. `selfmind selfcheck` runs before pushing;
batches touching the control store also test `internal/control` upgrade
fixtures.
