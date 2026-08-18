package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"selfmind/internal/app"
	selfeval "selfmind/internal/eval"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/command"
	"selfmind/internal/kernel"
	"selfmind/internal/tools"
)

// dispatch runs a management tool either on the daemon (client mode, via the
// installed tool-dispatch function) or on the in-process agent backend. It is
// the single seam that lets agent-backed slash commands work whether the TUI
// owns an agent or is a thin client to a gateway daemon.
func (m *uiModel) dispatch(tool string, args map[string]interface{}) (string, error) {
	if m.toolDispatchFn != nil {
		return m.toolDispatchFn(tool, args)
	}
	if m.agent != nil && m.agent.Dispatcher() != nil {
		return m.agent.Dispatcher().Dispatch(tool, args)
	}
	return "", fmt.Errorf("no agent or daemon available to run %s", tool)
}

func (m *uiModel) handleCommand(input string) tea.Cmd {
	parts := strings.Fields(input)
	m.editor.Reset()
	if len(parts) == 0 {
		return nil
	}
	// Echo the typed command as a user cell BEFORE any reply renders. Normal
	// chat turns echo their input, but slash turns did not, so a control
	// session (/workspaces → /workspace 2 → /resume …) read as disembodied
	// replies with no visible questions (observed live). addMessage covers
	// both surfaces: hybrid scrollback (commit) and the legacy viewport.
	m.addMessage("user", input)
	if cmd, ok := slashCommandIndex[parts[0]]; ok {
		// In daemon-client mode agent-backed commands route through the tool
		// dispatch seam (m.dispatch → daemon) or through the message processor
		// (/status, /tasks). The few that need the in-process store (/skills
		// stats, /model switch) detect client mode themselves and
		// return a clear notice. So no top-level gate is needed for safety.
		return cmd.Run(m, parts[1:])
	}
	if strings.HasPrefix(parts[0], "/") {
		instruction := strings.TrimSpace(strings.TrimPrefix(input, parts[0]))
		return m.handleSkillSlash(parts[0], instruction)
	}
	return nil
}

func (m *uiModel) handleSkillSlash(slashName, instruction string) tea.Cmd {
	prompt, displayName, ok, err := tools.ResolveSkillInvocationForTenant(m.tenantID, slashName, instruction)
	if err != nil {
		m.addMessage("assistant", fmt.Sprintf("Skill command error: %v", err))
		return nil
	}
	if !ok {
		// Unify the unknown-command decision with the gateway: if the token is a
		// near-miss for a control command, suggest it (same edit-distance logic
		// via the shared registry) before falling back to the generic hint.
		if suggestion := command.Suggest(slashName); suggestion != "" {
			m.addMessage("assistant", fmt.Sprintf("Unknown command %s — did you mean %s? Use /help for the full list.", slashName, suggestion))
			return nil
		}
		m.addMessage("assistant", fmt.Sprintf("Unknown command: %s. Use /help, /skills list, or /bundles list.", slashName))
		return nil
	}
	// The typed command was already echoed by handleCommand; only the loaded
	// skill notice is added here.
	m.setStatusNotice(noticeInfo, fmt.Sprintf("Loaded skill context: %s", displayName))
	m.thinking = true
	m.runStatus = "working"
	m.thinkingStart = time.Now()
	m.runTokens = 0
	ctx, cancel := context.WithCancel(context.Background())
	m.cancelFn = cancel
	return tea.Batch(m.runAgent(ctx, prompt), m.spinner.Tick)
}

func (m *uiModel) handleMigration() tea.Cmd {
	return func() tea.Msg {
		dir, exists := app.CheckHermesSkills()
		if !exists {
			return MsgAgentDone{Response: "No Hermes skills found to migrate."}
		}
		count, err := app.MigrateHermesSkills(dir)
		if err != nil {
			return MsgAgentDone{Response: fmt.Sprintf("Migration error: %v", err)}
		}
		m.migrationHint = "" // Clear hint after success
		return MsgAgentDone{Response: fmt.Sprintf("Successfully migrated %d skills from Hermes!", count)}
	}
}

