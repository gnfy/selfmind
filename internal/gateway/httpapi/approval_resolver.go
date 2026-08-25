package httpapi

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/delivery"
	"selfmind/internal/platform/log"
	"selfmind/internal/platform/textutil"
)

// Approval-reference resolution shared by every surface that responds to an
// approval: the /approve and /reject control commands (CLI + IM),
// POST /v1/approvals/respond, and Telegram callback buttons. A user should be
// able to say "/approve 1" (the number shown by /approvals), paste a unique
// apr_ prefix, or — with a single pending approval — just "/approve". All
// surfaces resolve through this file so ordinals and prefixes mean the same
// thing everywhere.

// minApprovalPrefixLen is the shortest accepted apr_ prefix. "apr_" plus four
// characters is enough to disambiguate in practice while staying typeable on a
// phone.
const minApprovalPrefixLen = 8

// sortApprovalsForDisplay orders approvals oldest-first (created_at ASC, id
// ASC as tiebreaker). This is the display order of /approvals and therefore
// the order ordinal references ("/approve 1") resolve against; both sides must
// use it or numbers would approve the wrong action.
func sortApprovalsForDisplay(approvals []control.ApprovalRequest) {
	sort.SliceStable(approvals, func(i, j int) bool {
		if !approvals[i].CreatedAt.Equal(approvals[j].CreatedAt) {
			return approvals[i].CreatedAt.Before(approvals[j].CreatedAt)
		}
		return approvals[i].ID < approvals[j].ID
	})
}

// resolveApprovalReference resolves a user-supplied token against the person's
// pending approvals (already in display order). Errors are user-facing
// sentences, safe to return verbatim on any channel.
func resolveApprovalReference(pending []control.ApprovalRequest, token string) (*control.ApprovalRequest, error) {
	token = strings.TrimSpace(token)

	if token == "" {
		switch len(pending) {
		case 0:
			return nil, fmt.Errorf("no pending approvals")
		case 1:
			return &pending[0], nil
		default:
			return nil, fmt.Errorf("%d approvals are pending; run /approvals and pick one, e.g. /approve 1", len(pending))
		}
	}

	if ordinal, err := strconv.Atoi(token); err == nil {
		if len(pending) == 0 {
			return nil, fmt.Errorf("no pending approvals")
		}
		if ordinal < 1 || ordinal > len(pending) {
			return nil, fmt.Errorf("no pending approval number %d; %d pending (see /approvals)", ordinal, len(pending))
		}
		return &pending[ordinal-1], nil
	}

	if strings.HasPrefix(token, "task_") {
		return nil, fmt.Errorf("%s is a task id; approval ids start with apr_, or use the list number from /approvals", token)
	}

	if strings.HasPrefix(token, "apr_") {
		if len(token) < minApprovalPrefixLen {
			return nil, fmt.Errorf("approval id prefix %q is too short; use at least %d characters or the list number from /approvals", token, minApprovalPrefixLen)
		}
		var matches []*control.ApprovalRequest
		for i := range pending {
			if strings.HasPrefix(pending[i].ID, token) {
				matches = append(matches, &pending[i])
			}
		}
		switch len(matches) {
		case 1:
			return matches[0], nil
		case 0:
			return nil, fmt.Errorf("no pending approval matches %q; run /approvals to list them", token)
		default:
			ids := make([]string, 0, len(matches))
			for _, m := range matches {
				ids = append(ids, m.ID)
			}
			return nil, fmt.Errorf("approval id prefix %q is ambiguous, it matches: %s; add more characters or use the list number", token, strings.Join(ids, ", "))
		}
	}

	return nil, fmt.Errorf("unrecognized approval reference %q; use the number from /approvals or an apr_ id", token)
}

