package cli

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"selfmind/internal/ui/components"
)

type slashCommandMeta struct {
	Name        string
	Usage       string
	Description string
	Hint        string
}

type slashCommand struct {
	slashCommandMeta
	Run func(*uiModel, []string) tea.Cmd
}

var slashCommandMetas = []slashCommandMeta{
	{Name: "/help", Usage: "/help", Description: "Open this temporary help page", Hint: "show available commands"},
	{Name: "/model", Usage: "/model [model-name]", Description: "Show or switch model", Hint: "choose model, provider, and reasoning"},
	{Name: "/status", Usage: "/status", Description: "Show runtime status and background processes", Hint: "show runtime, gateway, and model state"},
	{Name: "/tasks", Usage: "/tasks [open|done|archived|all|search <text>] [--workspace <id>] [--page <n>]", Description: "List or search paged work labels", Hint: "view and manage gateway tasks"},
	{Name: "/skills", Usage: "/skills [list|view|history|undo|search|install|audit|delete|archive|pin|unpin|stats|reload]", Description: "Manage learned skills", Hint: "list, view, undo, install, or archive skills"},
	{Name: "/bundles", Usage: "/bundles [list|view|create|delete]", Description: "Manage skill bundles", Hint: "load multiple skills together"},
	{Name: "/reload-skills", Usage: "/reload-skills", Description: "Reload skill tools from disk", Hint: "refresh skill commands"},
	{Name: "/memory", Usage: "/memory [category|conflicts|search|show|correct|forget|pin|unpin|raw|history|undo]", Description: "Review and manage long-term memory", Hint: "review or manage saved memories"},
	{Name: "/curator", Usage: "/curator [status|run|restore]", Description: "Review or run skill cleanup", Hint: "check learning cleanup status"},
	{Name: "/checkpoint", Usage: "/checkpoint [list|save|load|delete] [name]", Description: "Manage workspace checkpoints", Hint: "save or inspect workspace checkpoints"},
	{Name: "/migrate", Usage: "/migrate", Description: "Migrate skills from Hermes Agent", Hint: "run local storage migrations"},
	{Name: "/clear", Usage: "/clear", Description: "Clear conversation history", Hint: "clear this conversation view"},
	{Name: "/exit", Usage: "/exit", Description: "Exit SelfMind", Hint: "leave SelfMind"},
	{Name: "/compact", Usage: "/compact", Description: "Compact older conversation history to free context", Hint: "summarize and shrink the transcript"},
	{Name: "/paste-image", Usage: "/paste-image", Description: "Attach a screenshot from the clipboard (local GUI only, not over SSH)", Hint: "attach a clipboard screenshot"},
	{Name: "/mode", Usage: "/mode [on-request|read-only|auto-edit|full-auto]", Description: "Show or set the approval mode", Hint: "choose what runs without asking"},
	{Name: "/capture", Usage: "/capture [title]", Description: "Save the last turn as a replayable eval case", Hint: "turn this turn into a regression test"},
	{Name: "/history", Usage: "/history", Description: "Open a scrollable view of the full conversation with complete diffs", Hint: "review past turns and full diffs"},
	{Name: "/copy", Usage: "/copy", Description: "Copy the last assistant response to the clipboard", Hint: "copy the last response"},
	{Name: "/queue", Usage: "/queue [clear]", Description: "List queued tasks, or drop all pending queued tasks", Hint: "view or clear queued work"},
	{Name: "/diag", Usage: "/diag [memory]", Description: "Show runtime or memory-governance diagnostics", Hint: "active run, queue, approvals, memory health"},
	{Name: "/search", Usage: "/search [query]", Description: "Search past working sessions (empty = recent sessions)", Hint: "find prior work by keyword"},
	// Gateway control commands the TUI relays to the daemon. Previously the TUI
	// OMITTED these, so typing /approve fell through to the skill/unknown path
	// and never reached the approve lifecycle. They now route through the shared
	// control passthrough (see gatewayPassthroughCommands) so /approve means the
	// same thing on every surface.
	{Name: "/approvals", Usage: "/approvals", Description: "List pending approvals", Hint: "list pending approvals"},
	{Name: "/approve", Usage: "/approve <n|id|all> [task|always]", Description: "Approve a pending action", Hint: "approve a pending action"},
	{Name: "/reject", Usage: "/reject <n|id|all>", Description: "Reject a pending action (or all of them)", Hint: "reject a pending action"},
	{Name: "/stop", Usage: "/stop", Description: "Cancel the active run", Hint: "cancel the active run"},
	{Name: "/cancel", Usage: "/cancel", Description: "Cancel the current task even if no run is active", Hint: "cancel the current task"},
	{Name: "/id", Usage: "/id", Description: "Show your resolved account identity", Hint: "show your account identity"},
	{Name: "/new", Usage: "/new [title]", Description: "Create a new task", Hint: "create a new task"},
	{Name: "/resume", Usage: "/resume <n|task_id>", Description: "Resume a task by list number or id (an archived id reopens it)", Hint: "resume a task by number or id"},
	{Name: "/task", Usage: "/task <n|id> [runs|rename <name>|archive]", Description: "Show a task's detail, runs, rename it, or archive it", Hint: "inspect or manage one task"},
	{Name: "/workspace", Usage: "/workspace [n|id]", Description: "List workspaces (bare) or select one by number/id", Hint: "list or select a workspace"},
	{Name: "/ws", Usage: "/ws [n|id]", Description: "Short alias for /workspace (bare lists, arg selects)", Hint: "list or select a workspace"},
	{Name: "/workspaces", Usage: "/workspaces", Description: "List workspaces (same as bare /workspace)", Hint: "list your workspaces"},
	{Name: "/events", Usage: "/events", Description: "List recent events for the current task", Hint: "recent task events"},
	{Name: "/notify", Usage: "/notify <platform|auto>", Description: "Choose where detached CLI notifications go", Hint: "set notify preference"},
}

