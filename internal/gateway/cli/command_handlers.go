package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"selfmind/internal/app"
	selfeval "selfmind/internal/eval"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/command"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/tools"
)

// factProvenance renders a compact, dim "why is this remembered" suffix
// (source / scope / confidence / age) for /memory list (W3e). Legacy facts with
// no metadata get no suffix, so the view stays clean.
func factProvenance(f memory.Fact) string {
	var parts []string
	if f.Source != "" {
		parts = append(parts, "src="+f.Source)
	}
	if f.Scope != "" && f.Scope != "global" {
		parts = append(parts, "scope="+f.Scope)
	}
	if f.Confidence > 0 {
		parts = append(parts, fmt.Sprintf("conf=%.2f", f.Confidence))
	}
	if !f.CreatedAt.IsZero() {
		parts = append(parts, humanizeAge(time.Since(f.CreatedAt)))
	}
	if len(parts) == 0 {
		return ""
	}
	return "  · " + strings.Join(parts, " ")
}

func humanizeAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

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
	if cmd, ok := slashCommandIndex[parts[0]]; ok {
		// In daemon-client mode agent-backed commands route through the tool
		// dispatch seam (m.dispatch → daemon) or through the message processor
		// (/status, /tasks). The few that need the in-process store (/memory
		// list, /skills stats, /model switch) detect client mode themselves and
		// return a clear notice. So no top-level gate is needed for safety.
		return cmd.Run(m, parts[1:])
	}
	if strings.HasPrefix(parts[0], "/") {
		instruction := strings.TrimSpace(strings.TrimPrefix(input, parts[0]))
		return m.handleSkillSlash(input, parts[0], instruction)
	}
	return nil
}

func (m *uiModel) handleSkillSlash(rawInput, slashName, instruction string) tea.Cmd {
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
	m.addMessage("user", rawInput)
	m.statusMsg = fmt.Sprintf("Loaded skill context: %s", displayName)
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

func (m *uiModel) handleTasks() tea.Cmd {
	return func() tea.Msg {
		if m.messageProcessor != nil {
			resp, _ := m.messageProcessor(context.Background(), m.controlMessageRequest("/tasks"))
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

func (m *uiModel) controlMessageRequest(content string) api.MessageRequest {
	return api.MessageRequest{
		TenantID:       m.tenantID,
		Platform:       "cli",
		PlatformUserID: cliPlatformUserID(),
		DisplayName:    cliDisplayName(),
		Channel:        m.channel,
		Content:        content,
		ClientCWD:      currentWorkingDir(),
	}
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
			sb.WriteString("\nCommands: /skills view <name>, /skills history <name>, /skills undo <change_id>, /skills search <query>, /skills archive <name>, /skills pin <name>, /skills stats")
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
			resp, err := tools.ArchiveSkillForTenant(m.tenantID, args[1])
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
					return MsgAgentDone{Response: "`/skills stats` needs the in-process store. Run `SELFMIND_TUI_INPROC=1 selfmind`, or use `/skills list`."}
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
			return MsgAgentDone{Response: "Usage: /skills [list|view|history|undo|search|catalog|install|audit|delete|archive|pin|unpin|stats|reload]"}
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

func (m *uiModel) handleMemory(args []string) tea.Cmd {
	return func() tea.Msg {
		action := "list"
		if len(args) > 0 {
			action = args[0]
		}
		switch action {
		case "list":
			// In client mode there is no in-process store, so list the facts via
			// the daemon (memory tool "list" action). The daemon view omits the
			// synthesized profile, which is an in-process agent concern.
			if m.agent == nil || m.agent.Memory() == nil {
				resp, err := m.dispatch("memory", map[string]interface{}{"action": "list", "_tenant_id": m.tenantID})
				if err != nil {
					return MsgAgentDone{Response: fmt.Sprintf("Memory list error: %v", err)}
				}
				return MsgAgentDone{Response: resp}
			}
			mem := m.agent.Memory()
			userFacts, _ := mem.GetFacts(context.Background(), m.tenantID, "user")
			memFacts, _ := mem.GetFacts(context.Background(), m.tenantID, "memory")
			var sb strings.Builder
			sb.WriteString("## Memory\n\n### User\n")
			if len(userFacts) == 0 {
				sb.WriteString("- (empty)\n")
			}
			for _, f := range userFacts {
				sb.WriteString(fmt.Sprintf("- `%s` %s%s\n", f.ID, f.Content, factProvenance(f)))
			}
			sb.WriteString("\n### Project / Environment\n")
			if len(memFacts) == 0 {
				sb.WriteString("- (empty)\n")
			}
			for _, f := range memFacts {
				sb.WriteString(fmt.Sprintf("- `%s` %s%s\n", f.ID, f.Content, factProvenance(f)))
			}
			pinnedFacts, _ := mem.GetFacts(context.Background(), m.tenantID, "pinned")
			sb.WriteString("\n### Pinned (authoritative — synthesis won't override)\n")
			if len(pinnedFacts) == 0 {
				sb.WriteString("- (empty)\n")
			}
			for _, f := range pinnedFacts {
				sb.WriteString(fmt.Sprintf("- `%s` %s\n", f.ID, f.Content))
			}
			sb.WriteString("\n### Synthesized profile (\"what SelfMind thinks of you\")\n")
			if prof := m.agent.ProfileSummary(context.Background(), m.tenantID); prof != "" {
				sb.WriteString(prof + "\n")
			} else {
				sb.WriteString("- (not synthesized yet)\n")
			}
			sb.WriteString("\nCommands: /memory pin <text> · /memory remove <user|memory|pinned|profile> <id-or-text> · /memory history · /memory undo <change_id>")
			return MsgAgentDone{Response: sb.String()}
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
				return MsgAgentDone{Response: "Usage: /memory pin <authoritative fact>"}
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
		default:
			return MsgAgentDone{Response: "Usage: /memory [list|pin <text>|history [user|memory]|remove <user|memory|pinned|profile> <id-or-text>|undo <change_id>]"}
		}
	}
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
	m.viewport.GotoBottom()
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

// attachClipboardImage drops a saved clipboard-image path into the composer so
// the submit path turns it into an image attachment, stripping any leading
// command token first.
func (m *uiModel) attachClipboardImage(path, stripPrefix string) {
	cur := strings.TrimSpace(m.editor.Value())
	if stripPrefix != "" {
		cur = strings.TrimSpace(strings.TrimPrefix(cur, stripPrefix))
	}
	if cur == "" {
		m.editor.SetValue(path + " ")
	} else {
		m.editor.SetValue(cur + " " + path + " ")
	}
	m.addMessage("assistant", "📎 Image attached from the clipboard. Add your question and press Enter to send.")
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
			"Approval mode: %s\n\n  on-request  ask only on risky ops (default)\n  read-only   ask before any file write or command\n  auto-edit   auto-apply in-workspace edits; ask before commands\n  full-auto   run everything without asking (hard-floor safety limits still apply)\n  smart       auto-run clearly safe risky ops after LLM triage; ask otherwise\n\nUsage: /mode <on-request|read-only|auto-edit|full-auto|smart>",
			current))
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
