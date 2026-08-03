package httpapi

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/tools"
)

// The remembered-class surface.
//
// A grant is a standing permission the user gave. It must therefore be
// listable, expirable and revocable — the ledger previously supported only
// "record" and "does it exist", so a class remembered from one over-broad key
// stayed authoritative forever with nothing able to show or withdraw it.
//
// The rendering names classes in plain words. It never prints the raw pattern
// key or a resource fingerprint: those are storage detail, and a user cannot
// review what they cannot read.

// approvalGrantsReply backs `/approvals grants` and `/approvals revoke <n>`.
func (d *Server) approvalGrantsReply(ctx context.Context, identity *control.IdentityContext, rest string) string {
	if d == nil || d.Control == nil || identity == nil {
		return "Approval store is not available."
	}
	fields := strings.Fields(rest)
	switch strings.ToLower(fields[0]) {
	case "grants", "list":
		return d.renderApprovalGrants(ctx, identity)
	case "revoke":
		if len(fields) < 2 {
			return "Usage: /approvals revoke <n>  (numbers come from /approvals grants)"
		}
		return d.revokeApprovalGrantByOrdinal(ctx, identity, fields[1])
	default:
		return "Usage: /approvals | /approvals grants | /approvals revoke <n>"
	}
}

func (d *Server) renderApprovalGrants(ctx context.Context, identity *control.IdentityContext) string {
	grants, err := d.Control.ListApprovalGrants(ctx, identity.TenantID, identity.PersonID, false)
	if err != nil {
		return "Could not read remembered approvals: " + err.Error()
	}
	if len(grants) == 0 {
		return "No remembered approvals. Every action asks."
	}
	var sb strings.Builder
	sb.WriteString("Remembered approvals (these skip the next ask):\n")
	now := time.Now()
	for i, grant := range grants {
		fmt.Fprintf(&sb, "%d. %s\n", i+1, describeApprovalGrant(grant, now))
	}
	sb.WriteString("Withdraw one with /approvals revoke <n>.")
	return sb.String()
}

func (d *Server) revokeApprovalGrantByOrdinal(ctx context.Context, identity *control.IdentityContext, token string) string {
	grants, err := d.Control.ListApprovalGrants(ctx, identity.TenantID, identity.PersonID, false)
	if err != nil {
		return "Could not read remembered approvals: " + err.Error()
	}
	ordinal, convErr := strconv.Atoi(strings.TrimSpace(token))
	if convErr != nil || ordinal < 1 || ordinal > len(grants) {
		return fmt.Sprintf("No remembered approval number %s; %d remembered (see /approvals grants).", token, len(grants))
	}
	grant := grants[ordinal-1]
	withdrawn, err := d.Control.RevokeApprovalGrant(ctx, identity.TenantID, identity.PersonID, grant.ID)
	if err != nil {
		return "Could not withdraw it: " + err.Error()
	}
	if !withdrawn {
		return "That approval was already withdrawn."
	}
	d.appendApprovalGrantEvent(ctx, identity, grant, "approval.grant_revoked")
	return "Withdrawn: " + describeApprovalGrant(grant, time.Now()) + "\nThe next matching action will ask again."
}

// describeApprovalGrant renders one remembered class in plain words.
func describeApprovalGrant(grant control.ApprovalGrant, now time.Time) string {
	scope := "this task"
	if grant.ScopeKind == "person" {
		scope = "you, across tasks"
	}
	window := "no expiry"
	if !grant.ExpiresAt.IsZero() {
		remaining := grant.ExpiresAt.Sub(now)
		if remaining <= 0 {
			window = "expired"
		} else {
			window = compactDuration(remaining) + " left"
		}
	}
	family, _ := tools.ReviewPersistedGrantKey(grant.PatternKey)
	label := humanApprovalClass(grant.PatternKey, family)
	return fmt.Sprintf("%s — remembered for %s (%s)", label, scope, window)
}

// humanApprovalClass turns a stored pattern key into something a person can
// review. The key itself is never shown: it carries a hashed workspace
// fingerprint and internal reason text.
func humanApprovalClass(patternKey, family string) string {
	key := strings.TrimSpace(patternKey)
	if strings.Contains(key, tools.HostEscapeApprovalReason) {
		if family != "" {
			return fmt.Sprintf("run %q on the host, outside the sandbox", family)
		}
		return "run commands on the host, outside the sandbox"
	}
	reason := key
	if idx := strings.Index(reason, "|resource="); idx >= 0 {
		reason = reason[:idx]
	}
	reason = strings.TrimPrefix(reason, "exec:")
	if idx := strings.Index(reason, ":"); idx >= 0 && strings.HasPrefix(key, "exec:") {
		// "invokes dangerous command: chmod" reads well as-is.
		reason = strings.TrimSpace(reason)
	}
	if family != "" {
		return fmt.Sprintf("%s (%s)", reason, family)
	}
	return reason
}

// appendApprovalGrantEvent records grant lifecycle durably. A standing
// permission appearing or disappearing is auditable state, not a log line.
func (d *Server) appendApprovalGrantEvent(ctx context.Context, identity *control.IdentityContext, grant control.ApprovalGrant, eventType string) {
	if d == nil || d.Control == nil || identity == nil {
		return
	}
	taskID := grant.ScopeID
	if grant.ScopeKind != "task" {
		current, err := d.Control.CurrentTask(ctx, identity.TenantID, identity.PersonID)
		if err != nil || current == nil {
			return
		}
		taskID = current.ID
	}
	if strings.TrimSpace(taskID) == "" {
		return
	}
	_, _ = d.Control.AppendEvent(ctx, control.Event{
		TaskID:     taskID,
		Type:       eventType,
		Visibility: "task",
		Payload: mustJSON(map[string]interface{}{
			"grant_id":   grant.ID,
			"scope_kind": grant.ScopeKind,
			"class":      humanApprovalClass(grant.PatternKey, ""),
		}),
	})
}
