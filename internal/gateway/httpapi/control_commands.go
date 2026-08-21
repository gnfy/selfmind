package httpapi

import (
	"context"
	"fmt"
	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/command"
	"selfmind/internal/platform/textutil"
	"selfmind/internal/tools"
	"strconv"
	"strings"
	"time"
)

func (d *Server) tryHandleControlCommand(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest) (bool, string, error) {
	trimmed := strings.TrimSpace(req.Content)
	lower := strings.ToLower(trimmed)
	// Conversational approval: a bare "y"/"n" (or 好/可以/不行 …) answers a
	// pending approval without the /approve ceremony, so IM feels like asking
	// a human assistant. Only claimed when an approval is actually pending —
	// otherwise the word falls through to the agent (and to the continuation
	// cue handling for "ok"/"可以"). Runs before the "/" gate below.
	if handled, reply, err := d.tryHandleBareApprovalReply(ctx, identity, trimmed, req.Channel); handled {
		return true, reply, err
	}
	// Pending question: a plain (non-slash) reply while a clarify_requests row is
	// pending IS the answer (G3) — resolve it here, above the new-task/queue
	// logic, so a blocking run gets its answer instead of the reply being queued
	// or steered. Runs after the bare y/n approval leg (which wins for y/n-looking
	// input) and before the "/" gate (slash commands are never answers).
	if handled, reply, err := d.tryHandleClarifyAnswer(ctx, identity, trimmed, req.Channel); handled {
		return true, reply, err
	}
	// Command-shaped tokens only: a "/"-leading file path ("/mnt/c/pic.png …")
	// is ordinary message text and must fall through to the agent-first path,
	// never into the command switch or the near-miss suggester.
	if !command.LooksLikeCommand(lower) {
		return false, "", nil
	}
	switch {
	case lower == "/help":
		// Canonical gateway help comes from the shared command registry so the
		// help text, the switch below, and every other endpoint cannot drift.
		return true, command.HelpText(), nil
	case lower == "/model":
		if d != nil && d.Gateway != nil {
			return true, d.Gateway.ModelStatusReply(), nil
		}
		return true, "SelfMind is running, but the model gateway is not configured.", nil
	case lower == "/id":
		return true, formatIdentity(identity), nil
	case lower == "/stop":
		active := d.coordinator().stopActive(identity.PersonID)
		if active == nil {
			// No live run — but a task can be stuck non-terminal
			// (in_progress/blocked/running) with no run behind it (e.g. a run
			// that finalized without terminalizing its task, or a task created
			// but never executed). /stop should still let the user terminate
			// it, otherwise it sits in /tasks forever with no way to clear it.
			return true, d.cancelStuckCurrentTask(ctx, identity), nil
		}
		if active.RunID != "" {
			_ = d.Control.RequestRunCancel(context.Background(), identity.TenantID, active.RunID)
		}
		if active.TaskID != "" && active.RunID != "" {
			// Cancellation is a request until the execution body actually exits.
			// The run goroutine owns terminal materialization; declaring cancelled
			// here used to let a non-cooperative tool keep running after the DB said
			// it was gone, and could start queued work against the same workspace.
			_, _ = d.Control.AppendEvent(context.Background(), control.Event{
				TaskID:     active.TaskID,
				RunID:      active.RunID,
				Type:       "run.cancel_requested",
				Visibility: "task",
				Channel:    req.Channel,
				Payload:    mustJSON(map[string]string{"reason": "user requested stop"}),
			})
		}
		reply := fmt.Sprintf("Stopping run %s.", fallback(active.RunID, "(starting)"))
		// Queued work auto-starts when this run finalizes (the G1+G2 drain
		// contract). Say so, or the next task popping up right after a /stop
		// reads as "stop did nothing" (observed live).
		if n, err := d.Control.CountQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued); err == nil && n > 0 {
			reply += fmt.Sprintf(" %d queued task(s) will start next — /queue clear to drop them.", n)
		}
		return true, reply, nil
	case strings.HasPrefix(lower, "/cancel"):
		// Explicit "terminate a stuck task" — same as /stop's no-run fallback,
		// but a clearer verb for a task that is parked rather than running.
		return true, d.cancelStuckCurrentTask(ctx, identity), nil
	case strings.HasPrefix(lower, "/new"):
		title := strings.TrimSpace(trimmed[len("/new"):])
		if title == "" {
			title = "New task"
		}
		workspaceID := req.WorkspaceID
		if workspaceID == "" {
			if ws, _ := d.Control.CurrentWorkspace(ctx, identity.TenantID, identity.PersonID); ws != nil {
				workspaceID = ws.ID
			}
		}
		task, err := d.Control.CreateTask(ctx, control.TaskCreate{
			TenantID:    identity.TenantID,
			PersonID:    identity.PersonID,
			WorkspaceID: workspaceID,
			Title:       title,
			Channel:     req.Channel,
		})
		if err != nil {
			return true, "", err
		}
		return true, fmt.Sprintf("Created task: %s (%s)", task.Title, task.ID), nil
	case lower == "/task status" || lower == "/status":
		reply, err := d.statusReply(ctx, identity)
		return true, reply, err
	case strings.HasPrefix(lower, "/resume "):
		parts := strings.Fields(trimmed)
		if len(parts) < 2 {
			return true, "Usage: /resume <n|task_id>", nil
		}
		// Same reference grammar as /task: list ordinal (/tasks card order),
		// full id, or the short card-displayed prefix — the id the /tasks card
		// shows MUST round-trip through /resume.
		task, userErr, err := d.resolveTaskReference(ctx, identity, parts[1])
		if err != nil {
			return true, "", err
		}
		if userErr != "" {
			return true, userErr, nil
		}
		if err := d.Control.SetCurrentTask(ctx, identity.TenantID, identity.PersonID, task.ID); err != nil {
			return true, "", err
		}
		// One-shot pin: an explicit /resume IS continuation evidence, so the
		// next agent-bound message attaches to this task even without a
		// continuation cue. Consumed by resolveTask; after that, plain new
		// messages fall back to the pre-label guess again. /resume of an
		// ARCHIVED label deliberately reopens it (resolveTask honors the pin).
		_ = d.Control.SetPersonSetting(ctx, identity.TenantID, identity.PersonID, resumePinKey, task.ID)
		// State the workspace binding explicitly: the client's status bar keeps
		// showing its launch cwd until the next agent turn, so without this
		// line a successful /resume reads as "didn't work" (observed live).
		return true, "Resumed task: " + task.Title + " (" + task.ID + ")" + d.resumeWorkspaceNote(ctx, identity, task), nil
	case lower == "/task" || strings.HasPrefix(lower, "/task "):
		// /task <id> detail + subcommands (runs|rename|archive). "/task status"
		// is an alias of /status and is claimed by the case above.
		reply, err := d.taskCommandReply(ctx, identity, strings.Fields(trimmed)[1:])
		return true, reply, err
	case lower == "/tasks" || strings.HasPrefix(lower, "/tasks ") || lower == "tasks" || strings.Contains(lower, "任务列表"):
		variant := ""
		if strings.HasPrefix(lower, "/tasks ") {
			variant = strings.TrimSpace(strings.TrimPrefix(lower, "/tasks"))
		}
		reply, err := d.tasksOverviewReply(ctx, identity, variant)
		return true, reply, err
	case lower == "/queue" || strings.HasPrefix(lower, "/queue "):
		arg := strings.TrimSpace(trimmed[len("/queue"):])
		if strings.EqualFold(arg, "clear") {
			n, err := d.Control.ClearQueued(ctx, identity.TenantID, identity.PersonID)
			if err != nil {
				return true, "", err
			}
			return true, fmt.Sprintf("Cleared %d queued task(s).", n), nil
		}
		queued, err := d.Control.ListQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued)
		if err != nil {
			return true, "", err
		}
		// `/queue drop <n>` removes one item by its list position (same
		// ordering as the /queue listing), so a single unwanted task can be
		// dropped without clearing the whole queue.
		if rest := strings.TrimSpace(strings.TrimPrefix(strings.ToLower(arg), "drop")); arg != "" && strings.HasPrefix(strings.ToLower(arg), "drop") {
			n, convErr := strconv.Atoi(strings.TrimSpace(rest))
			if convErr != nil || n < 1 || n > len(queued) {
				return true, fmt.Sprintf("Usage: /queue drop <n> (1-%d). Run /queue to see positions.", len(queued)), nil
			}
			target := queued[n-1]
			if err := d.Control.MarkQueued(ctx, identity.TenantID, target.ID, control.QueueStatusCancelled); err != nil {
				return true, "", err
			}
			return true, "Dropped queued task: " + textutil.Truncate(toOneLine(target.Content), 60), nil
		}
		return true, formatQueue(queued), nil
	case lower == "/watchers" || strings.HasPrefix(lower, "/watchers "):
		reply, err := d.watchersCommandReply(ctx, identity, strings.Fields(trimmed)[1:])
		return true, reply, err
	case lower == "/diag memory":
		reply, err := d.memoryDiagReply(ctx, identity)
		return true, reply, err
	case lower == "/diag context":
		reply, err := d.contextDiagReply(ctx, identity)
		return true, reply, err
	case lower == "/diag tasks":
		reply, err := d.tasksDiagReply(ctx, identity)
		return true, reply, err
	case lower == "/diag models":
		reply, err := d.modelsDiagReply(ctx, identity)
		return true, reply, err
	case lower == "/diag execution":
		reply, err := d.executionDiagReply(ctx, identity)
		return true, reply, err
	case lower == "/diag tools":
		reply, err := d.toolsDiagReply(ctx, identity)
		return true, reply, err
	case lower == "/diag delivery recover stale-results":
		reply, err := d.recoverStaleDeliveryResultsReply(ctx, identity, req)
		return true, reply, err
	case lower == "/diag delivery dismiss stale-results":
		reply, err := d.dismissStaleDeliveryResultsReply(ctx, identity, req)
		return true, reply, err
	case strings.HasPrefix(lower, "/diag delivery retry "):
		ref := strings.TrimSpace(trimmed[len("/diag delivery retry "):])
		reply, err := d.retryDeliveryReply(ctx, identity, req, ref)
		return true, reply, err
	case strings.HasPrefix(lower, "/diag delivery dismiss "):
		ref := strings.TrimSpace(trimmed[len("/diag delivery dismiss "):])
		reply, err := d.dismissDeliveryReply(ctx, identity, req, ref)
		return true, reply, err
	case lower == "/diag delivery":
		reply, err := d.deliveryDiagReply(ctx, identity)
		return true, reply, err
	case lower == "/diag":
		reply, err := d.diagReply(ctx, identity)
		return true, reply, err
	case lower == "/report":
		return true, "Usage: /report daily [--since 24h]", nil
	case lower == "/report daily" || strings.HasPrefix(lower, "/report daily "):
		window, err := parseDailyReportWindow(trimmed)
		if err != nil {
			return true, "Usage: /report daily [--since 24h]", nil
		}
		reply, err := d.dailyQualityReport(ctx, identity, window)
		return true, reply, err
	case strings.HasPrefix(lower, "/workspace ") || strings.HasPrefix(lower, "/ws "):
		// Unified workspace verb: WITH an argument it selects; bare it lists
		// (handled by the case below). "/ws" is the short alias and
		// "/workspaces" the plural spelling — all three behave identically.
		parts := strings.Fields(req.Content)
		if len(parts) < 2 {
			return true, "Usage: /workspace <n|workspace_id>", nil
		}
		ws, userErr, err := d.resolveWorkspaceReference(ctx, identity, parts[1])
		if err != nil {
			return true, "", err
		}
		if userErr != "" {
			return true, userErr, nil
		}
		if err := d.Control.SetCurrentWorkspace(ctx, identity.TenantID, identity.PersonID, ws.ID); err != nil {
			return true, "", err
		}
		if !isLocalCLIRequest(req) {
			return true, formatWorkspaceSwitchForIM(ws), nil
		}
		return true, fmt.Sprintf("Current workspace: %s (%s)\n%s", ws.Name, ws.ID, ws.LocalPath), nil
	case lower == "/workspaces" || lower == "/workspace" || lower == "/ws":
		workspaces, err := d.listWorkspacesForDisplay(ctx, identity)
		if err != nil {
			return true, "", err
		}
		currentID := ""
		if current, _ := d.Control.CurrentWorkspace(ctx, identity.TenantID, identity.PersonID); current != nil {
			currentID = current.ID
		}
		if !isLocalCLIRequest(req) {
			return true, formatWorkspacesForIM(workspaces, currentID), nil
		}
		return true, formatWorkspaces(workspaces, currentID), nil
	case lower == "/approvals" || strings.HasPrefix(lower, "/approvals "):
		rest := strings.TrimSpace(trimmed[len("/approvals"):])
		if rest != "" {
			// Remembered classes are user-owned state, so they must be listable
			// and revocable. Without this surface ten person-scope host grants
			// accumulated in a single day with nothing able to show or withdraw
			// them.
			return true, d.approvalGrantsReply(ctx, identity, rest), nil
		}
		approvals, titles, err := d.pendingApprovalsForDisplay(ctx, identity)
		if err != nil {
			return true, "", err
		}
		return true, formatApprovals(approvals, titles), nil
	case lower == "/notify" || strings.HasPrefix(lower, "/notify "):
		reply, err := d.notifyPreferenceReply(ctx, identity, strings.TrimSpace(trimmed[len("/notify"):]))
		return true, reply, err
	case lower == "/mode" || strings.HasPrefix(lower, "/mode "):
		reply, err := d.approvalModeReply(ctx, identity, strings.TrimSpace(trimmed[len("/mode"):]))
		return true, reply, err
	case lower == "/approve" || strings.HasPrefix(lower, "/approve "):
		return true, d.respondApprovalCommand(ctx, identity, strings.TrimSpace(trimmed[len("/approve"):]), "approved", req.Channel), nil
	case lower == "/reject" || strings.HasPrefix(lower, "/reject "):
		return true, d.respondApprovalCommand(ctx, identity, strings.TrimSpace(trimmed[len("/reject"):]), "rejected", req.Channel), nil
	case lower == "/events":
		task, err := d.Control.CurrentTask(ctx, identity.TenantID, identity.PersonID)
		if err != nil {
			return true, "", err
		}
		if task == nil {
			return true, "No active task.", nil
		}
		events, err := d.Control.ListTaskEvents(ctx, task.ID, 20)
		if err != nil {
			return true, "", err
		}
		return true, formatEvents(events), nil
	case lower == "/task status" || lower == "task status" || lower == "/status" || lower == "status" || strings.Contains(lower, "进度"):
		reply, err := d.statusReply(ctx, identity)
		return true, reply, err
	default:
		// Near-miss typo help: "/approves" → suggest "/approvals". Only claim
		// the message when the token is close to a KNOWN control command —
		// unknown slashes may be skill invocations or agent input and must
		// keep flowing through unchanged.
		if suggestion := suggestControlCommand(lower); suggestion != "" {
			return true, fmt.Sprintf("Unknown command %s — did you mean %s?", strings.Fields(lower)[0], suggestion), nil
		}
		return false, "", nil
	}
}