func (m *uiModel) handleStatus() tea.Cmd {
	return func() tea.Msg {
		elapsed := time.Since(m.startTime)
		usage := fmt.Sprintf("%s total · %s", compactCount(m.totalTokens), formatUsage(m.runTokens, m.tokenLimit))

		status := fmt.Sprintf("## System Status\n\n- **Provider**: %s\n- **Model**: %s\n- **Uptime**: %s\n- **Token Usage**: %s\n",
			m.providerName, m.modelName, formatDuration(elapsed), usage)

		if m.messageProcessor != nil {
			resp, _ := m.messageProcessor(context.Background(), m.controlMessageRequest("/status"))
			if resp.Error != "" {
				status += fmt.Sprintf("- **Current Task**: error: %s\n", resp.Error)
			} else if strings.TrimSpace(resp.Content) != "" {
				status += "\n### Task Status\n\n" + resp.Content + "\n"
			}
		} else if m.gateway != nil {
			t, err := m.gateway.GetCurrentTaskInfo(context.Background(), m.tenantID)
			if err == nil && t != nil {
				status += fmt.Sprintf("- **Current Task**: [%d] %s\n", t.ID, t.Title)
			} else {
				status += "- **Current Task**: None\n"
			}
		}

		registry := tools.GetProcessRegistryForTenant(m.tenantID)
		procs := registry.List()
		if len(procs) > 0 {
			status += "\n### Background Processes\n"
			for _, p := range procs {
				idStr := p["id"].(string)
				if len(idStr) > 8 {
					idStr = idStr[:8]
				}
				status += fmt.Sprintf("- `%s`: %s (%s)\n", idStr, p["command"], p["status"])
			}
		}

		return MsgAgentDone{Response: status}
	}
}

func (m *uiModel) handleTasks(args []string) tea.Cmd {
	return func() tea.Msg {
		if m.messageProcessor != nil {
			// Relay variants (/tasks done|archived|all) to the gateway, which
			// owns the aggregated view.
			content := strings.TrimSpace("/tasks " + strings.Join(args, " "))
			resp, _ := m.messageProcessor(context.Background(), m.controlMessageRequest(content))
			if resp.Error != "" {
				return MsgAgentDone{Response: fmt.Sprintf("Error fetching tasks: %s", resp.Error)}
			}
			return MsgAgentDone{Response: resp.Content}
		}
		if m.gateway == nil {
			return MsgAgentDone{Response: "Gateway not initialized, cannot list tasks."}
		}
		tasks, err := m.gateway.ListTasks(context.Background(), m.tenantID)
		if err != nil {
			return MsgAgentDone{Response: fmt.Sprintf("Error fetching tasks: %v", err)}
		}
		if len(tasks) == 0 {
			return MsgAgentDone{Response: "No tasks found."}
		}
		var sb strings.Builder
		sb.WriteString("## Global Tasks\n\n")
		for _, t := range tasks {
			status := "⏳"
			if t.Status == "done" {
				status = "✅"
			}
			if t.Status == "cancelled" {
				status = "❌"
			}
			sb.WriteString(fmt.Sprintf("%s [%d] %s (Created: %s)\n",
				status, t.ID, t.Title, t.CreatedAt.Format("01-02 15:04")))
		}
		return MsgAgentDone{Response: sb.String()}
	}
}

// handleControlPassthrough forwards a gateway control command (e.g. /queue,
// /diag) to the daemon and renders its plain-text reply. These are pre-agent
// control commands owned by the gateway; the TUI is a thin client that just
// relays them (never resolves them locally).
func (m *uiModel) handleControlPassthrough(command string, args []string) tea.Cmd {
	content := command
	if len(args) > 0 {
		content = command + " " + strings.Join(args, " ")
	}
	return func() tea.Msg {
		if m.messageProcessor == nil {
			return MsgAgentDone{Response: "Gateway not initialized."}
		}
		resp, _ := m.messageProcessor(context.Background(), m.controlMessageRequest(content))
		if resp.Error != "" {
			return MsgAgentDone{Response: fmt.Sprintf("Error: %s", resp.Error)}
		}
		return MsgAgentDone{Response: resp.Content}
	}
}

// handleResumeSelect backs /resume. With a reference it is a plain relay. Bare,
// it turns into a picker: the daemon owns task ordering, so the TUI relays
// /tasks and presents that reply as the menu rather than numbering a list of
// its own — a locally numbered menu would drift from the resolver that
// /resume <n> actually uses. armResumePicker makes the next bare number expand
// to /resume <n>.
func (m *uiModel) handleResumeSelect(args []string) tea.Cmd {
	if len(args) > 0 {
		m.resumePickerArmed = false
		return m.handleControlPassthrough("/resume", args)
	}
	list := m.handleControlPassthrough("/tasks", nil)
	return func() tea.Msg {
		msg := list()
		done, ok := msg.(MsgAgentDone)
		if !ok || strings.TrimSpace(done.Response) == "" {
			return msg
		}
		m.resumePickerArmed = true
		done.Response = strings.TrimRight(done.Response, "\n") +
			"\n\nType a number to resume that task (or /resume <task_id>)."
		return done
	}
}