// respondApprovalByToken resolves token (ordinal, apr_ prefix, full id, or
// empty for a lone pending approval) for the person and records the decision.
// All validation failures come back as user-facing errors; only storage
// problems surface as internal errors.
func (d *Server) respondApprovalByToken(ctx context.Context, identity *control.IdentityContext, token, decision, channel string, input control.ApprovalDecisionInput) (*control.ApprovalRequest, error) {
	if d == nil || d.Control == nil || identity == nil {
		return nil, fmt.Errorf("approval store is not available")
	}
	pending, err := d.Control.ListApprovalRequests(ctx, identity.TenantID, identity.PersonID, "pending", 100)
	if err != nil {
		return nil, err
	}
	sortApprovalsForDisplay(pending)
	resolved, resolveErr := resolveApprovalReference(pending, token)
	if resolveErr != nil {
		// A full id that no longer matches any *pending* row usually means the
		// approval was already decided; report that instead of "not found".
		if strings.HasPrefix(strings.TrimSpace(token), "apr_") {
			if existing, getErr := d.Control.GetApprovalRequest(ctx, identity.TenantID, strings.TrimSpace(token)); getErr == nil && existing != nil && existing.PersonID == identity.PersonID && existing.Status != "pending" {
				return nil, fmt.Errorf("approval %s is already %s", existing.ID, existing.Status)
			}
		}
		return nil, resolveErr
	}
	options := decodeApprovalDecisions(resolved.Payload)
	if len(options) > 0 {
		option, ok := approvalOptionForDecision(options, decision, input.GrantScope, input.GrantKey)
		if !ok && decision == "approved" && strings.TrimSpace(input.GrantScope) == "" && strings.TrimSpace(input.GrantKey) == "" {
			if defaultOption, found := approvalOptionByShortcut(options, "y"); found && defaultOption.Decision == decision {
				option, ok = defaultOption, true
				input.GrantScope = option.Scope
				input.GrantKey = option.GrantKey
			}
		}
		if !ok && strings.TrimSpace(input.GrantKey) == "" {
			option, ok = uniqueApprovalOptionForScope(options, decision, input.GrantScope)
			if ok {
				input.GrantKey = option.GrantKey
			}
		}
		if !ok || (input.DecisionID != "" && input.DecisionID != option.ID) {
			return nil, fmt.Errorf("that approval choice was not offered for this request")
		}
		input.DecisionID = option.ID
	} else if strings.TrimSpace(input.GrantScope) != "" || strings.TrimSpace(input.GrantKey) != "" {
		return nil, fmt.Errorf("remembered approval is unavailable for this legacy request; approve it once instead")
	}
	var approval *control.ApprovalRequest
	if resolved.WaiterState == "parked" {
		task, taskErr := d.Control.GetTask(ctx, identity.TenantID, resolved.TaskID)
		if taskErr != nil || task == nil {
			if taskErr != nil {
				return nil, taskErr
			}
			return nil, fmt.Errorf("approval %s no longer has a resumable task", resolved.ID)
		}
		content := parkedApprovalResumeContent(*resolved, decision, input.Note)
		var executionRoots []executionenv.RootBinding
		if sourceRun, runErr := d.Control.GetRun(ctx, identity.TenantID, resolved.RunID); runErr != nil {
			return nil, runErr
		} else if sourceRun != nil {
			executionRoots = executionenv.CloneRootBindings(sourceRun.ExecutionRoots)
		}
		var queued *control.QueuedTask
		approval, queued, err = d.Control.RespondParkedApprovalAndEnqueue(ctx,
			identity.TenantID, identity.PersonID, resolved.ID, decision, channel, input,
			control.QueuedTask{
				PersonID: identity.PersonID, Platform: identity.Platform,
				PlatformUserID: identity.PlatformUserID, Channel: fallback(channel, identity.Platform),
				Content: content, WorkspaceID: task.WorkspaceID, TaskID: task.ID,
				ExecutionRoots: executionRoots,
				IdempotencyKey: "approval-resume:" + resolved.ID,
				Class:          control.QueueClassForeground,
			},
		)
		if err == nil && queued != nil {
			// The row is durable before draining. If another run is active this is a
			// harmless no-op; that run's finalizer drains it later.
			d.coordinator().drainQueue(identity)
		}
	} else {
		approval, err = d.Control.RespondApprovalRequest(ctx, identity.TenantID, identity.PersonID, resolved.ID, decision, channel, input)
	}
	if err != nil {
		return nil, err
	}
	if _, eventErr := d.Control.AppendApprovalResolutionEvent(ctx, approval, channel, ""); eventErr != nil {
		// The row is already terminal, so returning an error would invite a retry
		// that can never succeed. Keep the decision honest and make the missing
		// cross-endpoint signal observable instead of swallowing it.
		log.Warn("failed to append approval resolution event", "approval_id", approval.ID, "status", approval.Status, "error", eventErr)
	}
	d.notifyApprovalResolutionElsewhere(ctx, identity, approval, channel)
	return approval, nil
}

