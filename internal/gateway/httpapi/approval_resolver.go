package httpapi

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"selfmind/internal/control"
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
func (d *Server) respondApprovalByToken(ctx context.Context, identity *control.IdentityContext, token, decision, channel string) (*control.ApprovalRequest, error) {
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
	approval, err := d.Control.RespondApprovalRequest(ctx, identity.TenantID, identity.PersonID, resolved.ID, decision, channel)
	if err != nil {
		return nil, err
	}
	appendApprovalEvent(ctx, d.Control, approval, channel)
	return approval, nil
}

// respondApprovalCommand backs the /approve and /reject control commands. It
// always returns a one-line human reply — resolution problems are user
// mistakes, not server failures, so they come back as chat text (never a 500
// with raw JSON on any channel).
func (d *Server) respondApprovalCommand(ctx context.Context, identity *control.IdentityContext, token, decision, channel string) string {
	if fields := strings.Fields(token); len(fields) > 0 {
		token = fields[0]
	} else {
		token = ""
	}
	verbBase := "approve"
	if decision == "rejected" {
		verbBase = "reject"
	}
	approval, err := d.respondApprovalByToken(ctx, identity, token, decision, channel)
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
	return fmt.Sprintf("%s %s\n%s", verb, approvalSummaryLine(*approval, title), approval.ID)
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