// bareListNumber returns the input as a list index when it is nothing but a
// positive integer, else "". It gates the armed /resume picker so ordinary
// messages that merely start with a digit still reach the agent.
func bareListNumber(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return ""
	}
	for _, r := range trimmed {
		if r < '0' || r > '9' {
			return ""
		}
	}
	if strings.Trim(trimmed, "0") == "" {
		return ""
	}
	return trimmed
}

func (m *uiModel) controlMessageRequest(content string) api.MessageRequest {
	return api.MessageRequest{
		TenantID:       m.tenantID,
		Platform:       "cli",
		PlatformUserID: cliPlatformUserID(),
		DisplayName:    cliDisplayName(),
		Channel:        m.channel,
		Content:        content,
		ClientCWD:      currentWorkingDir(),
		// Session workspace override (set by /workspace this session): explicit
		// WorkspaceID wins over cwd derivation server-side, so /new and other
		// control turns act in the selected workspace, not the launch dir.
		WorkspaceID: m.workspaceOverrideID,
	}
}

// handleWorkspaceSelect relays `/workspace <n|id>` to the gateway exactly like
// a control passthrough, then — on a successful switch — pins the resolved
// workspace as this session's override so subsequent agent turns actually run
// there (defect: the next message silently fell back to the launch-cwd
// workspace because only ClientCWD rode the request). Failure replies (usage
// text, "no workspace matching …") render as-is and set nothing.
func (m *uiModel) handleWorkspaceSelect(args []string) tea.Cmd {
	content := "/workspace"
	if len(args) > 0 {
		content += " " + strings.Join(args, " ")
	}
	return func() tea.Msg {
		if m.messageProcessor == nil {
			return MsgAgentDone{Response: "Gateway not initialized."}
		}
		resp, _ := m.messageProcessor(context.Background(), m.controlMessageRequest(content))
		if resp.Error != "" {
			return MsgAgentDone{Response: fmt.Sprintf("Error: %s", resp.Error)}
		}
		if id, name, path, ok := parseWorkspaceSwitchReply(resp.Content); ok {
			return MsgWorkspaceSwitched{ID: id, Name: name, Path: path, Reply: resp.Content}
		}
		return MsgAgentDone{Response: resp.Content}
	}
}

// parseWorkspaceSwitchReply extracts the resolved workspace from the gateway's
// success reply, whose shape is a stable control-command contract:
//
//	Current workspace: <name> (<ws_id>)
//	<local_path>
//
// Parsing is tolerant (names may contain spaces or parentheses — the id is the
// LAST parenthesized token on the first line) and any mismatch returns ok=false
// so an unexpected reply never fabricates an override.
func parseWorkspaceSwitchReply(reply string) (id, name, path string, ok bool) {
	lines := strings.Split(strings.TrimSpace(reply), "\n")
	const prefix = "Current workspace: "
	first := strings.TrimSpace(lines[0])
	if !strings.HasPrefix(first, prefix) {
		return "", "", "", false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(first, prefix))
	if !strings.HasSuffix(rest, ")") {
		return "", "", "", false
	}
	open := strings.LastIndex(rest, "(")
	if open < 0 {
		return "", "", "", false
	}
	id = strings.TrimSpace(rest[open+1 : len(rest)-1])
	name = strings.TrimSpace(rest[:open])
	if id == "" {
		return "", "", "", false
	}
	if len(lines) > 1 {
		path = strings.TrimSpace(lines[1])
	}
	return id, name, path, true
}

