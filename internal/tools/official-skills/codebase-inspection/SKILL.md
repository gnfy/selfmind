---
name: codebase-inspection
description: Inspect an unfamiliar codebase before making a change.
---

# Codebase Inspection Skill

Use this skill when the user asks you to understand, explain, or modify an unfamiliar project.

## Workflow

1. Identify the project layout with fast file search before opening files.
2. Read the smallest set of entry points, configuration files, and tests that explain the requested behavior.
3. State the likely ownership boundaries before editing.
4. Prefer existing patterns over introducing new abstractions.
5. Verify with the narrowest useful test or build command.

## Pitfalls

- Do not refactor unrelated code while inspecting.
- Do not assume generated or dirty files are yours to revert.
- If a fact may be project-specific, save it to memory only when stable.

## Verification

Summarize the files inspected, the behavior learned, and the command used for verification.
