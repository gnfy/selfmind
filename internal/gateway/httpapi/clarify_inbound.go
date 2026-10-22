package httpapi

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/command"
)

// Inbound answer routing for pending questions (G3, docs/identity-continuity.md
// "Runtime attachment model"). While a run blocks on a clarify_requests row, a
// plain non-command reply from the person IS the answer — it must resolve the
// question rather than be queued as new work or steered into the running task.
// The exceptions are another open terminal's question (a plain message typed in
// a different terminal is that terminal's own input, "Session audience") and a
// question that has waited staleClarifyAfter with its asking terminal gone.
//
// Precedence in the inbound stack (tryHandleControlCommand): a bare y/n
// approval reply is tried FIRST, so if an approval is somehow also pending it
// wins for y/n-looking input; a pending clarify then claims any remaining
// non-slash free text. In practice the per-person active-run guard serializes
// blocks (one run blocks on one thing at a time), so an approval and a clarify
// are never simultaneously waiting for the same person — the defensive ordering
// only matters if that invariant is ever violated. Slash commands are never
// treated as answers, so /status, /stop, etc. still work while blocked.

// staleClarifyAfter is how long a question may wait, with the session that asked
// it gone, before a plain reply from another session stops answering it. The
// reply then takes the named form "N: answer", as it already does while the
// asking terminal is open, and anything else is new work. A person who reopens a
// terminal hours later and types a new request must not have it taken for the
// answer to a question they have long forgotten: on 2026-10-14 a release request
// became the answer to a question asked 2h35m earlier and resumed that old
// thread, with its plan, instead of starting the new work.
const staleClarifyAfter = 30 * time.Minute