// notifyApprovalResolutionElsewhere reconciles mobile surfaces that may still
// show the request after it was answered in the CLI or another IM. Platform
// message editing is not uniformly available, so the portable contract is one
// short, idempotent follow-up to each actually-delivered request route. The
// route that submitted the answer is skipped.
func (d *Server) notifyApprovalResolutionElsewhere(ctx context.Context, identity *control.IdentityContext, approval *control.ApprovalRequest, answerChannel string) {
	if d == nil || d.Control == nil || d.Delivery == nil || identity == nil || approval == nil {
		return
	}
	routes, err := d.Control.ListDeliveredApprovalRoutes(ctx, approval.TenantID, approval.PersonID, approval.ID)
	if err != nil {
		log.Warn("failed to list approval delivery routes", "approval_id", approval.ID, "error", err)
		return
	}
	seen := map[string]bool{}
	for _, route := range routes {
		key := strings.ToLower(strings.TrimSpace(route.Platform)) + "|" + strings.TrimSpace(route.PlatformUserID) + "|" + strings.TrimSpace(route.Channel)
		if seen[key] || sameApprovalAnswerRoute(route, identity, answerChannel) {
			continue
		}
		seen[key] = true
		verb := "denied"
		glyph := "✗"
		switch approval.Status {
		case "approved":
			verb, glyph = "approved", "✓"
		case "archived":
			verb, glyph = "archived after 7 days", "⚠"
		case "expired":
			verb, glyph = "cancelled", "⚠"
		}
		content := fmt.Sprintf("%s Approval resolved elsewhere: %s was %s. This request is closed.", glyph, approvalSummaryLine(*approval, ""), verb)
		accepted, sendErr := d.Delivery.EnqueueAndTryAccepted(ctx, delivery.Message{
			TenantID: approval.TenantID, PersonID: approval.PersonID,
			Platform: route.Platform, PlatformUserID: route.PlatformUserID, Channel: route.Channel,
			TaskID: approval.TaskID, RunID: approval.RunID, Content: content,
			Kind: delivery.KindApprovalResolution, ApprovalID: approval.ID,
			LogicalKey: "approval-resolution:" + approval.ID + ":" + approval.Status,
		})
		if sendErr != nil || !accepted {
			log.Warn("failed to enqueue approval resolution follow-up", "approval_id", approval.ID, "platform", route.Platform, "error", sendErr)
		}
	}
}

func sameApprovalAnswerRoute(route control.Delivery, identity *control.IdentityContext, answerChannel string) bool {
	if identity == nil || !strings.EqualFold(strings.TrimSpace(route.Platform), strings.TrimSpace(identity.Platform)) {
		return false
	}
	if userID := strings.TrimSpace(identity.PlatformUserID); userID != "" && strings.TrimSpace(route.PlatformUserID) != "" {
		return userID == strings.TrimSpace(route.PlatformUserID)
	}
	return strings.TrimSpace(answerChannel) != "" && strings.TrimSpace(answerChannel) == strings.TrimSpace(route.Channel)
}

func parkedApprovalResumeContent(approval control.ApprovalRequest, decision, note string) string {
	if strings.EqualFold(strings.TrimSpace(decision), "approved") || strings.EqualFold(strings.TrimSpace(decision), "approve") {
		return fmt.Sprintf("Resume task after parked approval %s was approved. Re-evaluate the interrupted step from durable evidence. Approval authority is enforced by the execution middleware; never infer permission from this message.", approval.ID)
	}
	content := fmt.Sprintf("Resume task after parked approval %s was rejected. Do not retry the rejected operation or a cosmetic variant; choose a safe alternative or finish with an actionable explanation.", approval.ID)
	if reason := strings.TrimSpace(note); reason != "" {
		content += " User guidance: " + reason
	}
	return content
}