func (m *uiModel) handleSkills(args []string) tea.Cmd {
	return func() tea.Msg {
		action := "list"
		if len(args) > 0 {
			action = args[0]
		}
		switch action {
		case "list":
			skills, err := tools.ListSkillsForTenant(m.tenantID, false)
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Error listing skills: %v", err)}
			}
			if len(skills) == 0 {
				return MsgAgentDone{Response: "No skills found. Skills are created automatically after reusable work is discovered."}
			}
			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("## Skills (%d)\n\n", len(skills)))
			for _, s := range skills {
				pin := ""
				if s.Pinned {
					pin = " pinned"
				}
				sb.WriteString(fmt.Sprintf("- **%s** [%s/%s%s]: %s\n  _%s_\n", s.Name, s.State, s.Source, pin, valueOr(s.Description, "(no description)"), s.Path))
			}
			sb.WriteString("\nCommands: /skills view <name>, /skills bind <name>, /skills unbind, /skills candidates, /skills candidate <version>, /skills promote <version>, /skills reject <version>, /skills rollback <version>")
			return MsgAgentDone{Response: sb.String()}
		case "view":
			if len(args) < 2 {
				return MsgAgentDone{Response: "Usage: /skills view <name>"}
			}
			content, err := tools.ReadSkillForTenant(m.tenantID, args[1])
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Error reading skill: %v", err)}
			}
			return MsgAgentDone{Response: content}
		case "search":
			if len(args) < 2 {
				return MsgAgentDone{Response: "Usage: /skills search <query>"}
			}
			skills, err := tools.SearchSkillsForTenant(m.tenantID, strings.Join(args[1:], " "))
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Error searching skills: %v", err)}
			}
			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("## Skill Search (%d)\n\n", len(skills)))
			for _, s := range skills {
				sb.WriteString(fmt.Sprintf("- **%s**: %s\n", s.Name, valueOr(s.Description, "(no description)")))
			}
			return MsgAgentDone{Response: sb.String()}
		case "candidates":
			request := map[string]interface{}{"action": "candidate_list", "_tenant_id": m.tenantID}
			if len(args) >= 2 {
				request["name"] = args[1]
			}
			resp, err := m.dispatch("skill_lifecycle_manage", request)
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Error loading Skill candidates: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "candidate", "promote", "reject", "rollback":
			if len(args) < 2 {
				return MsgAgentDone{Response: "Usage: /skills " + action + " <version_hash> [skill_key]"}
			}
			lifecycleAction := map[string]string{
				"candidate": "candidate_read", "promote": "candidate_promote",
				"reject": "candidate_reject", "rollback": "rollback",
			}[action]
			request := map[string]interface{}{
				"action": lifecycleAction, "version_hash": args[1], "_tenant_id": m.tenantID,
			}
			if len(args) >= 3 {
				request["skill_key"] = args[2]
			}
			resp, err := m.dispatch("skill_lifecycle_manage", request)
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Skill lifecycle error: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "binding":
			resp, err := m.dispatch("skill_lifecycle_manage", map[string]interface{}{"action": "binding_get", "_tenant_id": m.tenantID})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Error loading Skill binding: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "bind":
			if len(args) < 2 {
				return MsgAgentDone{Response: "Usage: /skills bind <name>"}
			}
			resp, err := m.dispatch("skill_lifecycle_manage", map[string]interface{}{
				"action": "binding_bind", "name": args[1], "_tenant_id": m.tenantID,
			})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Error binding Skill: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "unbind":
			resp, err := m.dispatch("skill_lifecycle_manage", map[string]interface{}{"action": "binding_unbind", "_tenant_id": m.tenantID})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Error releasing Skill binding: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "history":
			if len(args) < 2 {
				return MsgAgentDone{Response: "Usage: /skills history <name>"}
			}
			resp, err := m.dispatch("skill_manage", map[string]interface{}{"action": "history", "name": args[1], "_tenant_id": m.tenantID})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Error loading skill history: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "undo":
			if len(args) < 2 {
				return MsgAgentDone{Response: "Usage: /skills undo <change_id>"}
			}
			resp, err := m.dispatch("skill_manage", map[string]interface{}{"action": "undo", "change_id": args[1], "_tenant_id": m.tenantID})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Error undoing skill change: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "catalog":
			resp, err := m.dispatch("skill_catalog", map[string]interface{}{"action": "list", "_tenant_id": m.tenantID})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Error loading catalog: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "install":
			if len(args) < 2 {
				return MsgAgentDone{Response: "Usage: /skills install <official/name|path|url> [--name name] [--force]"}
			}
			installName := ""
			force := false
			for i := 2; i < len(args); i++ {
				switch args[i] {
				case "--force":
					force = true
				case "--name":
					if i+1 < len(args) {
						installName = args[i+1]
						i++
					}
				}
			}
			resp, err := m.dispatch("skill_catalog", map[string]interface{}{
				"action":     "install",
				"source":     args[1],
				"name":       installName,
				"force":      force,
				"_tenant_id": m.tenantID,
			})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Error installing skill: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "audit":
			auditName := ""
			if len(args) >= 2 {
				auditName = args[1]
			}
			resp, err := m.dispatch("skill_catalog", map[string]interface{}{"action": "audit", "name": auditName, "_tenant_id": m.tenantID})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Skill audit error: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "delete":
			if len(args) < 2 {
				return MsgAgentDone{Response: "Usage: /skills delete <name>"}
			}
			resp, err := m.dispatch("skill_manage", map[string]interface{}{"action": "delete", "name": args[1], "_tenant_id": m.tenantID})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Error deleting skill: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "archive":
			if len(args) < 2 {
				return MsgAgentDone{Response: "Usage: /skills archive <name>"}
			}
			resp, err := m.dispatch("skill_manage", map[string]interface{}{"action": "archive", "name": args[1], "_tenant_id": m.tenantID})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Error archiving skill: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "pin", "unpin":
			if len(args) < 2 {
				return MsgAgentDone{Response: "Usage: /skills " + action + " <name>"}
			}
			resp, err := m.dispatch("skill_manage", map[string]interface{}{"action": action, "name": args[1], "_tenant_id": m.tenantID})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Error updating pin: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "stats":
			if m.agent == nil || m.agent.Memory() == nil {
				if m.clientMode {
					return MsgAgentDone{Response: "`/skills stats` is not available over the daemon yet; use `/skills list` (per-skill call counts ride the list view)."}
				}
				return MsgAgentDone{Response: "Memory not initialized."}
			}
			store := kernel.NewSkillStore(m.agent.Memory())
			stats, err := store.FormatStats(context.Background(), m.tenantID)
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Error loading skill stats: %v", err)}
			}
			return MsgAgentDone{Response: stats}
		case "reload":
			resp, err := m.dispatch("skill_manage", map[string]interface{}{"action": "reload", "_tenant_id": m.tenantID})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Error reloading skills: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		default:
			return MsgAgentDone{Response: "Usage: /skills [list|view|candidates|candidate|promote|reject|rollback|binding|bind|unbind|history|undo|search|catalog|install|audit|delete|archive|pin|unpin|stats|reload]"}
		}
	}
}

