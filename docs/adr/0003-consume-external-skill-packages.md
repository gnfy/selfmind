---
status: accepted
---

# Consume external Skill packages through bounded discovery and attribution

SelfMind adopts externally authored Skill packages as instruction assets on the
existing `<name>/SKILL.md` contract. It does not delegate work to a second agent
runtime to obtain their behavior. Claude Code, Codex, and Hermes have already
converged on that directory contract and on a metadata-only catalog followed by
lazy body loading; SelfMind's `skills_list` and `skill_view` surfaces are the
same shape. The gap is therefore packaging, naming, and trust, not runtime, and
a second runtime would collide with the rule that a daemon failure must never
start a divergent second runtime.

## Discovery and Trust

External packages carry a distinct `external` provenance tier and are treated as
untrusted data below operator, user, and safety policy, matching how repository
instructions are already treated. The tier is a first-class field rather than a
property inferred at each call site.

Discovery is manifest-first: when a root declares a package manifest, only the
packages that manifest lists are discovered. A manifest is the sole author
signal available about publication intent, and the cost of ignoring it is
concrete — one observed third-party package ships thirty-five `SKILL.md` files
on disk while its manifest registers twenty-five, the difference being
`in-progress/` drafts and unpublished `misc/` entries. Feeding an author's
drafts to the model catalog spends the bounded catalog budget on material the
author withheld.

Roots without a manifest are scanned recursively to a fixed depth of four with a
fixed exclusion set. Recursion replaces single-level enumeration, which cannot
see the two-level `<category>/<name>/SKILL.md` layout that external packages
actually use and therefore renders whole packages invisible. The exclusion set
covers dependency, virtual-environment, VCS, and cache directories, and it also
excludes a `SKILL.md` sitting inside `references/`, `templates/`, `assets/`, or
`scripts/` directly beneath a directory that already contains a `SKILL.md`:
those are progressive-disclosure resources, not separate Skills. Depth and
exclusions are constants, not configuration, so every scanning site agrees and
so a root pointed at a home directory cannot trigger an unbounded walk.

`~/.agents/skills` becomes a user-scope read-only root at precedence 105, after
the writable control-tenant user root at 100 and before the legacy person root
at 110. It is the cross-vendor Agent Skills convention rather than a
vendor-specific location, so adopting it lets a Skill the person already
installed for another agent work here with no further action. The writable user
root keeps higher precedence because it is where `/skills install` writes, and a
default root silently shadowing an explicit install would be unexplainable.
Vendor-private cache directories stay the user's choice through
`SELFMIND_SKILLS_ROOTS`; generic discovery code gains no vendor branch.

Package resources remain readable through `skill_view` under the package hash,
and scripts inside a package remain ordinary files subject to normal tools,
scope, and approval. The resource manifest carries an explicit exclusion list,
because a vendored dependency tree inside the package identity would make every
upstream change register as package drift, and repeated drift fails closed.
Agent definitions shipped inside an external package are ignored: importing them
would let an untrusted asset declare execution authority. A package's
model-invocation opt-out is honored through a native front-matter key with the
external key accepted as an alias, replacing today's silent discard of unknown
keys, which lets a Skill its author marked user-only be model-selected.

## Names

Qualified names take the form `source:name`, preferring the package or source
name and falling back to a scope label when a bare directory root supplies no
manifest name. This is the form Claude Code and Hermes already use, so it costs
the person nothing to learn. A relative path is not used as identity because it
moves whenever a category is renamed.

A bare short name resolves when it is unambiguous. When two enabled Skills share
it, resolution fails and lists the qualified candidates. Ambiguity is never
settled by comparing descriptions: that would be an ingress classifier, and the
descriptions of external packages are untrusted text whose authors would then
influence routing. Today's silent first-root-wins behavior is the defect being
corrected.

## Nesting

The rule that one execution lane activates at most one Skill per work unit does
not change. An imperative redirect whose body instructs the model to select
another Skill is not supported; the target is installed directly instead. Two
real activations would break attribution, because incident and recovery evidence
binds to an exact version and the automatic repair thresholds are computed from
snapshots attached to that version.