// approvalOptionForDecision recovers the exact server-issued answer from the
// decision fields older clients already send. This keeps the wire compatible
// while making the durable audit explicit instead of guessing from scope later.
func approvalOptionForDecision(options []approvalDecisionOption, decision, scope, grantKey string) (approvalDecisionOption, bool) {
	decision = strings.TrimSpace(decision)
	scope = strings.TrimSpace(scope)
	grantKey = strings.TrimSpace(grantKey)
	for _, option := range options {
		if option.Decision == decision && option.Scope == scope && option.GrantKey == grantKey {
			return option, true
		}
	}
	return approvalDecisionOption{}, false
}

// uniqueApprovalOptionForScope keeps older clients and the `/approve run`
// grammar compatible with a server-issued precise run rule. It may fill in a
// grant key only when exactly one offered option matches; ambiguity never
// guesses authority.
func uniqueApprovalOptionForScope(options []approvalDecisionOption, decision, scope string) (approvalDecisionOption, bool) {
	decision = strings.TrimSpace(decision)
	scope = strings.TrimSpace(scope)
	var match approvalDecisionOption
	found := false
	for _, option := range options {
		if option.Decision != decision || option.Scope != scope {
			continue
		}
		if found {
			return approvalDecisionOption{}, false
		}
		match, found = option, true
	}
	return match, found
}

// parseBareApprovalReply maps a conversational one-word answer to an approval
// decision plus an optional run-local grant scope. It stays
// tight (whole-message match only) so ordinary sentences are never misread as a
// decision. "ok"/"可以"/"好" double as continuation cues elsewhere; the caller
// only invokes this when an approval is pending, so a blocking approval wins
// that collision by construction. "r" is the compact run-local choice shown by
// both the terminal and IM menus; "yr" remains accepted for compatibility.
// shortcut, when non-empty, is a server-issued option letter from the ask's own
// decision list. The caller resolves it against that list, so a stale or hidden
// shortcut can never widen an approval.
func parseBareApprovalReply(content string) (decision, scope, shortcut string, ok bool) {
	s := strings.ToLower(strings.TrimSpace(content))
	s = strings.Trim(s, " \t\r\n.!?。！？，,")
	switch s {
	case "r", "yr":
		return "approved", "run", "", true
	case "yp", "yh", "yw", "yt", "ya":
		// "y" + a rule letter: approve AND persist the rule that letter names on
		// this ask. The scope travels with the resolved option, not from here.
		return "approved", "", strings.TrimPrefix(s, "y"), true
	case "y", "yes", "ok", "okay", "approve", "approved", "sure",
		"好", "好的", "可以", "同意", "批准", "行":
		return "approved", "", "", true
	case "n", "no", "reject", "rejected", "deny", "denied", "cancel",
		"不", "不行", "不可以", "拒绝", "取消":
		return "rejected", "", "", true
	}
	return "", "", "", false
}

// parseApprovalScopeWord maps a grant-scope word from the /approve grammar to
// "task", "person", or "" (unrecognized/once). "always" is the friendly alias
// for person scope.
func parseApprovalScopeWord(word string) string {
	switch strings.ToLower(strings.TrimSpace(word)) {
	case "run":
		return "run"
	case "task", "session":
		return "task"
	case "always", "person", "persistent":
		return "person"
	default:
		return ""
	}
}

