# Coding Agent Foundations

SelfMind is an agent-first work assistant. Coding behavior must remain useful
across languages, repositories, providers, CLI sessions, and IM channels. It
must not depend on Go-specific or operations-specific keyword rules.

## Project Discovery

At the start of a run, SelfMind builds a bounded, read-only `ProjectProfile`
from the workspace root:

- It inspects the root project or at most eight immediate child projects.
- It recognizes declared manifests and lockfiles without executing repository
  code.
- It suggests only verification commands supported by detected evidence.
- It treats every suggested command as a candidate. The agent must still read
  the relevant manifest or project instructions before running it.

The initial profile supports Go, Rust, Swift, Node.js, Python, PHP, JVM,
Ruby, CMake, and .NET projects. Adding an ecosystem belongs in
`internal/kernel/project_profile.go`; it must not add routing rules to the
gateway or special cases to the main agent loop.

## Coding Loop Contract

For an ordinary coding request, the agent should:

1. Resolve the active workspace and obey applicable `AGENTS.md` instructions.
2. Inspect the smallest useful project surface before editing.
3. Use a plan only when the work is genuinely multi-step or uncertain.
4. Make scoped changes using the repository's existing conventions.
5. Run the narrowest relevant verification after the final change.
6. Treat tool errors as evidence, diagnose the environment, and adapt instead
   of repeating command variants blindly.
7. Report what changed, the verification evidence, and any remaining risk.

Completion is evidence-based:

- `completed`: requested work is done and relevant verification passed, or no
  executable verification exists and that limitation is stated.
- `verification_partial`: implementation is complete but some verification
  could not be run or did not finish.
- `waiting_user`: progress requires approval, credentials, an unavailable
  dependency, or a user decision.
- `blocked`: a confirmed external blocker prevents meaningful progress.

The agent must not claim success solely because it wrote files or because a
tool returned output. File changes, command results, artifacts, and run events
are the evidence surface.

## Platform Boundary

Linux is the strongest execution target. The terminal tool can use the
configured isolated sandbox and upgrade through approval when required.

On macOS, SelfMind supports the CLI, daemon, provider adapters, workspace
tools, and LaunchAgent lifecycle. Isolated Linux sandboxing is unavailable:

- `sandbox.mode: auto` falls back to approval-controlled host execution.
- `sandbox.mode: isolated` or `sandbox.required: true` fails closed.
- `/diag` and `selfmind doctor` must make this boundary visible.

Native Windows is not an official execution target. Windows users should use
WSL. Path and shell behavior must be selected by the runtime platform rather
than inferred from user prompts.

## Regression Gate

Behavior changes to the coding loop require:

- focused Go tests at the owning package boundary;
- a coding eval for user-visible message-path changes;
- `go test ./...` on Linux;
- macOS build/test coverage in CI;
- npm package smoke tests for every published platform package.

Recorded eval cassettes are deterministic release evidence. Live provider
success alone is not a release gate.