// clarifyAckReassureAfter is the age from which an acknowledgement of an answer
// given from another session also says how to undo the choice.
const clarifyAckReassureAfter = 2 * time.Minute

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
		return true, clarifyAnswerAck(*target, channel, time.Now()), nil
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
	// A question another open terminal asked is that terminal's to answer with
	// plain text: a message typed here is this session's own input, and here
	// the question takes the named form "N: answer". With no other terminal
	// open on a question — one window, or an IM question — any plain reply
	// answers, as before. Numbers always count the whole list, so a listing and
	// the reply that picks from it agree.
	now := time.Now()
	answerable := make(map[string]bool, len(pending))
	for _, clarify := range pending {
		if d.clarifyAnswersImplicitly(identity, channel, clarify, now) {
			answerable[clarify.ID] = true
		}
	}
	if len(answerable) < len(pending) {
		if picked, rest, named := pickNamedClarify(answer, pending); named {
			target, answer = picked, rest
			pending = []control.ClarifyRequest{picked}
		} else if len(answerable) == 0 {
			return false, "", nil
		} else if _, _, numbered := pickClarifyNumber(answer, pending, ":", "：", ".", "。"); len(answerable) == 1 && !numbered {
			// An unnumbered reply answers this session's one question. A
			// numbered one is read against the whole list below, like the
			// listing it answers, so it never lands on a different question.
			// "2 buckets" is an answer here: a bare space picks a question
			// only once a listing has asked for a number.
			for _, clarify := range pending {
				if answerable[clarify.ID] {
					target = clarify
				}
			}
			pending = []control.ClarifyRequest{target}
		}
	}
	if len(pending) > 1 {
		picked, rest, ok := pickNumberedClarify(answer, pending)
		if !ok || !answerable[picked.ID] {
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
	return true, clarifyAnswerAck(target, channel, time.Now()), nil
}

// clarifyAnswerAck confirms WHICH question the reply answered and how long it
// had waited, so a reply that was meant as something else is noticed at once
// rather than after the resumed run has worked for minutes. An answer given
// from another session to a question that had waited a while also says how to
// undo the choice.
func clarifyAnswerAck(target control.ClarifyRequest, replyChannel string, now time.Time) string {
	ack := "Got it — continuing with your answer"
	if question := truncate(toOneLine(target.Question), 80); question != "" {
		ack += " to “" + question + "”"
	}
	age := now.Sub(target.CreatedAt)
	if age >= time.Minute {
		ack += " (asked " + askedAge(age) + " ago)"
	}
	ack += "."
	if asked := strings.TrimSpace(target.Channel); asked != "" && asked != strings.TrimSpace(replyChannel) && age >= clarifyAckReassureAfter {
		ack += " Not what you meant? /stop it, then start new work with /new --run <request>."
	}
	return ack
}

// askedAge renders how long a question has waited: 35m, 2h35m, 3d.
func askedAge(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
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
		queuedInput.WorkspaceID = recoveryWorkspaceID(sourceRun, &control.Task{WorkspaceID: queuedInput.WorkspaceID})
		checkpoint, _ := d.Control.IncompleteLoopCheckpointForRun(ctx, identity.TenantID, sourceRun.ID)
		if checkpoint != nil && checkpoint.Outcome == "await_human" && checkpoint.WaitKind == control.LoopCheckpointWaitClarification && checkpoint.WaitRef == target.ID {
			queuedInput.Platform, queuedInput.PlatformUserID, queuedInput.Channel = d.humanWaitContinuationRoute(ctx, identity, sourceRun, control.LoopCheckpointWaitClarification, target.ID, channel)
		}
		queuedInput.ReplyToRunID = sourceRun.ID
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

// pickNamedClarify reads only the unmistakable "N: answer" form, which may
// answer a question another terminal asked. The looser forms below stay for
// choosing among this session's own questions.
func pickNamedClarify(answer string, pending []control.ClarifyRequest) (control.ClarifyRequest, string, bool) {
	for _, sep := range []string{":", "："} {
		before, after, found := strings.Cut(answer, sep)
		if !found || !clarifyPrefixSeparated(sep, after) {
			continue
		}
		index, err := strconv.Atoi(strings.TrimSpace(before))
		rest := strings.TrimSpace(after)
		if err == nil && index >= 1 && index <= len(pending) && rest != "" {
			return pending[index-1], rest, true
		}
	}
	return control.ClarifyRequest{}, "", false
}

// clarifyPrefixSeparated keeps "1:30" and "1.5" answers: an ASCII colon or
// period names a question only when a space follows it, as in "1: staging".
func clarifyPrefixSeparated(sep, after string) bool {
	return (sep != ":" && sep != ".") || strings.HasPrefix(after, " ")
}

// answersImplicitly reports whether a plain reply from replyChannel may answer
// a human wait raised in waitChannel: its own session always, another only
// while no terminal is open as that session.
func (d *Server) answersImplicitly(identity *control.IdentityContext, replyChannel, waitChannel string) bool {
	waitChannel = strings.TrimSpace(waitChannel)
	if identity == nil || waitChannel == "" || waitChannel == strings.TrimSpace(replyChannel) {
		return true
	}
	return !d.events().sessionAttached(identity.PersonID, waitChannel)
}

// clarifyAnswersImplicitly is answersImplicitly plus the age rule: a question
// whose asking session is gone and that has waited staleClarifyAfter no longer
// takes a plain reply from another session. The asking session itself always
// may, since it has the question in front of it. Approvals keep the plain rule.
func (d *Server) clarifyAnswersImplicitly(identity *control.IdentityContext, replyChannel string, clarify control.ClarifyRequest, now time.Time) bool {
	if !d.answersImplicitly(identity, replyChannel, clarify.Channel) {
		return false
	}
	asked := strings.TrimSpace(clarify.Channel)
	if asked == "" || asked == strings.TrimSpace(replyChannel) {
		return true
	}
	return now.Sub(clarify.CreatedAt) < staleClarifyAfter
}

// pickNumberedClarify parses the "N: answer" disambiguation prefix. It accepts
// ASCII and CJK colons and periods or a space after N, and requires N to name
// one of the listed pending questions; anything else is not a pick.
func pickNumberedClarify(answer string, pending []control.ClarifyRequest) (control.ClarifyRequest, string, bool) {
	return pickClarifyNumber(answer, pending, ":", "：", ".", "。", " ")
}

// pickClarifyNumber reads "N<separator>answer" for the given separators.
func pickClarifyNumber(answer string, pending []control.ClarifyRequest, separators ...string) (control.ClarifyRequest, string, bool) {
	for _, sep := range separators {
		before, after, found := strings.Cut(answer, sep)
		if !found || !clarifyPrefixSeparated(sep, after) {
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