// tryHandleBareApprovalReply resolves a conversational y/n against the person's
// pending approvals. It only claims the message when at least one approval is
// pending, so a bare "y" with nothing pending reaches the agent unchanged. One
// pending → decide it. Several pending (only possible with parallel runs, since
// the per-person active-run guard serializes interactive approvals) → the word
// is ambiguous, so return the numbered list and ask for /approve <n>.
func (d *Server) tryHandleBareApprovalReply(ctx context.Context, identity *control.IdentityContext, content, channel string) (bool, string, error) {
	decision, grantScope, shortcut, ok := parseBareApprovalReply(content)
	if !ok || d == nil || d.Control == nil || identity == nil {
		return false, "", nil
	}
	pending, err := d.Control.ListApprovalRequests(ctx, identity.TenantID, identity.PersonID, "pending", 100)
	if err != nil || len(pending) == 0 {
		// Store error or nothing pending: not an approval reply — let the word
		// flow to the agent (and to continuation-cue handling for "ok"/"可以").
		return false, "", nil
	}
	sortApprovalsForDisplay(pending)
	if len(pending) > 1 {
		titles := d.taskTitlesFor(ctx, identity.TenantID, pending)
		verb := "approve"
		if decision == "rejected" {
			verb = "reject"
		}
		return true, fmt.Sprintf("%d approvals are pending, so I cannot tell which one you mean. Reply /%s <n>:\n%s",
			len(pending), verb, formatApprovals(pending, titles)), nil
	}
	// A rule shortcut ("yp") means whatever `p` meant on THIS ask: resolve it
	// against the options the daemon actually offered for this row, so IM answers
	// from the same menu the terminal panel drew (batch B1). An unknown letter
	// degrades to a plain approval rather than guessing a rule.
	input := control.ApprovalDecisionInput{GrantScope: grantScope}
	if shortcut != "" {
		if option, found := approvalOptionByShortcut(decodeApprovalDecisions(pending[0].Payload), shortcut); found {
			input.GrantScope = option.Scope
			input.GrantKey = option.GrantKey
			input.DecisionID = option.ID
			decision = option.Decision
		} else {
			return true, "That approval option was not offered. Reply y, r, or n as shown in the request.", nil
		}
	}
	approval, err := d.respondApprovalByToken(ctx, identity, pending[0].ID, decision, channel, input)
	if err != nil {
		return true, "Could not record the decision: " + err.Error(), nil
	}
	verb := "Approved"
	if approval.Status == "rejected" {
		verb = "Rejected"
	}
	return true, verb + ": " + approvalSummaryLine(*approval, ""), nil
}

// respondApprovalCommand backs the /approve and /reject control commands. It
// always returns a one-line human reply — resolution problems are user
// mistakes, not server failures, so they come back as chat text (never a 500
// with raw JSON on any channel).
func (d *Server) respondApprovalCommand(ctx context.Context, identity *control.IdentityContext, token, decision, channel string) string {
	// The reference token may be followed by a grant-scope word:
	//   /approve            → this action once
	//   /approve run        → reuse the offered class/rule for this live run
	// Historical task/person scope words are parsed for wire compatibility, but a
	// current request rejects them because they are not server-issued choices.
	grantScope := ""
	fields := strings.Fields(token)
	if len(fields) > 0 {
		if s := parseApprovalScopeWord(fields[0]); s != "" {
			// First word IS the scope: token stays empty (lone pending approval).
			grantScope = s
			token = ""
		} else {
			token = fields[0]
			if len(fields) > 1 {
				grantScope = parseApprovalScopeWord(fields[1])
			}
		}
	} else {
		token = ""
	}
	verbBase := "approve"
	if decision == "rejected" {
		verbBase = "reject"
	}
	if strings.EqualFold(token, "all") {
		return d.respondAllApprovals(ctx, identity, decision, channel, verbBase)
	}
	approval, err := d.respondApprovalByToken(ctx, identity, token, decision, channel, control.ApprovalDecisionInput{GrantScope: grantScope})
	if err != nil {
		return "Cannot " + verbBase + ": " + err.Error()
	}
	verb := "Approved"
	if approval.Status == "rejected" {
		verb = "Rejected"
	}
	title := ""
	if approval.TaskID != "" {
		if task, terr := d.Control.GetTask(ctx, identity.TenantID, approval.TaskID); terr == nil && task != nil {
			title = task.Title
		}
	}
	note := grantScopeNoteWithClass(approval.DecisionScope, decodeApprovalPayload(*approval).GrantClass)
	if approval.ResumeQueueID != "" {
		note += " (task continuation queued)"
	}
	return fmt.Sprintf("%s%s %s\n%s", verb, note, approvalSummaryLine(*approval, title), approval.ID)
}

