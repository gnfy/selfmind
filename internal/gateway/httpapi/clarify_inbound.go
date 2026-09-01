package httpapi

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/command"
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
// handled=false so the message flows on to the agent / queue as usual.
//
// Exactly one pending question is answered structurally — it is the only one,
// not "the oldest". More than one pending (possible across parked runs) is
// disambiguated deterministically: the person gets a numbered list and picks
// with an "N: answer" prefix. Blind oldest-pending claiming is gone
// (simplification P1): an answer must never land on a question the person
// did not mean.
func (d *Server) tryHandleClarifyAnswer(ctx context.Context, identity *control.IdentityContext, clarifyID, content, channel string) (bool, string, error) {
	if d == nil || d.Control == nil || identity == nil {
		return false, "", nil
	}
	answer := strings.TrimSpace(content)
	if answer == "" || command.LooksLikeCommand(answer) {
		// Command-shaped messages are control commands, never answers. A
		// "/"-leading file path IS a valid answer (the agent may have asked
		// "which file?"), so the gate is command shape, not the bare prefix.
		return false, "", nil
	}
	if clarifyID = strings.TrimSpace(clarifyID); clarifyID != "" {
		target, err := d.Control.GetClarifyRequest(ctx, identity.TenantID, clarifyID)
		if err != nil {
			return true, "", fmt.Errorf("load clarification reply target: %w", err)
		}
		if target == nil || target.PersonID != identity.PersonID || target.Status != "pending" {
			return true, "That clarification is no longer pending; I did not apply the answer to another question.", nil
		}
		if _, err := d.answerClarifyTarget(ctx, identity, target, answer, channel); err != nil {
			return true, clarifyAnswerFailure(err), nil
		}
		return true, "Got it — continuing with your answer.", nil
	}
	pending, err := d.Control.ListClarifyRequests(ctx, identity.TenantID, identity.PersonID, "pending", 100)
	if err != nil || len(pending) == 0 {
		// Store error or nothing pending: not an answer — let the message flow to
		// the agent / continuation handling / queue unchanged.
		return false, "", nil
	}
	// Number questions oldest-first: the prefix numbering must stay stable if
	// another question arrives between the list and the person's pick (new
	// questions append at the end instead of shifting every number). The id
	// tie-break keeps the numbering identical across the listing turn and the
	// picking turn when timestamps collide.
	sort.SliceStable(pending, func(i, j int) bool {
		if pending[i].CreatedAt.Equal(pending[j].CreatedAt) {
			return pending[i].ID < pending[j].ID
		}
		return pending[i].CreatedAt.Before(pending[j].CreatedAt)
	})
	target := pending[0]
	if len(pending) > 1 {
		picked, rest, ok := pickNumberedClarify(answer, pending)
		if !ok {
			var sb strings.Builder
			sb.WriteString("Several questions are waiting. Prefix your answer with the question number (for example \"1: use the staging bucket\"):\n")
			for i, clarify := range pending {
				if i >= 5 {
					break
				}
				question := strings.TrimSpace(clarify.Question)
				if question == "" {
					question = "(no question text)"
				}
				fmt.Fprintf(&sb, "%d. [%s] %s\n", i+1, shortRunID(clarify.RunID), truncate(question, 120))
			}
			return true, strings.TrimRight(sb.String(), "\n"), nil
		}
		target = picked
		answer = rest
	}
	clarify, err := d.answerClarifyTarget(ctx, identity, &target, answer, channel)
	if err != nil {
		return true, clarifyAnswerFailure(err), nil
	}
	_ = clarify
	return true, "Got it — continuing with your answer.", nil
}

func clarifyAnswerFailure(err error) string {
	if errors.Is(err, control.ErrClarifyOriginUnavailable) {
		return "That question's original run is no longer waiting for an answer; I did not apply it to different work."
	}
	return "Could not record your answer: " + err.Error()
}

func (d *Server) answerClarifyTarget(ctx context.Context, identity *control.IdentityContext, target *control.ClarifyRequest, answer, channel string) (*control.ClarifyRequest, error) {
	if d == nil || d.Control == nil || identity == nil || target == nil {
		return nil, fmt.Errorf("clarification answer target is unavailable")
	}
	queuedInput := control.QueuedTask{
		PersonID: identity.PersonID, Platform: identity.Platform,
		PlatformUserID: identity.PlatformUserID, Channel: fallback(channel, identity.Platform),
		Content: answer, TaskID: target.TaskID, ClarifyID: target.ID,
		IdempotencyKey: "clarify-resume:" + target.ID,
		Class:          control.QueueClassForeground,
	}
	if task, err := d.Control.GetTask(ctx, identity.TenantID, target.TaskID); err != nil {
		return nil, err
	} else if task != nil {
		queuedInput.WorkspaceID = task.WorkspaceID
	}
	if sourceRun, err := d.Control.GetRun(ctx, identity.TenantID, target.RunID); err != nil {
		return nil, err
	} else if sourceRun != nil {
		queuedInput.ExecutionRoots = executionenv.CloneRootBindings(sourceRun.ExecutionRoots)
		if queuedInput.WorkspaceID == "" {
			queuedInput.WorkspaceID = sourceRun.WorkspaceID
		}
	}
	clarify, queued, err := d.Control.AnswerClarifyRequestWithResume(ctx,
		identity.TenantID, identity.PersonID, target.ID, answer, channel, queuedInput)
	if err != nil {
		return nil, err
	}
	if queued != nil {
		// The answer and queue row are already one durable transaction. If no
		// run is active, drain now; otherwise the active finalizer drains it.
		d.coordinator().drainQueue(identity)
	}
	return clarify, nil
}

// pickNumberedClarify parses the "N: answer" disambiguation prefix. It accepts
// ASCII and CJK colons plus a period separator, and requires N to name one of
// the listed pending questions; anything else is not a pick.
func pickNumberedClarify(answer string, pending []control.ClarifyRequest) (control.ClarifyRequest, string, bool) {
	for _, sep := range []string{":", "：", ".", "。", " "} {
		before, after, found := strings.Cut(answer, sep)
		if !found {
			continue
		}
		index := 0
		before = strings.TrimSpace(before)
		if before == "" || len(before) > 2 {
			continue
		}
		for _, r := range before {
			if r < '0' || r > '9' {
				index = -1
				break
			}
			index = index*10 + int(r-'0')
		}
		rest := strings.TrimSpace(after)
		if index >= 1 && index <= len(pending) && rest != "" {
			return pending[index-1], rest, true
		}
	}
	return control.ClarifyRequest{}, "", false
}
