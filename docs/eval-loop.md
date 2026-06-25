# SelfMind Eval Loop

SelfMind uses a local-first eval loop to test agent behavior without needing a
large production dataset. Eval cases run through the same gateway, task, tool,
stream, and model-provider path as normal CLI or IM requests.

## Goals

- Catch regressions in provider compatibility, tool calling, workspace
  resolution, continuation, output formatting, and long-task behavior.
- Keep fixtures small and representative of real daily development work.
- Store structured JSONL run logs locally for comparison and failure triage.

## Commands

```bash
selfmind eval list [path]
selfmind eval run evalcases/daily-dev/chat_basic.yaml
selfmind eval run --suite daily-dev --provider kimi-coding --model kimi-for-coding
selfmind eval report evalruns/2026-06-25
```

`eval run` writes JSONL logs under `evalruns/YYYY-MM-DD/` by default. The
directory is intentionally ignored by git.

## Case Format

```yaml
id: codebase_analysis_001
title: "inspect the current codebase"
suite: daily-dev
workspace: "."
channel: cli

turns:
  - input: "分析一下当前 selfmind 项目的代码结构。"

expect:
  status: completed
  require_tool_events: true
  min_tool_calls: 1
  max_tool_calls: 8
  max_tool_errors: 3
  max_duration_seconds: 300
  contains:
    - "selfmind"
  must_not_contain:
    - "<tool>"

checks:
  no_mojibake: true
  no_raw_json_leak: true
  no_tool_xml_leak: true
  no_empty_response: true
  no_provider_stack_dump: true
  tool_failure_should_recover: true
  workspace_should_match: true
  context_not_exceeded: true
```

Use multi-turn cases to test `continue`, `可以`, and `按方案做` behavior:

```yaml
turns:
  - input: "先给方案，不要改代码。"
  - input: "可以，继续。"

expect:
  require_same_task: true
  require_continuation: true
```

## Architecture

- `internal/eval/case.go` parses YAML fixtures.
- `internal/eval/runner.go` builds the normal SelfMind runtime and calls
  `httpapi.Server.ProcessMessage`.
- `internal/eval/recorder.go` observes `llm.StreamEvent` through
  `httpapi.WithStreamObserver` and writes JSONL.
- `internal/eval/checks.go` runs deterministic quality checks.
- `internal/eval/report.go` summarizes JSONL logs.
- `internal/cliapp/eval_commands.go` exposes the CLI.

Eval must not create a parallel agent implementation. It should exercise the
same path a user sees in CLI or IM. When adding new product behavior, add or
update eval cases first, then keep implementation fixes grounded in failed
events.

## P0/P1 Coverage

Use `evalcases/p0-p1-5/` as the small regression suite for the current
agent-first contract:

- direct identity/model questions should complete without tool calls;
- small code snippets should produce clean, readable output;
- short approval/continuation turns should reuse the active task;
- codebase inspection should emit visible tool progress;
- command or environment diagnosis should first identify the project ecosystem,
  manifests, scripts, CI files, runtime versions, and workspace boundaries
  before recommending any command or override. Fixtures should cover this as a
  language-agnostic behavior, not as a Go-only workaround.

`eval report` now aggregates first-token latency and tool call/error totals so
streaming responsiveness and tool visibility regressions are easier to spot.

## Failure Categories

The runner currently classifies common failures as:

- `provider_auth`
- `provider_transport`
- `tool_schema`
- `context_overflow`
- `workspace_scope`
- `command_failed`
- `timeout`
- `unknown`

Prefer adding precise categories over broad string matching when a new recurring
failure appears.

## Privacy

Eval logs are local by default. They record content previews and hashes. Full
assistant output is stored only when a case or command enables
`record_content`.