func (m *uiModel) handleReloadSkills() tea.Cmd {
	return func() tea.Msg {
		resp, err := m.dispatch("skill_manage", map[string]interface{}{"action": "reload", "_tenant_id": m.tenantID})
		if err != nil {
			return MsgAgentDone{Response: fmt.Sprintf("Error reloading skills: %v", err)}
		}
		return MsgAgentDone{Response: resp}
	}
}

func (m *uiModel) handleBundles(args []string) tea.Cmd {
	return func() tea.Msg {
		action := "list"
		if len(args) > 0 {
			action = args[0]
		}
		switch action {
		case "list":
			resp, err := m.dispatch("skill_bundle", map[string]interface{}{"action": "list", "_tenant_id": m.tenantID})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Bundle list error: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "view", "read":
			if len(args) < 2 {
				return MsgAgentDone{Response: "Usage: /bundles view <name>"}
			}
			resp, err := m.dispatch("skill_bundle", map[string]interface{}{"action": "read", "name": args[1], "_tenant_id": m.tenantID})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Bundle read error: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "create":
			if len(args) < 3 {
				return MsgAgentDone{Response: "Usage: /bundles create <name> <skill1,skill2,...>"}
			}
			resp, err := m.dispatch("skill_bundle", map[string]interface{}{
				"action":     "create",
				"name":       args[1],
				"skills":     args[2],
				"_tenant_id": m.tenantID,
			})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Bundle create error: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "delete":
			if len(args) < 2 {
				return MsgAgentDone{Response: "Usage: /bundles delete <name>"}
			}
			resp, err := m.dispatch("skill_bundle", map[string]interface{}{"action": "delete", "name": args[1], "_tenant_id": m.tenantID})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Bundle delete error: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		default:
			return MsgAgentDone{Response: "Usage: /bundles [list|view|create|delete]"}
		}
	}
}

// handleSessionSearch backs /search: find prior working sessions by keyword
// (empty query = recent sessions). It rides the session_search tool through
// m.dispatch, so BOTH modes hit their correct partition: client mode goes to
// the daemon (/v1/dispatch overrides the partition to the person id — what
// daemon runs write), in-process mode uses the local dispatcher with the
// in-process tenant. This closed the last session-search parity gap before the
// in-process path removal (ACTIVE PLAN P0-3).
func (m *uiModel) handleSessionSearch(args []string) tea.Cmd {
	query := strings.TrimSpace(strings.Join(args, " "))
	// `/search current` is the full-fidelity view of the conversation in
	// progress — the escape hatch the immutable hybrid transcript otherwise
	// gives up, since committed cells carry bounded diffs. It lives here rather
	// than under its own command so "look back at work" has one entry point.
	if strings.EqualFold(query, "current") {
		m.openHistory()
		return nil
	}
	return func() tea.Msg {
		dispatchArgs := map[string]interface{}{"limit": 10, "_tenant_id": m.tenantID}
		if query != "" {
			dispatchArgs["query"] = query
		}
		resp, err := m.dispatch("session_search", dispatchArgs)
		if err != nil {
			return MsgAgentDone{Response: fmt.Sprintf("Session search error: %v", err)}
		}
		if strings.TrimSpace(resp) == "" {
			resp = "No matching sessions."
		}
		return MsgAgentDone{Response: resp}
	}
}

