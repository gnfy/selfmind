package httpapi

import (
	"context"
	"net/http"
	"strings"

	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/command"
	"selfmind/internal/platform/log"
	"selfmind/internal/platform/textutil"
)

// processDaemonTurn admits a turn the daemon started on a person's behalf. A
// schedule is the one source that arrives here; watcher, recovery, approval,
// and wakeup work drains from the queue straight into a Run. Its text was
// stored earlier, often in the model's own words, so it carries no authority:
//
//   - it acts only through an account that already exists and never creates
//     one, so it cannot introduce a person;
//   - it is not the person at an endpoint, so it neither beats presence nor
//     lets the IM catch-up replay an unconfirmed push;
//   - its text is task input, never a command, an approval or clarification
//     answer, a choice answer, or a continuity control.
//
// Text shaped like a command is refused with a notice instead of being handed
// to the agent: as a task it would ask the model to improvise around a control
// the gateway will not run, and the person who scheduled it learns nothing.
func (d *Server) processDaemonTurn(ctx context.Context, req api.MessageRequest) (api.MessageResponse, int, *notDispatched) {
	identity, err := d.Control.ResolveAccount(ctx, req.TenantID, req.Platform, req.PlatformUserID)
	if err != nil {
		return api.MessageResponse{Error: err.Error(), Turn: messageTurn("failed", "", "", "", "", err.Error())}, http.StatusInternalServerError, nil
	}
	if identity == nil {
		msg := "No account " + req.Platform + ":" + req.PlatformUserID + " is bound here, so this scheduled turn has no person to act for."
		return api.MessageResponse{Error: msg, Turn: messageTurn("failed", "", "", "", "", msg)}, http.StatusNotFound, nil
	}
	if creator, ok := scheduleCreatorFromContext(ctx); ok && (identity.TenantID != creator.tenantID || identity.PersonID != creator.personID) {
		msg := "This scheduled turn was not run: its account no longer belongs to the person who created it."
		log.Warn("scheduled account changed before admission", "origin", runOrigin(ctx, req))
		return api.MessageResponse{Error: msg, Turn: messageTurn("failed", "", "idle", "", "", msg)}, http.StatusForbidden, nil
	}
	if command.LooksLikeCommand(req.Content) {
		notice := scheduledCommandNotice(req.Content)
		log.Warn("scheduled text refused", "person_id", identity.PersonID, "origin", runOrigin(ctx, req), "reason", notice)
		return api.MessageResponse{Identity: identity, Content: notice,
			Turn: messageTurn("completed", "", "idle", "", "", "")}, http.StatusOK, nil
	}
	return d.admitTurn(ctx, identity, req)
}

// scheduledCommandNotice tells the person why scheduled text that begins with
// a command did nothing, and what to do instead.
func scheduledCommandNotice(content string) string {
	name := textutil.Truncate(strings.Fields(strings.TrimSpace(content))[0], 40)
	return "Scheduled text was not run: " + name + " is a SelfMind command, and commands run only from your own messages. " +
		"Send it yourself if you meant it, or schedule the task described in words."
}
