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
	{Name: "/tasks", Usage: "/tasks", Description: "List global tasks", Hint: "view and manage gateway tasks"},
	{Name: "/skills", Usage: "/skills [list|view|history|undo|search|install|audit|delete|archive|pin|unpin|stats|reload]", Description: "Manage learned skills", Hint: "list, view, undo, install, or archive skills"},
	{Name: "/bundles", Usage: "/bundles [list|view|create|delete]", Description: "Manage skill bundles", Hint: "load multiple skills together"},
	{Name: "/reload-skills", Usage: "/reload-skills", Description: "Reload skill tools from disk", Hint: "refresh skill commands"},
	{Name: "/memory", Usage: "/memory [list|history|remove|undo]", Description: "Review, audit, remove, or undo saved memories", Hint: "review, audit, or undo saved memories"},
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
			return m.handleTasks()
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
			m.viewport.SetContent("")
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
}

var slashCommandIndex = func() map[string]slashCommand {
	out := make(map[string]slashCommand, len(slashCommands))
	for _, cmd := range slashCommands {
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