func (m *uiModel) handleMemory(args []string) tea.Cmd {
	return func() tea.Msg {
		action := "list"
		if len(args) > 0 {
			action = args[0]
		}
		switch action {
		case "list":
			resp, err := m.dispatch("memory", map[string]interface{}{"action": "list", "_tenant_id": m.tenantID})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Memory list error: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "raw":
			resp, err := m.dispatch("memory", map[string]interface{}{"action": "raw", "_tenant_id": m.tenantID})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Memory raw view error: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "category":
			if len(args) < 2 {
				return MsgAgentDone{Response: "Usage: /memory category <name> [page]"}
			}
			page := 1
			if len(args) >= 3 {
				parsed, err := strconv.Atoi(args[2])
				if err != nil || parsed < 1 {
					return MsgAgentDone{Response: "Usage: /memory category <name> [page]"}
				}
				page = parsed
			}
			resp, err := m.dispatch("memory", map[string]interface{}{
				"action": "category", "category": args[1], "page": page, "_tenant_id": m.tenantID,
			})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Memory category error: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "conflicts":
			resp, err := m.dispatch("memory", map[string]interface{}{"action": "conflicts", "_tenant_id": m.tenantID})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Memory conflicts error: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "search":
			if len(args) < 2 {
				return MsgAgentDone{Response: "Usage: /memory search <query>"}
			}
			resp, err := m.dispatch("memory", map[string]interface{}{
				"action": "search", "query": strings.Join(args[1:], " "), "_tenant_id": m.tenantID,
			})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Memory search error: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "show", "explain":
			if len(args) != 2 {
				return MsgAgentDone{Response: "Usage: /memory show <ref>"}
			}
			resp, err := m.dispatch("memory", map[string]interface{}{
				"action": action, "ref": args[1], "_tenant_id": m.tenantID,
			})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Memory detail error: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "forget":
			if len(args) != 2 {
				return MsgAgentDone{Response: "Usage: /memory forget <ref>"}
			}
			resp, err := m.dispatch("memory", map[string]interface{}{
				"action": "forget", "ref": args[1], "_tenant_id": m.tenantID,
			})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Memory forget error: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "correct":
			if len(args) < 3 {
				return MsgAgentDone{Response: "Usage: /memory correct <ref> <replacement text>"}
			}
			resp, err := m.dispatch("memory", map[string]interface{}{
				"action": "correct", "ref": args[1], "content": strings.Join(args[2:], " "), "_tenant_id": m.tenantID,
			})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Memory correction error: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "history":
			target := ""
			if len(args) >= 2 {
				target = args[1]
			}
			resp, err := m.dispatch("memory", map[string]interface{}{"action": "history", "target": target, "_tenant_id": m.tenantID})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Memory history error: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "remove":
			if len(args) < 3 {
				return MsgAgentDone{Response: "Usage: /memory remove <user|memory> <id-or-text>"}
			}
			target := args[1]
			needle := strings.Join(args[2:], " ")
			resp, err := m.dispatch("memory", map[string]interface{}{
				"action":     "remove",
				"target":     target,
				"old_text":   needle,
				"_tenant_id": m.tenantID,
			})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Remove failed: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "undo":
			if len(args) < 2 {
				return MsgAgentDone{Response: "Usage: /memory undo <change_id>"}
			}
			resp, err := m.dispatch("memory", map[string]interface{}{
				"action":     "undo",
				"change_id":  args[1],
				"_tenant_id": m.tenantID,
			})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Memory undo error: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "pin":
			if len(args) < 2 {
				return MsgAgentDone{Response: "Usage: /memory pin <ref|authoritative fact>"}
			}
			if len(args) == 2 && looksLikeMemoryReference(args[1]) {
				resp, err := m.dispatch("memory", map[string]interface{}{
					"action": "pin", "ref": args[1], "_tenant_id": m.tenantID,
				})
				if err != nil {
					return MsgAgentDone{Response: fmt.Sprintf("Pin failed: %v", err)}
				}
				return MsgAgentDone{Response: resp}
			}
			resp, err := m.dispatch("memory", map[string]interface{}{
				"action":     "add",
				"target":     "pinned",
				"content":    strings.Join(args[1:], " "),
				"_tenant_id": m.tenantID,
			})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Pin failed: %v", err)}
			}
			return MsgAgentDone{Response: "Pinned (authoritative): " + resp}
		case "unpin":
			if len(args) != 2 || !looksLikeMemoryReference(args[1]) {
				return MsgAgentDone{Response: "Usage: /memory unpin <ref>"}
			}
			resp, err := m.dispatch("memory", map[string]interface{}{
				"action": "unpin", "ref": args[1], "_tenant_id": m.tenantID,
			})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Unpin failed: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		default:
			return MsgAgentDone{Response: "Usage: /memory [list|category <name> [page]|conflicts|search <query>|show <ref>|correct <ref> <text>|forget <ref>|pin <ref|text>|unpin <ref>|raw|history|undo]"}
		}
	}
}