// notifyPreferenceReply handles /notify: show, set, or reset the person's
// preferred notify endpoint for detached CLI-origin pushes. Setting a concrete
// platform is validated against the person's OWN bound accounts — this bound
// check is a security boundary (never an arbitrary push target), not a
// convenience (docs/identity-continuity.md conversation-layer rule 1).
func (d *Server) notifyPreferenceReply(ctx context.Context, identity *control.IdentityContext, arg string) (string, error) {
	arg = strings.ToLower(strings.TrimSpace(arg))
	if arg == "" {
		current, err := d.Control.GetPersonSetting(ctx, identity.TenantID, identity.PersonID, personSettingNotifyPlatform)
		if err != nil {
			return "", err
		}
		if current == "" {
			current = "auto (most recently active IM account)"
		}
		if current == "off" {
			current = "off (detached notifications disabled)"
		}
		surface, err := d.Control.GetPersonSetting(ctx, identity.TenantID, identity.PersonID, personSettingApprovalSurface)
		if err != nil {
			return "", err
		}
		if surface == "" {
			surface = "desk-first"
		}
		return "Notify preference: " + current + "\nApproval surface: " + surface + "\nUsage: /notify <on|off|auto|platform|desk-first|phone-first>", nil
	}
	if arg == "desk-first" || arg == "phone-first" {
		value := arg
		if value == "desk-first" {
			value = ""
		}
		if err := d.Control.SetPersonSetting(ctx, identity.TenantID, identity.PersonID, personSettingApprovalSurface, value); err != nil {
			return "", err
		}
		if arg == "phone-first" {
			return "Approval surface set to phone-first. CLI-origin approvals will also go immediately to the preferred IM endpoint.", nil
		}
		return "Approval surface set to desk-first. CLI-origin approvals escalate to IM only after the CLI detaches or T1 elapses.", nil
	}
	if arg == "on" || arg == "auto" {
		if err := d.Control.SetPersonSetting(ctx, identity.TenantID, identity.PersonID, personSettingNotifyPlatform, ""); err != nil {
			return "", err
		}
		if arg == "on" {
			return "Notifications enabled (most recently active IM account).", nil
		}
		return "Notify preference set to auto (most recently active IM account).", nil
	}
	if arg == "off" {
		if err := d.Control.SetPersonSetting(ctx, identity.TenantID, identity.PersonID, personSettingNotifyPlatform, "off"); err != nil {
			return "", err
		}
		return "Detached notifications disabled. Direct replies in this chat are unchanged.", nil
	}
	accounts, err := d.Control.ListAccountsByPerson(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		return "", err
	}
	var bound []string
	valid := false
	for _, account := range accounts {
		if account.Platform == "cli" {
			continue
		}
		bound = append(bound, account.Platform)
		if account.Platform == arg {
			valid = true
		}
	}
	if !valid {
		if len(bound) == 0 {
			return "You have no bound IM accounts yet, so there is nothing to notify. Bind an account first.", nil
		}
		return fmt.Sprintf("%s is not one of your bound IM accounts (bound: %s). Use /notify <on|off|auto|platform|desk-first|phone-first>.", arg, strings.Join(bound, ", ")), nil
	}
	if err := d.Control.SetPersonSetting(ctx, identity.TenantID, identity.PersonID, personSettingNotifyPlatform, arg); err != nil {
		return "", err
	}
	return "Notify preference set to " + arg + ".", nil
}

