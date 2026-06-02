# SelfMind Gateway / IM / SaaS Architecture

This document records the current architecture direction so future development
can continue without rediscovering the same decisions.

## Goal

SelfMind should first work as a personal always-on gateway, then evolve into a
SaaS system without a rewrite.

Personal mode:

- SelfMind runs on one long-lived computer.
- CLI can code locally.
- WeChat, Feishu, QQ, or Web clients can ask for progress or command work.
- All devices recognize the same human after account binding.

SaaS mode:

- One SelfMind service hosts many users.
- Each user has independent workspaces, memory, skills, tasks, and model
  policies.
- Platform accounts are bound to persons, not to raw chat sessions.

## Non-Negotiable Product Boundary

Chats are not mirrored across channels.

- CLI chat stays in CLI.
- IM chat stays in that IM channel.
- Web chat stays in Web.

The shared layer is durable work state:

- task status;
- task runs;
- task events;
- handoff summaries;
- approvals and notifications;
- memory;
- skills.

This keeps privacy and UX sane: a user can ask "how is the task going?" from
IM without the whole CLI conversation being pushed into the IM channel.

## Identity Model

```text
tenant_id
  SaaS tenant, or "default" in personal mode.

person_id
  The same human across CLI, IM, and Web.

account_id
  A bound platform account, for example:
  - cli/local
  - feishu/ou_xxx
  - wechat/openid_xxx
  - qq/user_xxx

workspace_id
  A project/work directory owned by a person.

task_id
  Durable task state shared across channels.

run_id
  One agent execution attempt.

event_id
  Auditable task state transition.
```

Implemented in:

- `internal/control/store.go`

Current storage file:

- `control.db`

## Gateway Control Plane

The gateway is the first real control-plane entrypoint. The user-facing entry is
`selfmind gateway`; `selfmindd` remains only as a hidden compatibility wrapper.

Implemented in:

- `cmd/selfmind/main.go`
- `internal/cliapp`
- `internal/gateway/api`
- `internal/gateway/httpapi`
- `internal/runtime/gateway`
- `cmd/selfmindd/main.go` (compatibility wrapper)

Core endpoints:

| Method | Path | Purpose |
|--------|------|---------|
| `GET` | `/health` | Liveness check |
| `POST` | `/v1/message` | Unified CLI/IM/Web message entrypoint |
| `POST` | `/v1/im/{platform}` | Generic IM webhook normalization |
| `POST` | `/v1/accounts/bind` | Bind platform account to an existing `person_id` |
| `GET` | `/v1/tasks` | List tasks for an account |
| `GET` | `/v1/tasks/current` | Get current task, latest handoff, and structured `active_run` |
| `POST` | `/v1/workspaces/register` | Register a local workspace |
| `GET` | `/v1/workspaces` | List workspaces |
| `GET` | `/v1/gateway/status` | Inspect gateway process state and active runs |
| `POST` | `/v1/gateway/shutdown` | Request graceful gateway shutdown |

Security:

- `SELF_GATEWAY_TOKEN` enables local API token auth.
- `SELF_DAEMON_TOKEN` remains compatible; `SELF_GATEWAY_TOKEN` has priority.
- Clients send either `Authorization: Bearer <token>` or `X-SelfMind-Token`.

Daily-use control commands handled before the agent:

- `/id`: show tenant/person/account binding details.
- `/status`: show current task, active run, elapsed time, and latest handoff.
- `/stop`: cancel the active run for this person.
- `/new <title>`: create a fresh current task.
- `/tasks`: list recent tasks.
- `/resume <task_id>` or `/task <task_id>`: switch current task.
- `/workspaces`: list registered workspaces.
- `/workspace <workspace_id>`: switch current workspace.

The gateway keeps an in-memory active-run guard per `person_id`, so one user
does not accidentally start two coding runs against the same shared agent state.
The actual agent call is serialized inside the gateway for the current personal
version. Future SaaS work should replace this with per-person/per-workspace
agent workers.

## IM Integration Pattern

Follow Hermes' shape, but keep the first SelfMind version smaller:

```text
platform adapter
  parse/authenticate platform payload
  normalize inbound message
  send outbound message if supported
        |
        v
gateway
  resolve tenant/person/account
  resolve workspace/task
  record channel-local message
  create run/event
  install workspace scope
  dispatch agent
  save handoff/status
```

Platform adapters should not own business state. They should be thin:

- Feishu adapter parses Feishu event JSON and URL challenge.
- WeChat adapter parses callback payload and signature.
- QQ adapter parses QQ payload.
- Generic webhook adapter accepts already-normalized JSON.

Current first version:

- `/v1/im/{platform}` accepts generic JSON.
- Feishu-like `event.sender.sender_id.open_id` and
  `event.message.content` are normalized.