func looksLikeMemoryReference(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 8 && len(value) != 36 {
		return false
	}
	for _, r := range value {
		if r == '-' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') {
			continue
		}
		return false
	}
	return true
}

// handleCompact shrinks the visible transcript to the most recent exchanges,
// replacing older messages with a single marker. This frees rendered context on
// long sessions (the codex /compact affordance) deterministically, without a
// model call. Durable task/memory state is unaffected — only the chat view.
func (m *uiModel) handleCompact() tea.Cmd {
	const keep = 6
	if len(m.messages) <= keep+1 {
		m.addMessage("assistant", "Nothing to compact yet.")
		return nil
	}
	removed := len(m.messages) - keep
	marker := ChatMessage{
		Role:      "system",
		Content:   fmt.Sprintf("[earlier conversation compacted — %d message(s) hidden from view]", removed),
		Timestamp: time.Now(),
	}
	tail := append([]ChatMessage{}, m.messages[len(m.messages)-keep:]...)
	m.messages = append([]ChatMessage{marker}, tail...)
	return nil
}

// handlePasteImage reads an image from the OS clipboard, saves it to a temp
// file, and drops its path into the composer — which the submit path then turns
// into an image attachment (reusing the path pipeline). Clipboard images are
// only reachable when the TUI runs on a machine with a GUI clipboard (including
// WSL reading the Windows clipboard); over SSH there is none, so it explains
// the alternative instead of failing silently.
func (m *uiModel) handlePasteImage() tea.Cmd {
	path, err := clipboardImageToFile()
	if err != nil {
		m.addMessage("assistant", "Could not read an image from the clipboard: "+err.Error()+
			"\nHint: clipboard capture only works when SelfMind runs on the local machine (including WSL); over SSH there is no clipboard — drop in / type an image file path instead, or send the image via WeChat.")
		return nil
	}
	m.attachClipboardImage(path, "/paste-image")
	return nil
}

// tryClipboardImagePaste is the Ctrl+V / empty-paste hook: if the clipboard
// holds an image, attach it and report handled=true. On no image (or no
// clipboard, e.g. over SSH) it stays silent and returns false so the key falls
// through to normal handling. Returns a Cmd alongside handled.
func (m *uiModel) tryClipboardImagePaste() (tea.Cmd, bool) {
	path, err := clipboardImageToFile()
	if err != nil {
		return nil, false
	}
	m.attachClipboardImage(path, "")
	return nil, true
}

// attachClipboardImage registers a saved clipboard-image path as an editor
// image attachment shown as a compact [[ image:N name ]] token (mirroring
// large-paste placeholders), stripping any leading command token first. The
// raw path never enters the composer text — ExpandValue substitutes it back at
// submit time, where the existing path→attachment pipeline picks it up. This
// is what keeps a WSL/Linux path ("/mnt/c/…") from ever being routed as a
// slash command (observed live) and keeps the sentence readable.
func (m *uiModel) attachClipboardImage(path, stripPrefix string) {
	if stripPrefix != "" {
		cur := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(m.editor.Value()), stripPrefix))
		m.editor.SetValue(cur)
	}
	token := m.editor.AttachImage(path)
	m.addMessage("assistant", "📎 Image attached from the clipboard as "+token+". Add your question and press Enter to send (delete the token to detach).")
}