// approvalModeReply handles /mode from any channel: show the person's current
// approval mode, or persist a new one (per person via person_settings). The
// persisted mode is applied when a later request carries no explicit per-request
// mode (see installExecutionScope). full-auto gets a warning that the hard-floor
// safety deny set still applies — it is not blocked, only flagged.
func (d *Server) approvalModeReply(ctx context.Context, identity *control.IdentityContext, arg string) (string, error) {
	const usage = "Usage: /mode <on-request|read-only|auto-edit|full-auto|smart>"
	arg = strings.TrimSpace(arg)
	if arg == "" {
		current, err := d.Control.GetPersonSetting(ctx, identity.TenantID, identity.PersonID, personSettingApprovalMode)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(current) == "" {
			current = string(tools.DefaultApprovalMode) + " (default)"
		}
		return "Approval mode: " + current + "\n" + usage, nil
	}
	// Reject unknown words instead of silently defaulting: a typo'd mode should
	// not quietly leave the person on on-request thinking they set full-auto.
	if tools.NormalizeApprovalMode(arg) == tools.ApprovalOnRequest && !tools.IsKnownApprovalModeWord(arg) {
		return "Unknown mode " + arg + ".\n" + usage, nil
	}
	normalized := tools.NormalizeApprovalMode(arg)
	mode := string(normalized)
	if err := d.Control.SetPersonSetting(ctx, identity.TenantID, identity.PersonID, personSettingApprovalMode, mode); err != nil {
		return "", err
	}
	reply := "Approval mode set to " + mode + "."
	// Re-evaluate approvals that were ALREADY pending under the new mode. Without
	// this a run blocked on a human ask before the switch stays blocked forever
	// (observed live: a read_file approval sat pending for minutes after /mode
	// smart) — the live ModeGetter only governs the NEXT dangerous op. The retro
	// pass NEVER bypasses the hard floor and fails safe (leaves pending) on any
	// uncertainty.
	reply += d.retroResolvePendingApprovals(ctx, identity, normalized)
	if mode == string(tools.ApprovalFullAuto) {
		reply += " Note: the hard-floor safety limits still apply (filesystem-root deletes, disk formatting, host shutdown, and similar are always blocked)."
	}
	return reply, nil
}

