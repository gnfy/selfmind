# Domain Documentation

This repository uses a single-context domain documentation layout.

## Before Exploring

Read these resources when they exist and are relevant:

- `CONTEXT.md` at the repository root for canonical domain terminology.
- ADRs under `docs/adr/` for architectural decisions affecting the work.
- The domain document referenced by `AGENTS.md`.

If `CONTEXT.md` or `docs/adr/` does not exist, proceed silently. Create them
lazily only when domain terminology or a durable architectural decision has
actually been resolved.

## Layout

The intended layout is:

    /
    ├── CONTEXT.md
    └── docs/
        └── adr/
            ├── 0001-example-decision.md
            └── ...

`CONTEXT.md` is a glossary, not a specification, implementation guide, status
report, or scratch pad.

System-wide architectural decisions belong under `docs/adr/`. Create an ADR
only when the decision is hard to reverse, surprising without context, and
represents a genuine trade-off.

## Vocabulary

Use canonical terms from `CONTEXT.md` in specifications, issue titles, tests,
and implementation discussions.

When a required concept is absent, determine whether existing project language
already covers it before introducing a new term. Record a new term only after
its meaning and boundaries have been resolved.

## ADR Conflicts

If proposed work contradicts an existing ADR, surface the conflict explicitly.
Do not silently override or reinterpret the existing decision.
