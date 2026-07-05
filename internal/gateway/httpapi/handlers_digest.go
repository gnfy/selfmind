package httpapi

// GET /v1/digest — the attach digest (G0-c, docs/identity-continuity.md
// "Runtime attachment model"): one person-scoped query answering "what
// happened while this endpoint was away". The anchor is the requesting
// account's durable accounts.last_seen_at (stamped by presence beats, G0-b);
// an account that has never been seen falls back to a 24h window. This
// handler is read-only on purpose: it must NOT touch presence, because the
// client fetches the digest before its first presence beat — a touch here
// would move the anchor to "now" and erase the digest it is computing.

import (
	"context"
	"net/http"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/tools"
)

const (
	// digestFallbackWindow bounds the digest for accounts with no recorded
	// presence (fresh install, pre-G0-b database): report the last day, not
	// all of history.
	digestFallbackWindow = 24 * time.Hour
	// maxDigestTasks / maxDigestPushes keep the digest a glanceable summary,
	// never a report dump (the /tasks and /status commands are the full views).
	maxDigestTasks  = 10
	maxDigestPushes = 5
	// digestSummaryChars bounds each task summary line.
	digestSummaryChars = 160
)

func (d *Server) handleDigest(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	identity, err := d.identityFromQuery(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	digest, err := d.buildDigest(r.Context(), identity)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, digest)
}

// buildDigest assembles the bounded attach digest for the requesting account.
// Pending approvals and the active run are point-in-time state (not anchored:
// still pending / still running is what matters); finished, disrupted, and
// unconfirmed-push sections are anchored on the account's last presence.
func (d *Server) buildDigest(ctx context.Context, identity *control.IdentityContext) (api.DigestResponse, error) {
	out := api.DigestResponse{Identity: identity}
	since := time.Now().Add(-digestFallbackWindow)
	accounts, err := d.Control.ListAccountsByPerson(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		return out, err
	}
	for _, account := range accounts {
		if account.ID == identity.AccountID && account.LastSeenAt > 0 {
			since = time.Unix(account.LastSeenAt, 0)
		}
	}
	out.SinceUnix = since.Unix()

	finished, err := d.Control.ListTasksByStatusSince(ctx, identity.TenantID, identity.PersonID, []string{"done", "completed"}, since, maxDigestTasks)
	if err != nil {
		return out, err
	}
	out.FinishedTasks = digestTasks(finished)

	disrupted, err := d.Control.ListTasksByStatusSince(ctx, identity.TenantID, identity.PersonID, []string{"failed", "interrupted"}, since, maxDigestTasks)
	if err != nil {
		return out, err
	}
	out.DisruptedTasks = digestTasks(disrupted)

	approvals, titles, err := d.pendingApprovalsForDisplay(ctx, identity)
	if err != nil {
		return out, err
	}
	for _, approval := range approvals {
		out.PendingApprovals = append(out.PendingApprovals, api.DigestApproval{
			ID:   approval.ID,
			Line: approvalSummaryLine(approval, titles[approval.TaskID]),
		})
	}

	// Pending questions are point-in-time state like approvals: still-waiting is
	// what matters, so this is not anchored on last presence.
	clarifies, err := d.Control.ListClarifyRequests(ctx, identity.TenantID, identity.PersonID, "pending", maxDigestTasks)
	if err != nil {
		return out, err
	}
	for _, clarify := range clarifies {
		out.PendingClarifies = append(out.PendingClarifies, api.DigestClarify{
			ID:   clarify.ID,
			Line: clarifySummaryLine(clarify),
		})
	}

	pushes, err := d.Control.ListUndeliveredOutbound(ctx, identity.TenantID, identity.PersonID, since, maxDigestPushes)
	if err != nil {
		return out, err
	}
	for _, push := range pushes {
		out.UnconfirmedPushes = append(out.UnconfirmedPushes, api.DigestPush{
			Platform: push.Platform,
			Status:   push.Status,
			Preview:  truncate(toOneLine(push.Content), 80),
		})
	}

	if active := d.coordinator().currentActive(identity.PersonID); active != nil {
		run := &api.DigestActiveRun{
			TaskID:         active.TaskID,
			Title:          strings.TrimSpace(active.Summary),
			ElapsedSeconds: int64(time.Since(active.StartedAt).Seconds()),
		}
		// Prefer the task title over the raw input summary — best-effort, the
		// summary is an honest fallback while the task row is still resolving.
		if active.TaskID != "" {
			if task, err := d.Control.GetTask(ctx, identity.TenantID, active.TaskID); err == nil && task != nil && strings.TrimSpace(task.Title) != "" {
				run.Title = strings.TrimSpace(task.Title)
			}
		}
		out.ActiveRun = run
	}

	// Effective approval mode: the person's persisted /mode preference, or
	// on-request when unset. Lets a client show the current mode in its status
	// bar from startup instead of guessing the local default.
	mode := ""
	if pref, err := d.Control.GetPersonSetting(ctx, identity.TenantID, identity.PersonID, personSettingApprovalMode); err == nil {
		mode = strings.TrimSpace(pref)
	}
	out.ApprovalMode = string(tools.NormalizeApprovalMode(mode))

	return out, nil
}

func digestTasks(tasks []control.Task) []api.DigestTask {
	out := make([]api.DigestTask, 0, len(tasks))
	for _, task := range tasks {
		out = append(out, api.DigestTask{
			ID:      task.ID,
			Title:   task.Title,
			Status:  task.Status,
			Summary: truncate(toOneLine(task.CurrentSummary), digestSummaryChars),
		})
	}
	return out
}