// retroResolvePendingApprovals re-checks the person's currently-pending
// approvals under a freshly-set approval mode and settles the ones the mode can
// decide on its own, so switching to smart/full-auto/auto-edit unblocks a run
// that was already stuck on a human ask. It mirrors the middleware funnel via
// tools.EvaluateModeDecision (hard floor authoritative; smart mode consults the
// same judge), only touches this person's pending rows, and returns a compact
// English summary suffix for the /mode reply (empty when there was nothing to
// re-check). Auto-approve/deny flip the pending row so the blocked waiter wakes
// on its next 1s poll (server.go ~812); no new wakeup channel is needed.
func (d *Server) retroResolvePendingApprovals(ctx context.Context, identity *control.IdentityContext, mode tools.ApprovalMode) string {
	// on-request / read-only still ask for everything they gate, so there is
	// nothing to auto-settle — leave the pending rows untouched and say nothing.
	if mode == tools.ApprovalOnRequest || mode == tools.ApprovalReadOnly {
		return ""
	}
	pending, err := d.Control.ListApprovalRequests(ctx, identity.TenantID, identity.PersonID, "pending", 100)
	if err != nil || len(pending) == 0 {
		return ""
	}
	approved, denied, stillPending := 0, 0, 0
	for _, ap := range pending {
		p := decodeApprovalPayload(ap)
		toolName := strings.TrimSpace(p.Tool)
		if toolName == "" {
			// Non-tool approval (no tool to classify): fail safe, leave pending.
			stillPending++
			continue
		}
		if strings.EqualFold(strings.TrimSpace(p.DecisionPolicy), tools.ApprovalDecisionPolicyOnceOnly) {
			stillPending++
			continue
		}
		decision := tools.EvaluateModeDecision(ctx, mode, "", toolName, p.Args, p.Reason, approvalReasonIsDangerous(p.Reason), d.ApprovalJudge)
		switch decision {
		case tools.ModeApprove:
			// Internal channel "mode-change", empty grant scope (a retro approval
			// is a one-off, it records no class grant).
			if _, err := d.respondApprovalByToken(ctx, identity, ap.ID, "approved", "mode-change", control.ApprovalDecisionInput{}); err == nil {
				approved++
				d.appendApprovalModeEvent(ctx, ap, "approval.auto_approved", string(mode))
			} else {
				stillPending++
			}
		case tools.ModeDeny:
			if _, err := d.respondApprovalByToken(ctx, identity, ap.ID, "rejected", "mode-change", control.ApprovalDecisionInput{}); err == nil {
				denied++
				d.appendApprovalModeEvent(ctx, ap, "approval.auto_rejected", string(mode))
			} else {
				stillPending++
			}
		default:
			stillPending++
		}
	}
	if approved == 0 && denied == 0 && stillPending == 0 {
		return ""
	}
	total := approved + denied + stillPending
	var parts []string
	if approved > 0 {
		parts = append(parts, fmt.Sprintf("%d auto-approved", approved))
	}
	if denied > 0 {
		parts = append(parts, fmt.Sprintf("%d blocked by safety triage", denied))
	}
	if stillPending > 0 {
		parts = append(parts, fmt.Sprintf("%d still needs your y/n", stillPending))
	}
	return fmt.Sprintf(" Re-checked %s: %s.", pluralize(total, "pending approval"), strings.Join(parts, ", "))
}

