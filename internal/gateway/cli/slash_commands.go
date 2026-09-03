package cli

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"selfmind/internal/gateway/command"
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

// slashCommandMetas is the TUI's display order and its local composer hints.
// Usage and Description are NOT owned here: every command the shared
// cross-endpoint catalog (internal/gateway/command) knows takes both from that
// catalog through tuiSlashMeta, so the TUI never carries a second copy that
// drifts from what the daemon accepts (it did: the TUI advertised
// "/queue [clear]" and "/notify <platform|auto>" while the daemon had grown
// "drop <n>" and "desk-first|phone-first"). Hint is the one-line composer
// affordance the catalog does not carry, so it stays local.
var slashCommandMetas = []slashCommandMeta{
	tuiSlashMeta("/help", "show available commands"),
	tuiSlashMeta("/model", "manage model settings"),
	tuiSlashMeta("/status", "show runtime, gateway, and model state"),
	tuiSlashMeta("/tasks", "view current action and work history"),
	tuiSlashMeta("/skills", "list, bind, review candidates, promote, or rollback skills"),
	tuiSlashMeta("/bundles", "load multiple skills together"),
	tuiSlashMeta("/reload-skills", "refresh skill commands"),
	tuiSlashMeta("/memory", "review or manage saved memories"),
	tuiSlashMeta("/curator", "check learning cleanup status"),
	tuiSlashMeta("/checkpoint", "save or inspect workspace checkpoints"),
	tuiSlashMeta("/migrate", "run local storage migrations"),
	tuiSlashMeta("/clear", "clear this conversation view"),
	tuiSlashMeta("/exit", "leave SelfMind"),
	tuiSlashMeta("/compact", "summarize and shrink the transcript"),
	tuiSlashMeta("/paste-image", "attach a clipboard image (Ctrl+V shortcut)"),
	tuiSlashMeta("/mode", "choose what runs without asking"),
	tuiSlashMeta("/capture", "turn this turn into a regression test"),
	tuiSlashMeta("/copy", "copy the last response"),
	tuiSlashMeta("/queue", "view or clear queued work"),
	tuiSlashMeta("/watchers", "view or manage external watchers"),
	tuiSlashMeta("/diag", "runs, queues, memory, context, tasks, models, delivery, execution, tools"),
	tuiSlashMeta("/report", "review recent execution quality and cost"),
	tuiSlashMeta("/search", "review this conversation or find prior work"),
	// Gateway control commands the TUI relays to the daemon. Previously the TUI
	// OMITTED these, so typing /approve fell through to the skill/unknown path
	// and never reached the approve lifecycle. They now route through the shared
	// control passthrough (see gatewayPassthroughCommands) so /approve means the
	// same thing on every surface.
	tuiSlashMeta("/approvals", "list pending approvals or remembered classes"),
	tuiSlashMeta("/approve", "approve once or for this run when offered"),
	tuiSlashMeta("/reject", "reject a pending action"),
	tuiSlashMeta("/stop", "cancel the active run"),
	tuiSlashMeta("/cancel", "cancel the current task"),
	tuiSlashMeta("/id", "show your account identity"),
	registrySlashMeta("/new"),
	registrySlashMeta("/resume"),
	registrySlashMeta("/task"),
	tuiSlashMeta("/workspace", "list or select a workspace"),
	// /ws is an ALIAS: the catalog resolves it to the /workspace entry and holds
	// no alias-specific usage or summary, so repeating that entry here would
	// print the /workspace help row twice. This one row stays local.
	{Name: "/ws", Usage: "/ws [n|id]", Description: "Short alias for /workspace (bare lists, arg selects)", Hint: "list or select a workspace"},
	tuiSlashMeta("/workspaces", "list your workspaces"),
	tuiSlashMeta("/events", "recent task events"),
	tuiSlashMeta("/notify", "set notify preference"),
}

// gatewayPassthroughCommands are derived from the cross-endpoint registry. The
// small wired set below has richer local rendering; every other gateway command
// is a pure relay to the daemon. This keeps newly added controls visible in the
// TUI without maintaining a second command-name catalog.
var gatewayPassthroughCommands = func() []string {
	wired := map[string]bool{
		"/help": true, "/model": true, "/status": true, "/tasks": true,
		"/mode": true, "/queue": true, "/diag": true,
	}
	out := make([]string, 0)
	for _, entry := range command.All() {
		if entry.Scope != command.Gateway {
			continue
		}
		names := append([]string{entry.Name}, entry.Aliases...)
		for _, name := range names {
			// Multi-word aliases are parser conveniences, not editor commands.
			if wired[name] || strings.ContainsAny(name, " \t") {
				continue
			}
			out = append(out, name)
		}
	}
	return out
}()

var slashCommands = []slashCommand{
	{
		slashCommandMeta: slashCommandMetas[0],
		Run:              runHelpCommand,
	},
	{
		slashCommandMeta: slashCommandMetas[1],
		Run: func(m *uiModel, args []string) tea.Cmd {
			if len(args) == 0 {
				return m.openModelManager()
			}
			m.addMessage("assistant", "Usage: /model")
			return nil
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
			return m.quitNow()
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
		slashCommandMeta: metaByName("/copy"),
		Run: func(m *uiModel, args []string) tea.Cmd {
			return m.handleCopyLast()
		},
	},
	{
		slashCommandMeta: metaByName("/queue"),
		Run: func(m *uiModel, args []string) tea.Cmd {
			return m.handleControlPassthrough("/queue", args)
		},
	},
	{
		slashCommandMeta: metaByName("/diag"),
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
	return registrySlashMeta(name)
}

// registrySlashMeta derives a command's presentation from the shared
// cross-endpoint catalog (internal/gateway/command). Only the name survives
// for a command the catalog does not know.
func registrySlashMeta(name string) slashCommandMeta {
	if entry, ok := command.Lookup(name); ok {
		return slashCommandMeta{
			Name:        name,
			Usage:       entry.Usage,
			Description: entry.Summary,
			Hint:        entry.Summary,
		}
	}
	return slashCommandMeta{Name: name}
}

// tuiSlashMeta is a catalog-derived command plus the TUI's own composer hint.
// Only the hint is local; usage and description stay the shared catalog's.
func tuiSlashMeta(name, hint string) slashCommandMeta {
	meta := registrySlashMeta(name)
	meta.Hint = hint
	return meta
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
		// Bare /resume becomes a task picker instead of a usage error — see
		// handleResumeSelect. With an argument it is the same relay as before.
		if name == "/resume" {
			run = func(m *uiModel, args []string) tea.Cmd {
				return m.handleResumeSelect(args)
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
	commands := slashHelpMetas()
	hints := make([]components.CommandHint, 0, len(commands))
	for _, cmd := range commands {
		hints = append(hints, components.CommandHint{Name: cmd.Name, Description: cmd.Hint})
	}
	return hints
}

// slashHelpMetas returns the locally rendered command metadata plus any
// registry-derived gateway passthroughs that do not have a local presentation.
// Keeping this independent of allSlashCommands avoids a package-init cycle:
// the /help command itself opens the view that consumes this list.
func slashHelpMetas() []slashCommandMeta {
	out := append([]slashCommandMeta(nil), slashCommandMetas...)
	seen := make(map[string]bool, len(out))
	for _, meta := range out {
		seen[meta.Name] = true
	}
	for _, name := range gatewayPassthroughCommands {
		if seen[name] {
			continue
		}
		out = append(out, metaByName(name))
		seen[name] = true
	}
	return out
}

func runHelpCommand(m *uiModel, args []string) tea.Cmd {
	m.openHelp()
	return nil
}
