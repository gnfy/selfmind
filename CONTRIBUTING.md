# Contributing to SelfMind

Thank you for your interest in contributing!

## Development Setup

```sh
git clone https://github.com/your-org/selfmind.git
cd selfmind
go build ./...     # verify it compiles
go test ./...      # run all tests
```

The project uses Go 1.26+. All tools are pure Go (`CGO_ENABLED=0`).

## Project Layout

```
cmd/selfmind/        # Application entry point
internal/
  kernel/            # Core agent (reasoning, context, memory, reflection)
  gateway/           # Platform adapters (CLI, WeChat, Telegram, etc.)
  tools/             # Tool registry, middleware, built-in tools
  ui/                # TUI components (Bubble Tea / Lip Gloss)
  platform/          # Config loading
```

**Key rules:**
- `kernel` must NOT import `tools` (avoid import cycles)
- `tools` may import `kernel` for interface assertions only
- All platform-specific code goes under `gateway/<platform>/`

## Adding a Built-in Tool

1. Create `internal/tools/my_tool.go`
2. Implement the `Tool` interface (see `builtin.go` for examples)
3. Register in `RegisterBuiltins(d *Dispatcher)` in `extended.go`

## Adding a Platform Adapter

1. Create `internal/gateway/<platform>/adapter.go`
2. Implement the handler pattern (see `wechat/adapter.go`)
3. Wire it up in `init_gateway.go`

## Running the TUI

```sh
go run ./cmd/selfmind
```

For debug logging:

```sh
export SELF_DEBUG=1
go run ./cmd/selfmind
```

## Commit Messages

Follow [Conventional Commits](https://www.conventionalcommits.org/):

```
feat: add Telegram adapter
fix: resolve import cycle in kernel tests
docs: add CONTRIBUTING.md
refactor: rename tool_backend to backend
```

## Testing

```sh
go test ./...              # all tests
go test ./internal/kernel  # kernel package only
go vet ./...               # static analysis
```

## Code Style

- Run `go fmt ./...` before committing
- All exported identifiers need doc comments
- Error messages should be lowercase and informative (e.g., `fmt.Errorf("load config: %w", err)`)
- No placeholder comments like `// TODO: fix later`

## Questions

Open an issue for bugs, feature requests, or design discussions.