// approvalReasonIsDangerous reports whether a stored approval reason reflects the
// dangerous-op heuristic (destructive command, restricted/out-of-workspace path)
// rather than a pure mode requirement ("… requires approval in read-only mode").
// It is used as EvaluateModeDecision's dangerousHint so the retro pass preserves
// the ORIGINAL danger signal even though it recomputes without the run's
// projectRoot — it can only raise danger, never downgrade it.
func approvalReasonIsDangerous(reason string) bool {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return false
	}
	return !strings.Contains(reason, "requires approval in")
}

// statusReply builds the /status card shared by every channel's control-command
// path. When the person has an active run, its task is what the user is
// waiting on, so report it first (an async run may attach to a task the
// current_task pointer has not caught up with yet); fall back to the
// per-person current_task pointer otherwise. The card format itself
// (formatTaskStatus, `Task:` / `Status:` markers) is a stable contract pinned
// by the continuity eval suite — change it there, not here.
// cancelStuckCurrentTask terminates the person's current task when it is stuck
// in a non-terminal state with no live run (the /stop no-run fallback and the
// /cancel command). It never touches a task that is already terminal. This is
// the user-facing escape hatch for a task that recovery sweeps missed (created
// but never run, or finalized without terminalizing).
func (d *Server) cancelStuckCurrentTask(ctx context.Context, identity *control.IdentityContext) string {
	if d == nil || d.Control == nil || identity == nil {
		return "No active run to stop."
	}
	task, err := d.Control.CurrentTask(ctx, identity.TenantID, identity.PersonID)
	if err != nil || task == nil {
		return "No active run to stop, and no current task to cancel."
	}
	if terminalTaskStatus(task.Status) {
		return "No active run to stop; the current task is already " + task.Status + "."
	}
	if err := d.Control.UpdateTaskStatus(ctx, identity.TenantID, task.ID, "cancelled", "Cancelled by user.", nil); err != nil {
		return "Could not cancel the task: " + err.Error()
	}
	_, _ = d.Control.AppendEvent(ctx, control.Event{
		TaskID:     task.ID,
		Type:       "task.cancelled",
		Visibility: "task",
		Payload:    mustJSON(map[string]string{"reason": "user cancelled a stuck task"}),
	})
	return "No live run was executing, so I cancelled the current task: " + textutil.Truncate(toOneLine(task.Title), 60)
}

