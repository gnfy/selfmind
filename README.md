# SelfMind

A production-grade, multi-tenant AI Agent kernel written in Go (1.26+).

```
┌─────────────────────────────────────────────────────────┐
│  CLI / WeChat / DingTalk / Telegram / Web               │
└──────────────────┬────────────────────────────────────┘
                   │
         ┌─────────▼──────────┐
         │  Gateway           │  Intent classification
         │  (router/)         │  Identity mapping
         └─────────┬──────────┘  Task management
                   │
         ┌─────────▼──────────┐
         │  Kernel            │  Agent reasoning loop
         │  (kernel/)        │  Context engine
         │                    │  Reflection & evolution
         │                    │  Memory (SQLite FTS5)
         └─────────┬──────────┘
                   │
         ┌─────────▼──────────┐
         │  Tools             │  Middleware chain
         │  (tools/)          │  Built-in tools
         │                    │  MCP client
         │                    │  Skill loader
         └────────────────────┘
```

## Features

- **Multi-tenant SQLite memory** with FTS5 full-text search
- **Autonomous self-evolution**: reflects on completed tasks and archives new skills
- **Dynamic skill system**: load `.md` skill files at runtime
- **MCP client**: connect to any MCP server (stdio or HTTP)
- **Multi-channel**: CLI, WeChat, DingTalk, Telegram — unified identity + task context
- **Bubble Tea TUI**: modern terminal interface with slash commands
- **Pure Go build**: `CGO_ENABLED=0`, static binary, no external dependencies

## Quick Start

### Build

```sh
git clone https://github.com/your-org/selfmind.git
cd selfmind
go build -ldflags="-s -w" -o selfmind ./cmd/selfmind
```

### Configure

```sh
mkdir -p ~/.selfmind
cat > ~/.selfmind/config.yaml << 'EOF'
providers:
  anthropic_api_key: "sk-ant-..."   # or openai_api_key

agent:
  max_iterations: 90

evolution:
  enabled: true
  mode: "auto"
  min_complexity_threshold: 3
  auto_archive_confidence: 0.8

storage:
  data_dir: "~/.selfmind/data"

cron:
  enabled: false

mcp:
  servers: []
EOF
```

### Run

```sh
./selfmind
```

Press `Ctrl+C` to exit.

## Architecture

```
cmd/selfmind/           # Entry point and wiring
internal/
  kernel/              # Core agent engine
    agent.go           # Reasoning loop
    context_engine.go  # Token budget & message building
    reflection.go       # Self-evolution logic
    backend.go         # ToolBackend interface
    memory/            # SQLite provider (FTS5, checkpoints)
    llm/               # Anthropic / OpenAI / OpenRouter adapters
    identity/          # Platform → unified_uid mapping
    task/              # Global task manager + cron scheduler
  gateway/             # Platform adapters
    router/            # Unified handler + intent classifier
    cli/               # Bubble Tea TUI controller
    wechat/            # WeChat adapter (stub)
    telegram/          # Telegram adapter (stub)
  tools/               # Tool system
    dispatcher.go      # Registry + middleware pipeline
    builtin.go         # list_files, read_file, write_file, ...
    extended.go        # web_search, vision, tts, session_search, ...
    middleware.go      # Auth, TenantIsolation, Approval, RateLimit
    skill_loader.go    # SKILL.md parser + dynamic registration
    mcp_client.go      # MCP server connections
  ui/                  # TUI components (Bubble Tea / Lip Gloss)
internal/platform/     # Config loading
```

## Slash Commands

| Command | Description |
|---------|-------------|
| `/help` | Show available commands |
| `/status` | Show agent status and config |
| `/model` | Switch LLM model |
| `/tasks` | List all tasks |
| `/exit` | Exit the TUI |

## Writing a Tool

```go
// internal/tools/my_tool.go
package tools

type MyTool struct {
    BaseTool
}

func NewMyTool() *MyTool {
    return &MyTool{
        BaseTool{
            name:        "my_tool",
            description: "Does something useful",
            schema: ToolSchema{
                Type: "object",
                Properties: map[string]PropertyDef{
                    "input": {Type: "string", Description: "Input text"},
                },
                Required: []string{"input"},
            },
        },
    }
}

func (t *MyTool) Execute(args map[string]interface{}) (string, error) {
    input, _ := args["input"].(string)
    // do work
    return "result: " + input, nil
}
```

Register in `init_tools.go`:

```go
d.RegisterTool(NewMyTool())
```

## Writing a Platform Adapter

Implement `Handle(ctx, unifiedUID, channel, input string) (string, error)` in `gateway/<platform>/adapter.go`. See `internal/gateway/wechat/adapter.go` for a reference stub.

## License

MIT
