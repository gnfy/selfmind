---
name: release-checklist
description: Prepare a small project release with build, tests, docs, and artifact checks.
---

# Release Checklist Skill

Use this skill when the user wants to package, publish, or verify a release.

## Workflow

1. Confirm the release target, version, and platform.
2. Run the project test command.
3. Build the release artifact from a clean command.
4. Check README or release docs for stale install and usage instructions.
5. Inspect generated artifact names and checksums when available.

## Pitfalls

- Do not publish automatically unless the user explicitly asks.
- Do not update unrelated version strings.
- Prefer a single documented release entry point over duplicate build instructions.

## Verification

List the artifact path, build command, and test command.