- Common WeChat/WeCom relay fields such as `FromUserName`, `ToUserName`, and
  `Content` are normalized.
- Payloads can include `"async": true`; `SELF_IM_ASYNC=1` makes non-control IM
  messages async by default. This avoids long webhook timeouts.

Next production step:

- add per-platform signature validation;
- add outbound senders;
- add delivery retry and message-length splitting;
- add queue/busy behavior for active task sessions;
- add button-based approval for IM.

## CLI Integration

The existing TUI remains available. A thin gateway client has been added for
multi-device workflows.

Implemented in:

- `cmd/selfmind/main.go` as the thin entrypoint
- `internal/cliapp` for gateway lifecycle and client commands

Modes:

```powershell
selfmind gateway start
selfmind gateway status
selfmind gateway stop
selfmind gateway restart
selfmind send "task status"
selfmind send --async "run the test suite and fix failures"
selfmind status
selfmind stop
selfmind tasks
selfmind workspaces
selfmind workspace add .
selfmind workspace use <workspace_id>
$env:SELF_USE_GATEWAY="1"; selfmind
```

Environment:

- `SELF_GATEWAY_URL`: default `http://127.0.0.1:8765`
- `SELF_GATEWAY_TOKEN`: optional gateway token
- `SELF_DAEMON_URL` / `SELF_DAEMON_TOKEN`: compatibility aliases
- `SELF_TENANT_ID`: tenant override
- `SELF_PLATFORM_USER_ID`: local account id override
- `SELF_WORKSPACE_ID`: force workspace for this message
- `SELF_TASK_ID`: force task for this message

## Workspace Execution Scope

Any gateway-triggered coding task must run inside the active workspace.

Implemented in:

- `internal/tools/workspace_scope.go`
- registered in `internal/app/tools.go`

Behavior:

- `terminal` defaults `cwd` to the workspace root.
- `read_file`, `write_file`, `search_files`, and `ls_r` resolve relative
  paths under the workspace root.
- `patch` rewrites patch file paths into the workspace root.
- paths outside `allowed_roots` are rejected.

When adding a new file/process/path tool, update `WorkspaceScopeMiddleware`.

## Model Routing

Different users, tasks, and background jobs should be able to use different
models or AI gateways.

Implemented in:

- `internal/kernel/llm/model_gateway.go`
- `internal/platform/config/loader.go`
- wired from `internal/app/agent.go`

Stable role names:

- `coding_agent`
- `memory_extract`
- `background_review`
- `skill_curator`
- `semantic_recall`

Current config shape:

```yaml
models:
  roles:
    coding_agent:
      provider: "anthropic"
      model: "claude-sonnet-4-7"
    background_review:
      provider: "gemini"
      model: "gemini-1.5-flash"
```

Future SaaS direction:

- resolve model policy by tenant/person/workspace/task role;
- store user-provided provider keys separately;
- route cheap background work to cheaper models;
- keep coding work on stronger models.

## Learning Loop

SelfMind's differentiator should remain "gets smarter with use".

Existing direction:

- user preferences and stable facts go to memory;
- reusable workflows go to skills;
- temporary task progress goes to task/handoff, not memory;
- outdated skills should be patched instead of duplicated;
- background review can use cheaper role-routed models.

Important files:

- `internal/kernel/background_review.go`
- `internal/kernel/prompt_guidance.go`
- `internal/tools/skill_manage.go`
- `internal/tools/session_search.go`

## Current Implementation Status

Done:

- role-based model gateway foundation;
- single-binary `selfmind gateway` lifecycle commands;
- hidden `selfmindd` compatibility wrapper;
- control-plane SQLite schema;
- task/run/event/handoff persistence;
- `/v1/message`;
- `/v1/im/{platform}`;
- `/v1/accounts/bind`;
- thin CLI gateway client with `send/status/stop/tasks/workspaces/workspace add`;
- async gateway messages for IM/CLI;
- pre-agent control commands;
- per-person active-run guard and `/stop` cancellation;
- tenant-aware `session_search`;
- workspace execution scope;
- gateway token auth with daemon-token compatibility;
- docs and tests for the above.

Still first-version / not production complete:

- no official Feishu/WeChat/QQ SDK adapter yet;
- no outbound IM sender implementation yet;
- no durable message queue or Hermes-style steer mode yet;
- no IM-native button approval yet;
- no SaaS admin UI or per-user model key storage yet;
- agent memory tenant currently maps to `person_id`; later SaaS should separate
  tenant/person cleanly while preserving data compatibility.

## Test Command

```powershell
$env:GOWORK='off'
go test ./...
```

In this local workspace, Go may be available at:

```powershell
$env:PATH="$env:USERPROFILE\.cache\selfmind-tools\go1.26.3\go\bin;" + $env:PATH
```