// handleCapture saves the last recorded turn as a replayable eval case (the
// "friction → permanent regression test" button). Requires the flight recorder
// (SELFMIND_FLIGHT_RECORDER=1).
func (m *uiModel) handleCapture(args []string) tea.Cmd {
	title := strings.TrimSpace(strings.Join(args, " "))
	res, err := selfeval.CaptureFromFlight("latest", selfeval.CaptureOptions{Title: title})
	if err != nil {
		m.addMessage("assistant", "Capture failed: "+err.Error())
		return nil
	}
	m.addMessage("assistant", fmt.Sprintf(
		"📌 Saved as a regression case: %s\n  case: %s\n  cassettes: %d\nNext: edit the file to add `assert_state` (what SHOULD have happened), then replay offline with `selfmind selfcheck`.",
		res.CaseID, res.CasePath, res.Cassettes))
	return nil
}

// handleMode shows or sets the codex-style approval mode for this session.
// An unset session mode ("") is deliberate: requests then OMIT approval_mode,
// so the gateway resolves the person's persisted /mode preference (set from
// any endpoint, e.g. IM) instead of this terminal shadowing it forever.
func (m *uiModel) handleMode(args []string) tea.Cmd {
	if len(args) == 0 {
		current := m.approvalMode
		if current == "" {
			current = "not set this session (your saved /mode preference applies; on-request if none)"
		}
		m.addMessage("assistant", fmt.Sprintf(
			"Approval mode: %s\n\n  on-request  ask only on risky ops\n  read-only   ask before any file write or command\n  auto-edit   auto-apply in-workspace edits; ask before commands\n  full-auto   run everything without asking (hard-floor safety limits still apply)\n  smart       auto-run clearly safe risky ops after LLM triage; ask otherwise (default)\n\nUsage: /mode <on-request|read-only|auto-edit|full-auto|smart>",
			m.effectiveApprovalMode()))
		return nil
	}
	mode := string(tools.NormalizeApprovalMode(args[0]))
	m.approvalMode = mode
	m.addMessage("assistant", "Approval mode set to: "+mode+" (this session's messages send it explicitly; it overrides your saved /mode preference)")
	return nil
}

func (m *uiModel) handleCurator(args []string) tea.Cmd {
	return func() tea.Msg {
		action := "status"
		if len(args) > 0 {
			action = args[0]
		}
		switch action {
		case "status":
			resp, err := tools.CuratorStatusForTenant(m.tenantID)
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Curator status error: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "run":
			opts := tools.CuratorOptions{StaleAfterDays: 30, ArchiveAfterDays: 90}
			for _, arg := range args[1:] {
				switch arg {
				case "--dry-run", "dry-run":
					opts.DryRun = true
				case "--report", "report":
					opts.WriteReport = true
				}
			}
			resp, err := tools.RunCuratorForTenantWithOptions(m.tenantID, opts)
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Curator run error: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "restore":
			if len(args) < 2 {
				return MsgAgentDone{Response: "Usage: /curator restore <skill-name>"}
			}
			resp, err := tools.RestoreSkillForTenant(m.tenantID, args[1])
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Curator restore error: %v", err)}
			}
			_, _ = m.dispatch("skill_manage", map[string]interface{}{"action": "reload", "_tenant_id": m.tenantID})
			return MsgAgentDone{Response: resp}
		default:
			return MsgAgentDone{Response: "Usage: /curator [status|run [--dry-run] [--report]|restore <skill-name>]"}
		}
	}
}

func (m *uiModel) handleModelSwitch(modelName string) tea.Cmd {
	return func() tea.Msg {
		if m.agent == nil {
			if m.clientMode {
				return MsgAgentDone{Response: "Runtime model switching isn't available in daemon-client mode yet (it mutates the daemon's agent). Set the model via config / `selfmind model set` and restart the daemon."}
			}
			return MsgAgentDone{Response: "Agent not initialized."}
		}
		oldModel := m.agent.CurrentModel()
		if ok := m.agent.SwitchModel(modelName); !ok {
			return MsgAgentDone{Response: fmt.Sprintf("Provider does not support runtime model switching. Current model: %s", oldModel)}
		}
		m.modelName = modelName
		m.providerName = modelName
		return MsgAgentDone{Response: fmt.Sprintf("Model switched: %s → %s", oldModel, modelName)}
	}
}

func (m *uiModel) handleCheckpoint(args []string) tea.Cmd {
	action := args[0]
	name := ""
	if len(args) > 1 {
		name = args[1]
	}
	return func() tea.Msg {
		resp, err := m.dispatch("checkpoint", map[string]interface{}{
			"action":     action,
			"name":       name,
			"_tenant_id": m.tenantID,
		})
		if err != nil {
			return MsgAgentDone{Response: fmt.Sprintf("Checkpoint error: %v", err)}
		}
		return MsgAgentDone{Response: resp}
	}
}
