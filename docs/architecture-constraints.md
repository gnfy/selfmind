# SelfMind Architecture Constraints

This document is for maintainers and future AI coding tools. It defines the guardrails SelfMind must keep while evolving so the codebase does not drift back into oversized controllers, broad switches, global state, and one-off UI surfaces.

## Core Rules

- Keep `selfmind` as the single binary. Run the daemon as `selfmind gateway run`; do not add a separate daemon entrypoint binary.
- Gateway, TUI, IM, and webhook channels may share task/run/workspace/memory/skill state, but chat transcripts stay channel-local.
- Reuse the existing layers: `cliapp` owns command entrypoints, `gateway/httpapi` owns HTTP, `gateway/cli` owns TUI orchestration, `ui/components` owns reusable UI pieces, `app` owns dependency wiring, and `kernel` owns the agent loop and model calls.
- Before adding code, decide whether it belongs to product entrypoints, application wiring, agent core, tools, gateway, UI components, or platform config/storage. Do not put business logic into the nearest large file.

## Rules For Future AI Agents

- Do not keep growing `internal/gateway/cli/controller.go`. The controller should orchestrate state, route messages, and connect components.
- Do not implement new full-screen help/detail/list pages directly in the controller. Transient pages must use `internal/ui/components/Pager` or a similar reusable surface component.
- Do not scatter slash command metadata across help text, editor hints, and dispatch logic. New commands should move toward a shared command registry.
- Do not make `internal/ui/components/editor.go` know full business command behavior. The editor should render externally supplied hints and lightweight input state.
- Do not add cross-tenant or cross-test global mutable state. Objects that must be process-wide need clear lifecycle ownership and should be injected by `app` or the gateway runner.
- Do not make `kernel` depend on `gateway`, `server`, or concrete tool implementations. The agent talks to tools through abstract backends.
- File, terminal, patch, and process tools must keep workspace scope enforcement.
- Do not mirror CLI chats into IM or IM chats into CLI automatically. Shared cross-channel state is task state and events, not raw chat text.

## TUI Constraints

- Transcript rendering, composer, status line, and transient pages should be components. The controller should not manage every visual detail.
- `/help` and similar temporary pages do not enter chat history and should restore the previous transcript after closing.
- Multi-line input must remain in normal layout flow and must not float over transcript history.
- Composer height must have a cap; extra lines scroll inside the composer.
- Colors, spacing, and selection styles should come from `internal/ui/common` style tokens first.

## Gateway And Channel Constraints

- Gateway control commands such as status, tasks, stop, workspace, and resume should remain model-free whenever possible.
- IM adapters only verify, parse, normalize, and send platform payloads. Identity, workspace lookup, task/run state, and dispatch belong to the gateway.
- `person_id` identifies the same human. `account_id` identifies a bound platform account.
- Keep the per-person active-run guard until a real worker pool exists.
- Gateway agent events must be per-run context state via `kernel.WithEventChannel`; do not swap the shared `Agent.EventChannel` in gateway code.
- User-visible progress should come from structured run outcomes, handoffs, and task events, not one-off prose parsing in channel adapters.
- Approval state belongs in `control.approval_requests` and gateway control/API handlers. IM adapters may render buttons or parse callbacks, but must not own approval lifecycle state.

## Model And Tool Constraints

- Model selection goes through role-based routing such as `coding_agent`, `memory_extract`, `background_review`, `skill_curator`, and `semantic_recall`.
- Provider discovery, credential resolution, live model listing, and provider profile overrides belong in `internal/modelruntime`. Do not add new vendor credential probing or model-list fetch logic directly to `internal/app` or LLM adapters.
- P2 external auth reuse is currently limited to Codex CLI, Claude Code, Gemini CLI, and Qwen CLI. Other vendors should use API keys, custom OpenAI-compatible endpoints, or `provider_profiles`.
- New provider adapters should not be packed into one large file. Split protocol handling, providers, model listing, and streaming behavior.
- Prefer provider-native tool calls. Text `[TOOL:...]` remains only a compatibility fallback.
- Clearly read-only tools may run in parallel. File writes, patches, terminals, memory/skill mutations, process control, and unknown tools run sequentially by default.
- Skill handling must remain progressive and layered: `skills_list` for metadata, `skill_view` for full content/files, `skill_manage` for mutation, `skill_catalog` for install/audit, and `skill_bundle` for bundle CRUD.
- Skill mutations should hot-reload the active registry when possible. Direct slash invocation resolves bundles first, then individual skills.
- Curator automation must only govern `agent-created` skills by default. Manual, catalog-installed, bundled, or pinned skills must not be auto-archived.
- Catalog-installed skills must have durable install provenance in `skills/.catalog/lock.json`. Install should reject same-name directory and legacy `.md` collisions by default; `--force` must back up the previous copy under `skills/.catalog/backups/` before replacement.
- Memory and skill mutations should use the shared learning audit log. Do not add scattered history files or channel-specific learning records.
- User-facing learning history and undo should go through `skill_manage` and `memory` tool actions, not through duplicate TUI/IM-only code paths.

## File Size Guardrails

These are not compile-time limits, but future AI agents should actively avoid crossing them:

- If a file exceeds 800 lines, prefer extracting a component, handler, service, or adapter.
- If a function exceeds 120 lines, prefer splitting it into smaller functions or component methods.
- If a switch exceeds 12 branches, prefer a registry, handler map, or strategy object.
- If a feature requires editing more than three unrelated large files, introduce a clearer application service or component interface first.

## Where Status Lives

This file is rules only. Implementation state and the live priority list are in
`docs/STATUS.md`; the product north star and continuity contract are in
`docs/identity-continuity.md`. Do not add per-feature status notes here.