func (d *Server) statusReply(ctx context.Context, identity *control.IdentityContext) (string, error) {
	active := d.coordinator().currentActive(identity.PersonID)
	var task *control.Task
	if active != nil && active.TaskID != "" {
		// Best-effort lookup: a missing/errored row falls back to the pointer.
		task, _ = d.Control.GetTask(ctx, identity.TenantID, active.TaskID)
	}
	if task == nil {
		var err error
		task, err = d.Control.CurrentTask(ctx, identity.TenantID, identity.PersonID)
		if err != nil {
			return "", err
		}
	}
	if task == nil {
		return "No active task.", nil
	}
	handoff, _ := d.Control.LatestHandoff(ctx, task.ID)
	plan := d.latestPlanForTask(ctx, task.ID)
	card := formatTaskStatus(task, handoff, active, plan)
	// A run blocked on an approval looks "stuck" unless the card says the run
	// is waiting for the HUMAN (observed live: 15 minutes of staring at
	// "running" while an ls_r approval sat pending). Surface it with the same
	// conversational summary the push uses.
	if pending, err := d.Control.ListApprovalRequests(ctx, identity.TenantID, identity.PersonID, "pending", 5); err == nil && len(pending) > 0 {
		sortApprovalsForDisplay(pending)
		waitAge := humanDuration(time.Since(pending[0].CreatedAt))
		card += "\n⚠ Waiting for your approval — reply y or n:\n"
		titles := d.taskTitlesFor(ctx, identity.TenantID, pending)
		for i, approval := range pending {
			if len(pending) == 1 {
				card += approvalSummaryLine(approval, "") + "\n"
				break
			}
			card += fmt.Sprintf("%d. %s\n", i+1, approvalSummaryLine(approval, titles[approval.TaskID]))
		}
		card = strings.Replace(card, "Waiting for your approval", fmt.Sprintf("Waiting for your approval (%s elapsed)", waitAge), 1)
	}
	// A run blocked on a clarify looks just as "stuck" as one blocked on an
	// approval: surface the pending question(s) so the person knows their reply
	// is what unblocks the run. ListClarifyRequests is already oldest-first, the
	// order tryHandleClarifyAnswer answers in.
	if clarifies, err := d.Control.ListClarifyRequests(ctx, identity.TenantID, identity.PersonID, "pending", 5); err == nil && len(clarifies) > 0 {
		waitAge := humanDuration(time.Since(clarifies[0].CreatedAt))
		card += "\n⚠ Waiting for your answer — just reply with it:\n"
		for i, clarify := range clarifies {
			if len(clarifies) == 1 {
				card += clarifySummaryLine(clarify) + "\n"
				break
			}
			card += fmt.Sprintf("%d. %s\n", i+1, clarifySummaryLine(clarify))
		}
		card = strings.Replace(card, "Waiting for your answer", fmt.Sprintf("Waiting for your answer (%s elapsed)", waitAge), 1)
	}
	return card, nil
}