// gatewayPassthroughCommands are the Gateway-scope control commands whose TUI
// wiring is a pure relay to the daemon (no local rendering). Kept as a list so
// the cross-endpoint drift-guard test can assert the TUI exposes every gateway
// command.
var gatewayPassthroughCommands = []string{
	"/approvals", "/approve", "/reject", "/stop", "/cancel", "/id", "/new",
	"/resume", "/task", "/workspace", "/workspaces", "/ws", "/events", "/notify",
}

var slashCommands = []slashCommand{
	{
		slashCommandMeta: slashCommandMetas[0],
		Run:              runHelpCommand,
	},
	{
		slashCommandMeta: slashCommandMetas[1],
		Run: func(m *uiModel, args []string) tea.Cmd {
			if len(args) < 1 {
				current := "(unknown)"
				if m.agent != nil {
					current = m.agent.CurrentModel()
				} else if m.modelName != "" {
					current = m.modelName // client mode: show the daemon's display model
				}
				m.addMessage("assistant", fmt.Sprintf("Current model: %s", current))
				m.addMessage("assistant", "Usage: /model <model-name>  (e.g. /model claude-3-5-haiku-20241022)")
				return nil
			}
			return m.handleModelSwitch(args[0])
		},
	},
	{
		slashCommandMeta: slashCommandMetas[2],
		Run: func(m *uiModel, args []string) tea.Cmd {
			return m.handleStatus()
		},
	},
	{
		slashCommandMeta: slashCommandMetas[3],
		Run: func(m *uiModel, args []string) tea.Cmd {
			return m.handleTasks(args)
		},
	},
	{
		slashCommandMeta: slashCommandMetas[4],
		Run: func(m *uiModel, args []string) tea.Cmd {
			return m.handleSkills(args)
		},
	},
	{
		slashCommandMeta: slashCommandMetas[5],
		Run: func(m *uiModel, args []string) tea.Cmd {
			return m.handleBundles(args)
		},
	},
	{
		slashCommandMeta: slashCommandMetas[6],
		Run: func(m *uiModel, args []string) tea.Cmd {
			return m.handleReloadSkills()
		},
	},
	{
		slashCommandMeta: slashCommandMetas[7],
		Run: func(m *uiModel, args []string) tea.Cmd {
			return m.handleMemory(args)
		},
	},
	{
		slashCommandMeta: slashCommandMetas[8],
		Run: func(m *uiModel, args []string) tea.Cmd {
			return m.handleCurator(args)
		},
	},
	{
		slashCommandMeta: slashCommandMetas[9],
		Run: func(m *uiModel, args []string) tea.Cmd {
			if len(args) < 1 {
				m.addMessage("assistant", "Usage: /checkpoint [list|save|load|delete] [name]")
				return nil
			}
			return m.handleCheckpoint(args)
		},
	},
	{
		slashCommandMeta: slashCommandMetas[10],
		Run: func(m *uiModel, args []string) tea.Cmd {
			return m.handleMigration()
		},
	},
	{
		slashCommandMeta: slashCommandMetas[11],
		Run: func(m *uiModel, args []string) tea.Cmd {
			m.messages = []ChatMessage{}
			m.activePlanJSON = ""
			return m.clearHybridScreen()
		},
	},
	{
		slashCommandMeta: slashCommandMetas[12],
		Run: func(m *uiModel, args []string) tea.Cmd {
			return tea.Quit
		},
	},
	{
		slashCommandMeta: slashCommandMetas[13],
		Run: func(m *uiModel, args []string) tea.Cmd {
			return m.handleCompact()
		},
	},
	{
		slashCommandMeta: slashCommandMetas[14],
		Run: func(m *uiModel, args []string) tea.Cmd {
			return m.handlePasteImage()
		},
	},
	{
		slashCommandMeta: slashCommandMetas[15],
		Run: func(m *uiModel, args []string) tea.Cmd {
			return m.handleMode(args)
		},
	},
	{
		slashCommandMeta: slashCommandMetas[16],
		Run: func(m *uiModel, args []string) tea.Cmd {
			return m.handleCapture(args)
		},
	},
	{
		slashCommandMeta: slashCommandMetas[17],
		Run: func(m *uiModel, args []string) tea.Cmd {
			m.openHistory()
			return nil
		},
	},
	{
		slashCommandMeta: slashCommandMetas[18],
		Run: func(m *uiModel, args []string) tea.Cmd {
			return m.handleCopyLast()
		},
	},
	{
		slashCommandMeta: slashCommandMetas[19],
		Run: func(m *uiModel, args []string) tea.Cmd {
			return m.handleControlPassthrough("/queue", args)
		},
	},
	{
		slashCommandMeta: slashCommandMetas[20],
		Run: func(m *uiModel, args []string) tea.Cmd {
			return m.handleControlPassthrough("/diag", args)
		},
	},
	{
		slashCommandMeta: metaByName("/search"),
		Run: func(m *uiModel, args []string) tea.Cmd {
			return m.handleSessionSearch(args)
		},
	},
}