// grantScopeNote renders the human-facing "remembered" suffix for an approval
// that also recorded a class-level grant. Empty for a once-only decision.
//
// It names the CLASS, not just the scope: a user who cannot see what was
// remembered cannot notice when the class is wider than they intended, which is
// how ten person-scope host grants — two of them keyed on a shell prologue —
// accumulated unnoticed in a single day. When the eligibility floor refused to
// persist anything, the note says so instead of claiming a grant was made.
func grantScopeNote(scope string) string {
	return grantScopeNoteWithClass(scope, "")
}

func grantScopeNoteWithClass(scope, grantClass string) string {
	switch scope {
	case "task", "person":
	default:
		return ""
	}
	if strings.TrimSpace(grantClass) == "" {
		return " (not remembered: this command's class cannot be reused)"
	}
	window := "for this task"
	if scope == "person" {
		window = "for you across tasks, 8h"
	}
	return fmt.Sprintf(" (remembered %s: %s)", window, textutil.Truncate(toOneLine(grantClass), 80))
}

// respondAllApprovals backs "/approve all" and "/reject all": it decides every
// currently-pending approval in display order and reports each line. Parallel
// tool batches can raise several approvals at once; answering them one ordinal
// at a time re-numbers the list after every reply, which is exactly the
// friction this shortcut removes.
func (d *Server) respondAllApprovals(ctx context.Context, identity *control.IdentityContext, decision, channel, verbBase string) string {
	pending, err := d.Control.ListApprovalRequests(ctx, identity.TenantID, identity.PersonID, "pending", 100)
	if err != nil {
		return "Cannot " + verbBase + " all: " + err.Error()
	}
	if len(pending) == 0 {
		return "No pending approvals."
	}
	sortApprovalsForDisplay(pending)
	verb := "Approved"
	if decision == "rejected" {
		verb = "Rejected"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %d:\n", verb, len(pending))
	for _, item := range pending {
		// Resolve each row against its own server-issued default. This preserves
		// run-bundle semantics without inventing a broad grant for the batch.
		approval, err := d.respondApprovalByToken(ctx, identity, item.ID, decision, channel, control.ApprovalDecisionInput{})
		if err != nil {
			fmt.Fprintf(&sb, "- failed: %s (%s)\n", approvalSummaryLine(item, ""), err.Error())
			continue
		}
		fmt.Fprintf(&sb, "- %s\n", approvalSummaryLine(*approval, ""))
	}
	return strings.TrimRight(sb.String(), "\n")
}

// pendingApprovalsForDisplay loads the person's pending approvals in the
// stable display order plus a bounded task-title lookup for rich rendering.
func (d *Server) pendingApprovalsForDisplay(ctx context.Context, identity *control.IdentityContext) ([]control.ApprovalRequest, map[string]string, error) {
	approvals, err := d.Control.ListApprovalRequests(ctx, identity.TenantID, identity.PersonID, "pending", 20)
	if err != nil {
		return nil, nil, err
	}
	sortApprovalsForDisplay(approvals)
	return approvals, d.taskTitlesFor(ctx, identity.TenantID, approvals), nil
}

// taskTitlesFor resolves task titles referenced by the approvals, best-effort:
// a missing task simply renders without a title.
func (d *Server) taskTitlesFor(ctx context.Context, tenantID string, approvals []control.ApprovalRequest) map[string]string {
	titles := map[string]string{}
	if d == nil || d.Control == nil {
		return titles
	}
	for _, approval := range approvals {
		if approval.TaskID == "" {
			continue
		}
		if _, ok := titles[approval.TaskID]; ok {
			continue
		}
		if task, err := d.Control.GetTask(ctx, tenantID, approval.TaskID); err == nil && task != nil {
			titles[approval.TaskID] = task.Title
		}
	}
	return titles
}