A declarative redirect resolved in the catalog remains accepted but is not
implemented, and the mechanism this decision first named does not survive
contact: the bundle path records no activation at all, so a redirect modeled as
a degenerate bundle would produce no delivery receipt, no frozen identity, and
no attribution. The workable shape is catalog-level alias resolution feeding the
ordinary single-Skill activation path. It is not built because nothing needs it:
a declarative key is SelfMind-native, so no external package uses it, and the
repository has no entry-point Skill of its own. The first Skill that genuinely
wants to be a thin entry point is the trigger to build it.

## Implicit Use Is Attribution, Not Activation

Recognizing implicit Skill use is adopted, but it is recorded as attribution.
Codex, whose design this follows, injects no bytes on an implicit hit: it
de-duplicates, counts usage, notifies contributors, and emits telemetry. The
body was already in context because the model read it with ordinary tools. In
SelfMind that ordinary read is `skill_view`, so the loading path already exists
and only the accounting is missing.

Detection lives in the tool-dispatch layer and matches the path arguments of
tool calls against discovered package directories. Codex parses shell command
text because its tool is a shell; SelfMind's reads are structured tool calls
whose paths are directly available, so the runner and extension allow-lists that
exist only to recover semantics from a command string are unnecessary.
Path-based matching also means name collisions are irrelevant to implicit use.

Attribution does not consume the one-Skill-per-work-unit budget, does not freeze
a delivery receipt, and does not draw on the Skill context budget. Because of
this, `skill_view` continues not to count as activation and that contract line
stands unchanged; attribution is recorded beside it. De-duplication is per work
unit, which is the granularity that outcome, verification, activation, and
evidence already use. When a Skill is activated in a work unit, attribution for
that same Skill is suppressed, since reading its own `references/` is that
activation's progressive disclosure and is already recorded in its resource
manifest; another Skill viewed in the same work unit is genuine attribution and
is recorded.

Attribution records persist in the control store as their own records, not mixed
into activations. A sidecar usage file cannot be written for a read-only root,
which is where most attribution occurs, and separate storage keeps the evidence
boundary structural rather than a field convention. They survive terminal
work-unit cleanup, because selection references are expiring authorization while
attribution is historical fact, and because a full-history explicit column is
not comparable to a recent-window implicit one.

Attribution is not admissible evidence for curator cohorts or repair thresholds.
It freezes no version, package hash, or resource manifest, and those thresholds
are defined over immutable snapshots attached to an exact version. It updates
usage recency and local completion ordering but not catalog ranking, which stays
metadata-only and reproducible so that usage heat cannot become a path for an
external package to rank itself up. Channels do not surface attribution;
`/skills stats` reports explicit activations and implicit attributions as
separate columns, so a high usage count that never produces a curator candidate
remains explicable.

## Explicit Invocation

A `$` completion is added in the TUI only. Selecting a candidate sends a
structured reference carrying the resolved path while the interface displays the
short name, so ambiguity cannot arise for completion-originated input and the
ambiguity rule above applies only to typed text. IM does not parse `$` in text,
where input is least structured and no completion affordance exists. The popup
reuses the existing metadata-only ranker; being local UI it consumes no model
budget and may therefore list every installed Skill, covering Skills the bounded
catalog omits.

## Consequences

Delivery is sequenced by `docs/plans/external-skill-packages.md`, which holds
the active plan slot and owns the batch order and its evidence. Two consequences
of these decisions belong here rather than to any batch.

A selection made in the `$` completion writes an existing `/<reference>` slash
invocation rather than a new protocol field. The reference is the qualified name
when it is unique, and the discovery path when two roots share both scope and
source; resolution accepts either, and a path is always unique. A slash command
is tokenized on whitespace, so a path containing whitespace cannot be carried
this way; that case writes the qualified name instead and the person gets the
ambiguity refusal, which lists the paths, rather than a silently wrong
resolution.

`/skills list`, `/skill <name>`, and `/skills stats` are TUI-local surfaces. The
eval harness reaches the gateway command surface, which rejects them as unknown
commands, so a case written against any of the three would be invalid evidence
rather than a message-path test. Their rendering and resolution are covered by
Go tests at the same behavioral boundary: the listing label, the ambiguity
refusal and its typeable candidates, and slash resolution of bare, qualified,
and path forms.

Nothing here widens execution authority. External packages remain instruction
assets that are never rewritten automatically, curated Skills continue to
prohibit executable resources, and the safety hard floor is untouched.
