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

// tryHandleControlCommand returns whether a control command claimed the message,
// its reply, and — for the commands that select or re-trust a workspace — the
// resulting workspace in typed form so the client never parses the reply prose.
func (d *Server) tryHandleControlCommand(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest) (bool, string, *api.DigestWorkspace, error) {
	trimmed := strings.TrimSpace(req.Content)
	lower := strings.ToLower(trimmed)
	// Conversational approval: a bare "y"/"n" (or 好/可以/不行 …) answers a
	// pending approval without the /approve ceremony, so IM feels like asking
	// a human assistant. Only claimed when an approval is actually pending —
	// otherwise the word falls through to the agent (and to the continuation
	// cue handling for "ok"/"可以"). Runs before the "/" gate below.
	if handled, reply, err := d.tryHandleBareApprovalReply(ctx, identity, trimmed, req.Channel); handled {
		return true, reply, nil, err
	}
	// Pending question: a plain (non-slash) reply while a clarify_requests row is
	// pending IS the answer (G3) — resolve it here, above the new-task/queue
	// logic, so a blocking run gets its answer instead of the reply being queued
	// or steered. Runs after the bare y/n approval leg (which wins for y/n-looking
	// input) and before the "/" gate (slash commands are never answers).
	if handled, reply, err := d.tryHandleClarifyAnswer(ctx, identity, req.ClarifyID, trimmed, req.Channel); handled {
		return true, reply, nil, err
	}
	// Command-shaped tokens only: a "/"-leading file path ("/mnt/c/pic.png …")
	// is ordinary message text and must fall through to the agent-first path,
	// never into the command switch or the near-miss suggester.
	if !command.LooksLikeCommand(lower) {
		return false, "", nil, nil
	}
	switch {
	case lower == "/help":
		// Canonical gateway help comes from the shared command registry so the
		// help text, the switch below, and every other endpoint cannot drift.
		return true, command.HelpText(), nil, nil
	case lower == "/model" || strings.HasPrefix(lower, "/model "):
		reply, err := d.handleModelControl(ctx, req.Channel, trimmed)
		return true, reply, nil, err
	case lower == "/id":
		return true, formatIdentity(identity), nil, nil
	case lower == "/remember" || strings.HasPrefix(lower, "/remember "):
		reply, err := d.handleRememberCommand(ctx, identity, strings.TrimSpace(trimmed[len("/remember"):]))
		return true, reply, nil, err
	case lower == "/forget" || strings.HasPrefix(lower, "/forget "):
		reply, err := d.handleForgetCommand(ctx, identity, strings.TrimSpace(trimmed[len("/forget"):]))
		return true, reply, nil, err
	case strings.HasPrefix(lower, "/stop "):
		// `/stop <n>` clears ONE attention item without running it. Dismissing
		// used to require pinning the item first — `/resume <n>` then `/stop` —
		// which starts the work you were trying to put down. Stale items
		// therefore had no exit: automatic retention will not touch anything
		// with pending human input, so `waiting_user` residue accumulated (nine
		// items, six of them a day old, observed 2026-09-04).
		parts := strings.Fields(trimmed)
		if len(parts) < 2 {
			return true, "Usage: /stop <n|run_id>  (bare /stop cancels the active run)", nil, nil
		}
		return true, d.dismissAttentionByReference(ctx, identity, req.Channel, parts[1]), nil, nil
	case lower == "/stop":
		active := d.coordinator().stopActive(identity.PersonID)
		if active == nil {
			return true, d.dismissCurrentAttention(ctx, identity), nil, nil
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
		return true, reply, nil, nil
	case strings.HasPrefix(lower, "/cancel"):
		// With no live execution, cancel is the compatibility spelling for
		// dismissing the one unambiguous Attention item. It never rewrites a
		// historical Run outcome.
		return true, d.dismissCurrentAttention(ctx, identity), nil, nil
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
			return true, "", nil, err
		}
		return true, fmt.Sprintf("Created task: %s (%s)", task.Title, task.ID), nil, nil
	case lower == "/choose" || strings.HasPrefix(lower, "/choose "):
		// ProcessMessage handles /choose before this generic switch so it can
		// restore and continue the original request. Reaching this branch means
		// the invocation did not satisfy that typed contract.
		return true, "Usage: /choose <choice_id> <number>", nil, nil
	case lower == "/status":
		reply, err := d.statusReply(ctx, identity)
		return true, reply, nil, err
	case lower == "/resume":
		// Bare /resume IS the attention list. It used to relay to /tasks, which
		// made "what can I continue" a Task listing and put the ordinal
		// snapshot in a view about labels rather than about runs.
		reply, err := d.attentionListReply(ctx, identity, req.Channel)
		return true, reply, nil, err
	case strings.HasPrefix(lower, "/resume "):
		parts := strings.Fields(trimmed)
		if len(parts) < 2 {
			return true, "Usage: /resume <n|run_id>  (bare /resume lists what needs attention)", nil, nil
		}
		if ordinal, convErr := strconv.Atoi(parts[1]); convErr == nil {
			_, runID, count, found := d.taskLists.resolveRun(identity, req.Channel, ordinal, time.Now())
			if found {
				if runID == "" {
					return true, fmt.Sprintf("No resumable run number %d in the last list; it showed %d (run /resume to refresh).", ordinal, count), nil, nil
				}
				handled, reply, err := d.selectExactResumeRun(ctx, identity, runID)
				return handled, reply, nil, err
			}
		}
		if strings.HasPrefix(strings.ToLower(parts[1]), "run_") {
			handled, reply, err := d.selectExactResumeRun(ctx, identity, parts[1])
			return handled, reply, nil, err
		}
		// A bare number is a list ordinal against the last /resume listing;
		// anything else is a full run id, or a thread id kept working for
		// references copied out of older transcripts.
		task, userErr, err := d.resolveTaskReference(ctx, identity, parts[1], req.Channel)
		if err != nil {
			return true, "", nil, err
		}
		if userErr != "" {
			return true, userErr, nil, nil
		}
		runs, err := d.Control.ListUnresolvedRuns(ctx, identity.TenantID, identity.PersonID, task.ID, 20)
		if err != nil {
			return true, "", nil, err
		}
		switch len(runs) {
		case 0:
			return true, "That thread has no exact resumable run. Use /search to find the work, then /resume <run_id>.", nil, nil
		case 1:
			handled, reply, err := d.selectExactResumeRun(ctx, identity, runs[0].ID)
			return handled, reply, nil, err
		default:
			return true, fmt.Sprintf("That thread has %d resumable runs. Run /resume to list them, then /resume <run_id> to choose one exactly.", len(runs)), nil, nil
		}
	case lower == "/search" || strings.HasPrefix(lower, "/search "):
		reply, err := d.searchCommandReply(ctx, identity, strings.TrimSpace(trimmed[len("/search"):]))
		return true, reply, nil, err
	case lower == "/queue" || strings.HasPrefix(lower, "/queue "):
		arg := strings.TrimSpace(trimmed[len("/queue"):])
		if strings.EqualFold(arg, "clear") {
			n, err := d.Control.ClearQueued(ctx, identity.TenantID, identity.PersonID)
			if err != nil {
				return true, "", nil, err
			}
			return true, fmt.Sprintf("Cleared %d queued task(s).", n), nil, nil
		}
		queued, err := d.Control.ListQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued)
		if err != nil {
			return true, "", nil, err
		}
		// `/queue drop <n>` removes one item by its list position (same
		// ordering as the /queue listing), so a single unwanted task can be
		// dropped without clearing the whole queue.
		if rest := strings.TrimSpace(strings.TrimPrefix(strings.ToLower(arg), "drop")); arg != "" && strings.HasPrefix(strings.ToLower(arg), "drop") {
			n, convErr := strconv.Atoi(strings.TrimSpace(rest))
			if convErr != nil || n < 1 || n > len(queued) {
				return true, fmt.Sprintf("Usage: /queue drop <n> (1-%d). Run /queue to see positions.", len(queued)), nil, nil
			}
			target := queued[n-1]
			if err := d.Control.MarkQueued(ctx, identity.TenantID, target.ID, control.QueueStatusCancelled); err != nil {
				return true, "", nil, err
			}
			return true, "Dropped queued task: " + textutil.Truncate(toOneLine(target.Content), 60), nil, nil
		}
		return true, formatQueue(queued), nil, nil
	case lower == "/watchers" || strings.HasPrefix(lower, "/watchers "):
		reply, err := d.watchersCommandReply(ctx, identity, strings.Fields(trimmed)[1:])
		return true, reply, nil, err
	case lower == "/diag memory":
		reply, err := d.memoryDiagReply(ctx, identity)
		return true, reply, nil, err
	case lower == "/diag context":
		reply, err := d.contextDiagReply(ctx, identity)
		return true, reply, nil, err
	case lower == "/diag models":
		reply, err := d.modelsDiagReply(ctx, identity)
		return true, reply, nil, err
	case lower == "/diag execution":
		reply, err := d.executionDiagReply(ctx, identity)
		return true, reply, nil, err
	case lower == "/diag tools":
		reply, err := d.toolsDiagReply(ctx, identity)
		return true, reply, nil, err
	case lower == "/diag delivery recover stale-results":
		reply, err := d.recoverStaleDeliveryResultsReply(ctx, identity, req)
		return true, reply, nil, err
	case lower == "/diag delivery dismiss stale-results":
		reply, err := d.dismissStaleDeliveryResultsReply(ctx, identity, req)
		return true, reply, nil, err
	case strings.HasPrefix(lower, "/diag delivery retry "):
		ref := strings.TrimSpace(trimmed[len("/diag delivery retry "):])
		reply, err := d.retryDeliveryReply(ctx, identity, req, ref)
		return true, reply, nil, err
	case strings.HasPrefix(lower, "/diag delivery dismiss "):
		ref := strings.TrimSpace(trimmed[len("/diag delivery dismiss "):])
		reply, err := d.dismissDeliveryReply(ctx, identity, req, ref)
		return true, reply, nil, err
	case lower == "/diag delivery":
		reply, err := d.deliveryDiagReply(ctx, identity)
		return true, reply, nil, err
	case lower == "/diag":
		reply, err := d.diagReply(ctx, identity)
		return true, reply, nil, err
	case lower == "/report":
		return true, "Usage: /report daily [--since 24h]", nil, nil
	case lower == "/report daily" || strings.HasPrefix(lower, "/report daily "):
		window, err := parseDailyReportWindow(trimmed)
		if err != nil {
			return true, "Usage: /report daily [--since 24h]", nil, nil
		}
		reply, err := d.dailyQualityReport(ctx, identity, window)
		return true, reply, nil, err
	case strings.HasPrefix(lower, "/ws "):
		// One workspace verb, the short one. It used to have two more spellings
		// (/workspace and /workspaces) that behaved identically; three names for
		// one thing is something to read, remember, and keep in sync.
		parts := strings.Fields(req.Content)
		if len(parts) < 2 {
			return true, "Usage: /ws <n|id> | /ws default <n|id> | /ws trust|untrust|decline", nil, nil
		}
		switch strings.ToLower(parts[1]) {
		case "trust", "untrust", "decline":
			reply, ws, err := d.workspaceTrustReply(ctx, identity, req, strings.ToLower(parts[1]))
			return true, reply, ws, err
		case "default":
			// The DURABLE default, used by turns that carry no directory of
			// their own (IM, cron). Selecting a workspace for the session no
			// longer changes it: two terminals in different projects were
			// overwriting one person-level value, so each one's list showed the
			// other's choice as current while its own work ran elsewhere.
			if len(parts) < 3 {
				return true, "Usage: /ws default <n|workspace_id>", nil, nil
			}
			ws, userErr, err := d.resolveWorkspaceReference(ctx, identity, parts[2])
			if err != nil {
				return true, "", nil, err
			}
			if userErr != "" {
				return true, userErr, nil, nil
			}
			if err := d.Control.SetCurrentWorkspace(ctx, identity.TenantID, identity.PersonID, ws.ID); err != nil {
				return true, "", nil, err
			}
			return true, fmt.Sprintf("Default workspace for IM and scheduled work: %s (%s)\n%s", ws.Name, ws.ID, ws.LocalPath), nil, nil
		}
		ws, userErr, err := d.resolveWorkspaceReference(ctx, identity, parts[1])
		if err != nil {
			return true, "", nil, err
		}
		if userErr != "" {
			return true, userErr, nil, nil
		}
		if !isLocalCLIRequest(req) {
			// An IM turn has no session of its own, so selecting there IS the
			// durable default.
			if err := d.Control.SetCurrentWorkspace(ctx, identity.TenantID, identity.PersonID, ws.ID); err != nil {
				return true, "", nil, err
			}
			return true, formatWorkspaceSwitchForIM(ws), nil, nil
		}
		reply := fmt.Sprintf("Current workspace: %s (%s)\n%s", ws.Name, ws.ID, ws.LocalPath)
		// Entering an untrusted workspace is where the client asks the trust
		// question, so the switch publishes the workspace typed. The reply text
		// stays as-is for endpoints that render prose only.
		if note := sessionWorkspaceTrustNote(ws); note != "" {
			reply += "\n" + note
		}
		return true, reply, digestWorkspaceFrom(ws), nil
	case lower == "/ws":
		// Ensure the session's own directory is a workspace before listing:
		// control commands run before the run pipeline would create it, so a
		// fresh directory used to be missing from its own listing.
		session := d.ensureSessionWorkspace(ctx, identity, req)
		workspaces, err := d.listWorkspacesForDisplay(ctx, identity)
		if err != nil {
			return true, "", nil, err
		}
		defaultID := ""
		if current, _ := d.Control.CurrentWorkspace(ctx, identity.TenantID, identity.PersonID); current != nil {
			defaultID = current.ID
		}
		if !isLocalCLIRequest(req) {
			return true, formatWorkspacesForIM(workspaces, defaultID), nil, nil
		}
		sessionID := ""
		if session != nil {
			sessionID = session.ID
		}
		reply := formatWorkspaces(workspaces, sessionID, defaultID)
		if note := sessionWorkspaceTrustNote(session); note != "" {
			reply += "\n\n" + note
		}
		return true, reply, nil, nil
	case lower == "/approvals" || strings.HasPrefix(lower, "/approvals "):
		rest := strings.TrimSpace(trimmed[len("/approvals"):])
		if rest != "" {
			// Remembered classes are user-owned state, so they must be listable
			// and revocable. Without this surface ten person-scope host grants
			// accumulated in a single day with nothing able to show or withdraw
			// them.
			return true, d.approvalGrantsReply(ctx, identity, rest), nil, nil
		}
		approvals, titles, err := d.pendingApprovalsForDisplay(ctx, identity)
		if err != nil {
			return true, "", nil, err
		}
		return true, formatApprovals(approvals, titles), nil, nil
	case lower == "/notify" || strings.HasPrefix(lower, "/notify "):
		reply, err := d.notifyPreferenceReply(ctx, identity, strings.TrimSpace(trimmed[len("/notify"):]))
		return true, reply, nil, err
	case lower == "/mode" || strings.HasPrefix(lower, "/mode "):
		reply, err := d.approvalModeReply(ctx, identity, strings.TrimSpace(trimmed[len("/mode"):]))
		return true, reply, nil, err
	case lower == "/approve" || strings.HasPrefix(lower, "/approve "):
		return true, d.respondApprovalCommand(ctx, identity, strings.TrimSpace(trimmed[len("/approve"):]), "approved", req.Channel), nil, nil
	case lower == "/reject" || strings.HasPrefix(lower, "/reject "):
		return true, d.respondApprovalCommand(ctx, identity, strings.TrimSpace(trimmed[len("/reject"):]), "rejected", req.Channel), nil, nil
	case lower == "/events":
		task, err := d.Control.CurrentTask(ctx, identity.TenantID, identity.PersonID)
		if err != nil {
			return true, "", nil, err
		}
		if task == nil {
			return true, "No active task.", nil, nil
		}
		events, err := d.Control.ListTaskEvents(ctx, task.ID, 20)
		if err != nil {
			return true, "", nil, err
		}
		return true, formatEvents(events), nil, nil
	case lower == "/task status" || lower == "task status" || lower == "/status" || lower == "status" || strings.Contains(lower, "进度"):
		reply, err := d.statusReply(ctx, identity)
		return true, reply, nil, err
	default:
		// Near-miss typo help: "/approves" → suggest "/approvals". Only claim
		// the message when the token is close to a KNOWN control command —
		// unknown slashes may be skill invocations or agent input and must
		// keep flowing through unchanged.
		if suggestion := suggestControlCommand(lower); suggestion != "" {
			return true, fmt.Sprintf("Unknown command %s — did you mean %s?", strings.Fields(lower)[0], suggestion), nil, nil
		}
		return false, "", nil, nil
	}
}

