package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"selfmind/internal/app"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel"
	"selfmind/internal/tools"
)

func (m *uiModel) handleCommand(input string) tea.Cmd {
	parts := strings.Fields(input)
	m.editor.Reset()
	if len(parts) == 0 {
		return nil
	}
	if cmd, ok := slashCommandIndex[parts[0]]; ok {
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
			resp, err := m.agent.Dispatcher().Dispatch("skill_manage", map[string]interface{}{"action": "history", "name": args[1], "_tenant_id": m.tenantID})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Error loading skill history: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "undo":
			if len(args) < 2 {
				return MsgAgentDone{Response: "Usage: /skills undo <change_id>"}
			}
			resp, err := m.agent.Dispatcher().Dispatch("skill_manage", map[string]interface{}{"action": "undo", "change_id": args[1], "_tenant_id": m.tenantID})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Error undoing skill change: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "catalog":
			resp, err := m.agent.Dispatcher().Dispatch("skill_catalog", map[string]interface{}{"action": "list", "_tenant_id": m.tenantID})
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
			resp, err := m.agent.Dispatcher().Dispatch("skill_catalog", map[string]interface{}{
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
			resp, err := m.agent.Dispatcher().Dispatch("skill_catalog", map[string]interface{}{"action": "audit", "name": auditName, "_tenant_id": m.tenantID})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Skill audit error: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "delete":
			if len(args) < 2 {
				return MsgAgentDone{Response: "Usage: /skills delete <name>"}
			}
			resp, err := m.agent.Dispatcher().Dispatch("skill_manage", map[string]interface{}{"action": "delete", "name": args[1], "_tenant_id": m.tenantID})
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
			resp, err := m.agent.Dispatcher().Dispatch("skill_manage", map[string]interface{}{"action": action, "name": args[1], "_tenant_id": m.tenantID})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Error updating pin: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "stats":
			if m.agent == nil || m.agent.Memory() == nil {
				return MsgAgentDone{Response: "Memory not initialized."}
			}
			store := kernel.NewSkillStore(m.agent.Memory())
			stats, err := store.FormatStats(context.Background(), m.tenantID)
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Error loading skill stats: %v", err)}
			}
			return MsgAgentDone{Response: stats}
		case "reload":
			resp, err := m.agent.Dispatcher().Dispatch("skill_manage", map[string]interface{}{"action": "reload", "_tenant_id": m.tenantID})
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
		resp, err := m.agent.Dispatcher().Dispatch("skill_manage", map[string]interface{}{"action": "reload", "_tenant_id": m.tenantID})
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
			resp, err := m.agent.Dispatcher().Dispatch("skill_bundle", map[string]interface{}{"action": "list", "_tenant_id": m.tenantID})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Bundle list error: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "view", "read":
			if len(args) < 2 {
				return MsgAgentDone{Response: "Usage: /bundles view <name>"}
			}
			resp, err := m.agent.Dispatcher().Dispatch("skill_bundle", map[string]interface{}{"action": "read", "name": args[1], "_tenant_id": m.tenantID})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Bundle read error: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		case "create":
			if len(args) < 3 {
				return MsgAgentDone{Response: "Usage: /bundles create <name> <skill1,skill2,...>"}
			}
			resp, err := m.agent.Dispatcher().Dispatch("skill_bundle", map[string]interface{}{
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
			resp, err := m.agent.Dispatcher().Dispatch("skill_bundle", map[string]interface{}{"action": "delete", "name": args[1], "_tenant_id": m.tenantID})
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
		if m.agent == nil || m.agent.Memory() == nil {
			return MsgAgentDone{Response: "Memory not initialized."}
		}
		action := "list"
		if len(args) > 0 {
			action = args[0]
		}
		mem := m.agent.Memory()
		switch action {
		case "list":
			userFacts, _ := mem.GetFacts(context.Background(), m.tenantID, "user")
			memFacts, _ := mem.GetFacts(context.Background(), m.tenantID, "memory")
			var sb strings.Builder
			sb.WriteString("## Memory\n\n### User\n")
			if len(userFacts) == 0 {
				sb.WriteString("- (empty)\n")
			}
			for _, f := range userFacts {
				sb.WriteString(fmt.Sprintf("- `%s` %s\n", f.ID, f.Content))
			}
			sb.WriteString("\n### Project / Environment\n")
			if len(memFacts) == 0 {
				sb.WriteString("- (empty)\n")
			}
			for _, f := range memFacts {
				sb.WriteString(fmt.Sprintf("- `%s` %s\n", f.ID, f.Content))
			}
			sb.WriteString("\nCommands: /memory history [user|memory], /memory remove <user|memory> <id-or-text>, /memory undo <change_id>")
			return MsgAgentDone{Response: sb.String()}
		case "history":
			target := ""
			if len(args) >= 2 {
				target = args[1]
			}
			resp, err := m.agent.Dispatcher().Dispatch("memory", map[string]interface{}{"action": "history", "target": target, "_tenant_id": m.tenantID})
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
			resp, err := m.agent.Dispatcher().Dispatch("memory", map[string]interface{}{
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
			resp, err := m.agent.Dispatcher().Dispatch("memory", map[string]interface{}{
				"action":     "undo",
				"change_id":  args[1],
				"_tenant_id": m.tenantID,
			})
			if err != nil {
				return MsgAgentDone{Response: fmt.Sprintf("Memory undo error: %v", err)}
			}
			return MsgAgentDone{Response: resp}
		default:
			return MsgAgentDone{Response: "Usage: /memory [list|history [user|memory]|remove <user|memory> <id-or-text>|undo <change_id>]"}
		}
	}
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
			_, _ = m.agent.Dispatcher().Dispatch("skill_manage", map[string]interface{}{"action": "reload", "_tenant_id": m.tenantID})
			return MsgAgentDone{Response: resp}
		default:
			return MsgAgentDone{Response: "Usage: /curator [status|run [--dry-run] [--report]|restore <skill-name>]"}
		}
	}
}

func (m *uiModel) handleModelSwitch(modelName string) tea.Cmd {
	return func() tea.Msg {
		if m.agent == nil {
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
		resp, err := m.agent.Dispatcher().Dispatch("checkpoint", map[string]interface{}{
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