// metaByName finds a declared slashCommandMeta by command name.
func metaByName(name string) slashCommandMeta {
	for _, meta := range slashCommandMetas {
		if meta.Name == name {
			return meta
		}
	}
	return slashCommandMeta{Name: name}
}

// allSlashCommands is the full command table: the hand-wired commands above
// plus the gateway control commands that relay to the daemon.
var allSlashCommands = func() []slashCommand {
	out := make([]slashCommand, 0, len(slashCommands)+len(gatewayPassthroughCommands))
	out = append(out, slashCommands...)
	for _, name := range gatewayPassthroughCommands {
		name := name
		run := func(m *uiModel, args []string) tea.Cmd {
			return m.handleControlPassthrough(name, args)
		}
		// /workspace still relays to the gateway (which owns resolution and
		// current_workspace), but the TUI additionally captures the resolved
		// workspace from the success reply as the session override — see
		// handleWorkspaceSelect.
		if name == "/workspace" || name == "/ws" {
			run = func(m *uiModel, args []string) tea.Cmd {
				return m.handleWorkspaceSelect(args)
			}
		}
		out = append(out, slashCommand{
			slashCommandMeta: metaByName(name),
			Run:              run,
		})
	}
	return out
}()

var slashCommandIndex = func() map[string]slashCommand {
	out := make(map[string]slashCommand, len(allSlashCommands))
	for _, cmd := range allSlashCommands {
		out[cmd.Name] = cmd
	}
	return out
}()

func slashCommandHints() []components.CommandHint {
	hints := make([]components.CommandHint, 0, len(slashCommandMetas))
	for _, cmd := range slashCommandMetas {
		hints = append(hints, components.CommandHint{Name: cmd.Name, Description: cmd.Hint})
	}
	return hints
}

func runHelpCommand(m *uiModel, args []string) tea.Cmd {
	m.openHelp()
	return nil
}
