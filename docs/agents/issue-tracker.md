# Issue Tracker: GitHub

Issues and specifications for this repository live in GitHub Issues under
`gnfy/selfmind`. Use the `gh` CLI for tracker operations. `gh` infers the
repository from the current Git remote, so do not embed credentials or duplicate
repository identity in command arguments.

## Conventions

- Create an issue with `gh issue create --title "..." --body "..."`, using a
  heredoc for a multi-line body.
- Read an issue and its discussion with `gh issue view <number> --comments`.
- Comment with `gh issue comment <number> --body "..."`.
- Apply or remove labels with `gh issue edit <number> --add-label "..."` or
  `gh issue edit <number> --remove-label "..."`.
- Close an issue with `gh issue close <number> --comment "..."`.

List issues with structured output when filtering by label or state:

```
gh issue list --state open \
  --json number,title,body,labels,comments \
  --jq '[.[] | {number, title, body, labels: [.labels[].name],
        comments: [.comments[].body]}]'
```

GitHub shares one number space between issues and pull requests, so a bare `#42`
may be either. Resolve it with `gh pr view 42` and fall back to
`gh issue view 42`.

## Pull Requests as a Request Surface

PRs as a request surface: no.

External pull requests do not enter the issue-triage queue. Set this flag to
`yes` only if this repository starts treating external pull requests as feature
requests; the triage Skill reads it.

While the flag is `yes`, pull requests carry the same labels and states as
issues through the `gh pr` equivalents:

- Read a pull request with `gh pr view <number> --comments`, and its diff with
  `gh pr diff <number>`.
- List candidate external pull requests with `gh pr list --state open --json
  number,title,body,labels,author,authorAssociation,comments`, then keep only
  an `authorAssociation` of `CONTRIBUTOR`, `FIRST_TIME_CONTRIBUTOR`, or `NONE`.
- Comment, label, and close with `gh pr comment`, `gh pr edit --add-label` or
  `--remove-label`, and `gh pr close`.

## Skill Operations

When a Skill says "publish to the issue tracker," create a GitHub issue.

When a Skill says "fetch the relevant ticket," run
`gh issue view <number> --comments` and read its comments and labels.

When a Skill publishes an implementation-ready specification, apply the label
mapped to `ready-for-agent` in `docs/agents/triage-labels.md`.

## Wayfinding

A wayfinding map is one GitHub issue labelled `wayfinder:map`, holding the
Notes, Decisions-so-far, and Fog body. Create it with
`gh issue create --label wayfinder:map`.

A child ticket is an issue linked to the map as a native GitHub sub-issue
through `gh api` on the sub-issues endpoint, labelled `wayfinder:research`,
`wayfinder:prototype`, `wayfinder:grilling`, or `wayfinder:task`. Where native
sub-issues are unavailable, add the child to a task list in the map body and put
`Part of #<map>` at the top of the child body.

Blocking uses native GitHub issue dependencies, which are the canonical,
UI-visible representation:

```
gh api --method POST \
  repos/<owner>/<repo>/issues/<child>/dependencies/blocked_by \
  -F issue_id=<blocker-database-id>
```

`<blocker-database-id>` is the blocker's numeric database id, read with
`gh api repos/<owner>/<repo>/issues/<n> --jq .id`. It is neither the `#number`
nor the `node_id`, and passing either one fails. GitHub then reports open
blockers in `issue_dependencies_summary.blocked_by`, which is the live gate.
Where dependencies are unavailable, fall back to a `Blocked by: #<n>, #<n>` line
at the top of the child body. A ticket is unblocked when every blocker is
closed.

The frontier query lists the map's open children, scoped to its sub-issues or
task list, then drops every child that has an open blocker
(`issue_dependencies_summary.blocked_by > 0`, or an open issue named in the
`Blocked by` line) or an assignee. First in map order wins.

Claim a ticket with `gh issue edit <n> --add-assignee @me` as the session's
first write. Resolve it with `gh issue comment <n> --body "<answer>"`, then
`gh issue close <n>`, then append a context pointer to the map's
Decisions-so-far.
