package httpapi

import (
	"context"
	"strings"

	"selfmind/internal/control"
)

// Inbound answer routing for pending questions (G3, docs/identity-continuity.md
// "Runtime attachment model"). While a run blocks on a clarify_requests row, a
// plain non-command reply from the person IS the answer — it must resolve the
// question rather than be queued as new work or steered into the running task.
//
// Precedence in the inbound stack (tryHandleControlCommand): a bare y/n
// approval reply is tried FIRST, so if an approval is somehow also pending it
// wins for y/n-looking input; a pending clarify then claims any remaining
// non-slash free text. In practice the per-person active-run guard serializes
// blocks (one run blocks on one thing at a time), so an approval and a clarify
// are never simultaneously waiting for the same person — the defensive ordering
// only matters if that invariant is ever violated. Slash commands are never
// treated as answers, so /status, /stop, etc. still work while blocked.

// tryHandleClarifyAnswer resolves a free-text reply against the person's pending
// question. It claims the message only when (1) the content is not a slash
// command and (2) at least one clarify is pending; otherwise it returns
// handled=false so the message flows on to the agent / queue as usual. When
// several are pending (only possible with parallel runs), the oldest — the one
// the person has been waiting on longest, first in ListClarifyRequests order —
// is answered.
func (d *Server) tryHandleClarifyAnswer(ctx context.Context, identity *control.IdentityContext, content, channel string) (bool, string, error) {
	if d == nil || d.Control == nil || identity == nil {
		return false, "", nil
	}
	answer := strings.TrimSpace(content)
	if answer == "" || strings.HasPrefix(answer, "/") {
		// Slash messages are control commands, never answers.
		return false, "", nil
	}
	pending, err := d.Control.ListClarifyRequests(ctx, identity.TenantID, identity.PersonID, "pending", 100)
	if err != nil || len(pending) == 0 {
		// Store error or nothing pending: not an answer — let the message flow to
		// the agent / continuation handling / queue unchanged.
		return false, "", nil
	}
	clarify, err := d.Control.AnswerClarifyRequest(ctx, identity.TenantID, identity.PersonID, pending[0].ID, answer, channel)
	if err != nil {
		return true, "Could not record your answer: " + err.Error(), nil
	}
	_ = clarify
	return true, "Got it — continuing with your answer.", nil
}