func (d *Server) selectExactResumeRun(ctx context.Context, identity *control.IdentityContext, ref string) (bool, string, error) {
	run, userErr, err := d.resolveUnresolvedRunReference(ctx, identity, ref)
	if err != nil {
		return true, "", err
	}
	if userErr != "" {
		return true, userErr, nil
	}
	task, err := d.Control.GetTask(ctx, identity.TenantID, run.TaskID)
	if err != nil || task == nil || task.PersonID != identity.PersonID {
		if err != nil {
			return true, "", err
		}
		return true, "Run not found or no longer resumable.", nil
	}
	// Reopening the thread and writing both pins is one control-store
	// transaction: a crash can never leave a thread pin without its exact run.
	if err := d.Control.PinResumeSelection(ctx, identity.TenantID, identity.PersonID, task.ID, run.ID); err != nil {
		return true, "", err
	}
	return true, fmt.Sprintf("Selected run %s under thread %s. Your next message will continue that exact run.%s",
		shortRunID(run.ID), shortTaskID(task.ID), d.resumeWorkspaceNote(ctx, identity, task)), nil
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

// dismissCurrentAttention is the /stop-without-a-run and /cancel compatibility
// path. It only ever dismisses one exact Run: the pinned /resume Run when a run
// pin exists, else the single unambiguous Attention item. A thread pin without
// an exact Run dismisses nothing, because a Thread is history, not something
// to stop. Run outcomes and pending control rows remain intact, and a Run that
// still owns an approval, question, or watcher refuses.
func (d *Server) dismissCurrentAttention(ctx context.Context, identity *control.IdentityContext) string {
	if d == nil || d.Control == nil || identity == nil {
		return "No active run to stop."
	}
	timeline := control.NewWorkTimeline(d.Control)
	runPin, err := d.Control.GetPersonSetting(ctx, identity.TenantID, identity.PersonID, resumeRunPinKey)
	if err != nil {
		return "Could not inspect current attention: " + err.Error()
	}
	threadPin, err := d.Control.GetPersonSetting(ctx, identity.TenantID, identity.PersonID, resumePinKey)
	if err != nil {
		return "Could not inspect current attention: " + err.Error()
	}
	clearPins := func() {
		_ = d.Control.SetPersonSetting(ctx, identity.TenantID, identity.PersonID, resumePinKey, "")
		_ = d.Control.SetPersonSetting(ctx, identity.TenantID, identity.PersonID, resumeRunPinKey, "")
	}
	if runPin = strings.TrimSpace(runPin); runPin != "" {
		run, err := d.Control.GetRun(ctx, identity.TenantID, runPin)
		if err != nil {
			return "Could not inspect current attention: " + err.Error()
		}
		if run == nil || run.PersonID != identity.PersonID {
			clearPins()
			return "No active run to stop. The pinned resume selection no longer exists, so the pin was cleared."
		}
		dismissed, err := timeline.DismissAttentionRun(ctx, identity.TenantID, identity.PersonID, run.TaskID, run.ID)
		if err != nil {
			if refusal, ok := attentionDismissalRefusal(err); ok {
				return "No active run to stop. " + refusal
			}
			return "Could not dismiss attention: " + err.Error()
		}
		clearPins()
		if !dismissed {
			return fmt.Sprintf("No active run to stop. Pinned run %s is not current attention (already dismissed or superseded); the resume pin was cleared.", shortRunID(run.ID))
		}
		return d.reportDismissedAttentionRun(ctx, identity, run.TaskID, run.ID, "user dismissed the pinned resume run")
	}
	if strings.TrimSpace(threadPin) != "" {
		return "No active run to stop. A thread is selected for /resume but no exact run is pinned, so nothing was dismissed; run /resume to see what needs attention."
	}
	attention, err := timeline.Attention(ctx, identity.TenantID, identity.PersonID, 2)
	if err != nil {
		return "Could not inspect current attention: " + err.Error()
	}
	switch len(attention) {
	case 0:
		return "No active run to stop, and nothing currently needs attention."
	case 1:
		item := attention[0]
		dismissed, err := timeline.DismissAttentionRun(ctx, identity.TenantID, identity.PersonID, item.Thread.ID, item.RunID)
		if err != nil {
			if refusal, ok := attentionDismissalRefusal(err); ok {
				return "No active run to stop. " + refusal
			}
			return "Could not dismiss attention: " + err.Error()
		}
		if !dismissed {
			return "No active run to stop, and nothing currently needs attention."
		}
		return d.reportDismissedAttentionRun(ctx, identity, item.Thread.ID, item.RunID, "user dismissed inactive attention")
	default:
		return "No active run. Several items need attention; run /resume to see them."
	}
}

// dismissAttentionByReference resolves ONE attention item — a list ordinal from
// the same snapshot `/resume` owns, or an exact run id — and dismisses it
// without executing anything. Cancelling the active run stays the job of bare
// `/stop`, so naming the running item here routes there instead of pretending a
// live run can be put down without stopping it.
func (d *Server) dismissAttentionByReference(ctx context.Context, identity *control.IdentityContext, channel, ref string) string {
	if d == nil || d.Control == nil || identity == nil {
		return "Could not dismiss attention: the control store is unavailable."
	}
	ref = strings.TrimSpace(ref)
	runID := ""
	if ordinal, convErr := strconv.Atoi(ref); convErr == nil {
		_, resolved, count, found := d.taskLists.resolveRun(identity, channel, ordinal, time.Now())
		if !found {
			return fmt.Sprintf("No list to number %d against; run /resume first.", ordinal)
		}
		if resolved == "" {
			return fmt.Sprintf("No item number %d in the last list; it showed %d (run /resume to refresh).", ordinal, count)
		}
		runID = resolved
	} else if strings.HasPrefix(strings.ToLower(ref), "run_") {
		runID = ref
	} else {
		return "Usage: /stop <n|run_id>  (bare /stop cancels the active run)"
	}

	run, err := d.Control.GetRun(ctx, identity.TenantID, runID)
	if err != nil {
		return "Could not dismiss attention: " + err.Error()
	}
	if run == nil || run.PersonID != identity.PersonID {
		return "That run is not yours or no longer exists."
	}
	if active := d.coordinator().currentActive(identity.PersonID); active != nil && active.RunID == run.ID {
		return fmt.Sprintf("Run %s is executing now; use /stop with no number to cancel it.", shortRunID(run.ID))
	}
	dismissed, err := control.NewWorkTimeline(d.Control).DismissAttentionRun(ctx, identity.TenantID, identity.PersonID, run.TaskID, run.ID)
	if err != nil {
		if refusal, ok := attentionDismissalRefusal(err); ok {
			return refusal
		}
		return "Could not dismiss attention: " + err.Error()
	}
	if !dismissed {
		return fmt.Sprintf("Run %s is not current attention (already dismissed or superseded).", shortRunID(run.ID))
	}
	return d.reportDismissedAttentionRun(ctx, identity, run.TaskID, run.ID, "user dismissed an attention item by reference")
}

// reportDismissedAttentionRun records the exact-run dismissal and names it.
func (d *Server) reportDismissedAttentionRun(ctx context.Context, identity *control.IdentityContext, threadID, runID, reason string) string {
	_, _ = d.Control.AppendEvent(ctx, control.Event{
		TaskID:     threadID,
		RunID:      runID,
		Type:       "attention.dismissed",
		Visibility: "task",
		Payload:    mustJSON(map[string]string{"reason": reason, "run_id": runID}),
	})
	title := shortTaskID(threadID)
	if task, err := d.Control.GetTask(ctx, identity.TenantID, threadID); err == nil && task != nil && strings.TrimSpace(task.Title) != "" {
		title = textutil.Truncate(toOneLine(task.Title), 60)
	}
	return fmt.Sprintf("No live run was executing. Dismissed current attention for exact run %s in %s. Explicit /resume %s can still continue it.",
		shortRunID(runID), title, shortRunID(runID))
}

// statusReply builds the /status card shared by every channel. The active Run
// wins; without one, an explicit resume pin then the highest derived Attention
// item supplies the presentation Thread. Recency never invents a current item.
// When the card is about one exact parked Run (a pinned or Attention Run), its
// handoff and plan come from that Run, never from whichever Run in the Thread
// happens to be newest.
func (d *Server) statusReply(ctx context.Context, identity *control.IdentityContext) (string, error) {
	active := d.coordinator().currentActive(identity.PersonID)
	var task *control.Task
	exactRunID := ""
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
		if task != nil && task.ActiveRunID == "" {
			// CurrentTask without an executing Run is the explicit /resume pin;
			// its exact Run pin names the parked Run the card describes.
			if pinned, err := d.Control.GetPersonSetting(ctx, identity.TenantID, identity.PersonID, resumeRunPinKey); err == nil {
				if run, err := d.Control.GetRun(ctx, identity.TenantID, strings.TrimSpace(pinned)); err == nil && run != nil && run.TaskID == task.ID {
					exactRunID = run.ID
				}
			}
		}
	}
	if task == nil {
		attention, err := control.NewWorkTimeline(d.Control).Attention(ctx, identity.TenantID, identity.PersonID, 1)
		if err != nil {
			return "", err
		}
		if len(attention) > 0 {
			task, err = d.Control.GetTask(ctx, identity.TenantID, attention[0].Thread.ID)
			if err != nil {
				return "", err
			}
			exactRunID = attention[0].RunID
		}
	}
	if task == nil {
		return "No active task.", nil
	}
	var handoff *control.Handoff
	var plan []taskPlanStep
	if exactRunID != "" {
		handoff, _ = d.Control.RunHandoff(ctx, identity.TenantID, identity.PersonID, exactRunID)
		plan = d.latestPlanForRun(ctx, identity.TenantID, identity.PersonID, task.ID, exactRunID)
	} else {
		handoff, _ = d.Control.LatestHandoff(ctx, task.ID)
		plan = d.latestPlanForTask(ctx, task.ID)
	}
	card := formatTaskStatus(task, handoff, active, plan)
	if recovery := d.latestRecoveryHandoffForTask(ctx, identity, task.ID); recovery != nil {
		card += "\n\n" + formatRecoveryHandoff(recovery)
	}
	// A run blocked on an approval looks "stuck" unless the card says the run
	// is waiting for the HUMAN (observed live: 15 minutes of staring at
	// "running" while an ls_r approval sat pending). Surface it with the same
	// conversational summary the push uses.
	if pending, err := d.Control.ListApprovalRequests(ctx, identity.TenantID, identity.PersonID, "pending", 5); err == nil && len(pending) > 0 {
		sortApprovalsForDisplay(pending)
		waitAge := humanDuration(time.Since(pending[0].CreatedAt))
		card += "\n⚠ Waiting for your approval — reply y or n:\n"
		// Pending input is person-scoped: an approval raised by another Thread
		// still blocks this person, so it belongs on the card. Name its Thread
		// whenever it is not the one this card describes, or a foreign item
		// reads as if it came from the work shown above.
		titles := d.taskTitlesFor(ctx, identity.TenantID, pending)
		foreignTitle := func(threadID string) string {
			if strings.TrimSpace(threadID) == "" || threadID == task.ID {
				return ""
			}
			return titles[threadID]
		}
		for i, approval := range pending {
			if len(pending) == 1 {
				card += approvalSummaryLine(approval, foreignTitle(approval.TaskID)) + "\n"
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
		// Same rule as the approvals above: a question from another Thread is
		// still the person's to answer, but it must say which work it belongs to.
		clarifyLine := func(clarify control.ClarifyRequest) string {
			line := clarifySummaryLine(clarify)
			if strings.TrimSpace(clarify.TaskID) == "" || clarify.TaskID == task.ID {
				return line
			}
			if other, err := d.Control.GetTask(ctx, identity.TenantID, clarify.TaskID); err == nil && other != nil && strings.TrimSpace(other.Title) != "" {
				line += fmt.Sprintf(" (task: %s)", truncate(toOneLine(other.Title), 30))
			}
			return line
		}
		for i, clarify := range clarifies {
			if len(clarifies) == 1 {
				card += clarifyLine(clarify) + "\n"
				break
			}
			card += fmt.Sprintf("%d. %s\n", i+1, clarifyLine(clarify))
		}
		card = strings.Replace(card, "Waiting for your answer", fmt.Sprintf("Waiting for your answer (%s elapsed)", waitAge), 1)
	}
	return card, nil
}
