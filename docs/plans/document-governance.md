# Documentation and Release-Gate Governance

## Purpose

Make repository guidance trustworthy before expanding `selfcheck`. Agents and
human reviewers need one current status, one active plan, explicit document
lifecycle, synchronized public language pairs, and a release gate that rejects
documentation drift deterministically.

## Scope

1. Reduce `AGENTS.md` to cross-cutting invariants below 20 KiB.
2. Reduce `docs/STATUS.md` to current capability, limitations, and priority
   state below 300 lines.
3. Register every Markdown document in `docs/manifest.yaml` as a contract,
   reference, guide, status, plan, decision, or archive.
4. Generate `docs/README.md` from that registry.
5. Add `selfmind docs check` and run it in every `selfmind selfcheck` profile.
6. Track canonical-source hashes for public English/Chinese pairs.
7. Make the clean-checkout CI job run the same documentation contract.

## Non-Goals

- Deciding whether a feature is complete from source text.
- Rewriting every domain design document in this iteration.
- Treating line-count equality as translation correctness.
- Adding SaaS, Runner, or code-size refactors to documentation work.

## Acceptance

- `selfmind docs check` passes on a clean checkout and reports actionable file
  errors for invalid UTF-8, missing registry entries, broken local links, stale
  translations, expired plans, multiple active plans, and size violations.
- `selfmind selfcheck --skip-go --skip-eval` performs a docs-only gate.
- The full local selfcheck and CI profile pass.
- English and Chinese command references include the new docs commands.
- npm packaging and the installed WSL binary expose the updated help.

## Exit Verdict

When all acceptance items pass, mark this document archived in the manifest,
remove it from the active-plan line in `docs/STATUS.md`, and record the next
priority as ordinary status work rather than opening another plan by default.
