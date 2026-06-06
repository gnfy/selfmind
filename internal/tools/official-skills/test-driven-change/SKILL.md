---
name: test-driven-change
description: Make a code change by first pinning expected behavior with focused tests.
---

# Test Driven Change Skill

Use this skill when the requested change touches behavior that can be verified locally.

## Workflow

1. Find the closest existing test style.
2. Add or update the smallest test that captures the requested behavior.
3. Run the focused test and confirm it fails for the expected reason when practical.
4. Implement the minimal production change.
5. Re-run the focused test, then broaden to package or project tests if risk is higher.

## Pitfalls

- Do not add broad snapshot tests when a specific assertion would explain the behavior better.
- Do not skip existing helper APIs in the codebase.
- Do not hide an implementation bug by weakening the test.

## Verification

Report the exact test command and whether broader tests were run.
