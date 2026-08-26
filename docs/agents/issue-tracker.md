# Issue Tracker: GitHub

Issues and specifications for this repository live in GitHub Issues under
`gnfy/selfmind`. Use the `gh` CLI for tracker operations.

## Conventions

- Create an issue with `gh issue create`.
- Read an issue and its discussion with `gh issue view <number> --comments`.
- List issues with `gh issue list`, using structured JSON output when filtering
  by labels or state.
- Comment with `gh issue comment <number>`.
- Apply or remove labels with `gh issue edit`.
- Close an issue with `gh issue close`.
- Infer the repository from the current Git remote; do not embed credentials or
  duplicate repository identity in command arguments unnecessarily.

## Pull Requests as a Request Surface

PRs as a request surface: no.

External pull requests do not enter the issue-triage queue unless this setting
is explicitly changed later.

## Skill Operations

When a Skill says "publish to the issue tracker," create a GitHub issue.

When a Skill says "fetch the relevant ticket," read the GitHub issue, including
its comments and labels.

When a Skill publishes an implementation-ready specification, apply the label
mapped to `ready-for-agent` in `docs/agents/triage-labels.md`.

## Wayfinding

A wayfinding map is one GitHub issue labelled `wayfinder:map`.

Child tickets should use `wayfinder:research`, `wayfinder:prototype`,
`wayfinder:grilling`, or `wayfinder:task` and should be represented as native
sub-issues when available. If native sub-issues are unavailable, link them from
a task list in the map and add `Part of #<map>` to each child.

Use native issue dependencies for blocking relationships when available.
Otherwise record `Blocked by: #<issue>` near the top of the child issue.

A ticket is available when it is open, unassigned, and has no open blocker.
Claim it by assigning it to the current developer before starting work.
