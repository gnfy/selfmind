package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/delivery"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/platform/log"
	"selfmind/internal/platform/textutil"
	"selfmind/internal/tools"
)

// A Run's authority is frozen once, when its work is admitted, and every later
// hop of the same work restores it from that durable fact (control's
// AdmissionSnapshot) rather than re-deriving it from current settings or from
// the daemon's scheduling text. A new admission is a request carrying the
// person's own words or a schedule they authorized; everything the daemon
// queues to continue parked work is a continuation of an exact parent.

// newAdmission builds the snapshot a new top-level admission records: the
// mode resolved now, the strategy its own words earn, and those words. A turn
// the daemon started to continue parked work has no words of its own, and its
// parent had no snapshot to hand them on, so the request stays unrecorded
// rather than recording the scheduling text as if the person had said it.
//
// The mode is the one thing it can refuse over: it is frozen for the whole
// lineage, so a preference that cannot be read (errApprovalModeUnreadable) is
// never replaced by a guess.
func (c *RunCoordinator) newAdmission(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest, intent router.IntentResult) (control.AdmissionSnapshot, error) {
	mode, err := c.resolveApprovalMode(ctx, identity, admissionModeRequest(req))
	if err != nil {
		return control.AdmissionSnapshot{}, err
	}
	admission := control.AdmissionSnapshot{
		Version:          control.AdmissionSnapshotVersion,
		Strategy:         admissionStrategy(taskStrategyForRequest(req, intent)),
		RecoveryMode:     strings.TrimSpace(req.RecoveryMode),
		ExecutionProfile: strings.TrimSpace(req.ExecutionProfile),
		WatchID:          strings.TrimSpace(req.WatchID),
		ApprovalMode:     string(mode),
		// The moment a later tightening of the person's mode is measured
		// against; every continuation copies it.
		AdmittedAt: time.Now().Unix(),
	}
	if !continuesParkedWork(ctx, req) {
		admission.Input.Summary = strings.TrimSpace(req.Content)
	}
	// Only a schedule changes who authorized the work: it carries the classes
	// that existed when the person created it. Every other daemon trigger
	// continues work whose own snapshot already says who that was. The turn tag
	// counts as well as the request's origin: a scheduled turn must never be
	// recorded as the person's own.
	if runOrigin(ctx, req) == runOriginCron {
		admission.Origin = control.AdmissionOrigin{Kind: runOriginCron, ID: strings.TrimSpace(req.OriginID)}
		if cutoff := req.StandingGrantsAuthorizedAt; cutoff.Unix() > 0 {
			admission.StandingGrantsAuthorizedAt = cutoff.Unix()
		}
	}
	return admission, nil
}

// modeRefusal is the refusal for input whose approval mode cannot be read:
// nothing was accepted, nothing ran, and sending it again is safe. A durable
// inbound receipt may return to pending on it.
func modeRefusal(identity *control.IdentityContext, cause error) (api.MessageResponse, int, *notDispatched) {
	text := "SelfMind could not read your approval mode, so it did not start this under a guessed one. Send it again in a moment."
	return api.MessageResponse{Identity: identity, Error: text, Turn: messageTurn("failed", "", "idle", "", "", text)},
		http.StatusServiceUnavailable, &notDispatched{reason: refusalRecords, cause: cause}
}

// freezeAdmissionMode resolves, before anything is accepted, the mode a new
// admission will be frozen under, and states it on the request: a turn that was
// told it was accepted can no longer be refused for want of a readable mode. A
// request that restores an admission, or names its own mode, needs no read.
func (d *Server) freezeAdmissionMode(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest) (api.MessageRequest, error) {
	if !req.Admission.IsZero() || strings.TrimSpace(req.ApprovalMode) != "" {
		return req, nil
	}
	mode, err := d.coordinator().resolveApprovalMode(ctx, identity, "")
	if err != nil {
		return req, err
	}
	// Not req.ApprovalMode: that stays the mode the request itself named, which
	// is all that guidance and queue rows carry forward.
	req.FrozenApprovalMode = string(mode)
	return req, nil
}

// admissionModeRequest is the mode a new admission resolves from: the one the
// request named, else the one frozen at ingress, else (empty) the person's
// preference read now.
func admissionModeRequest(req api.MessageRequest) string {
	if mode := strings.TrimSpace(req.ApprovalMode); mode != "" {
		return mode
	}
	return strings.TrimSpace(req.FrozenApprovalMode)
}

// requestAdmission is the authority req runs under: the snapshot it was
// restored with, or a new admission built now.
func (c *RunCoordinator) requestAdmission(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest, intent router.IntentResult) (control.AdmissionSnapshot, error) {
	if !req.Admission.IsZero() {
		return req.Admission, nil
	}
	return c.newAdmission(ctx, identity, req, intent)
}

// coordinationAdmission is what a Main coordination Run is admitted with: no
// tools and no plan, whatever the message it routes asks for. It never parks,
// so nothing continues it; the row still says what authority it held.
func coordinationAdmission(c *RunCoordinator, identity *control.IdentityContext, req api.MessageRequest) (control.AdmissionSnapshot, error) {
	mode, err := c.resolveApprovalMode(context.Background(), identity, admissionModeRequest(req))
	if err != nil {
		return control.AdmissionSnapshot{}, err
	}
	strategy := kernel.BuildTaskStrategy("", req.Channel)
	strategy.ToolMode, strategy.PlanPolicy, strategy.WebPolicy = kernel.ToolModeNone, kernel.PlanPolicyDisabled, kernel.WebPolicyDisabled
	return control.AdmissionSnapshot{
		Version:      control.AdmissionSnapshotVersion,
		Strategy:     admissionStrategy(strategy),
		ApprovalMode: string(mode),
		Input:        control.AdmissionInput{Summary: strings.TrimSpace(req.Content)},
	}, nil
}

// requestIntent classifies a request admitted outside runMessage, as the
// dispatch path would.
func (d *Server) requestIntent(ctx context.Context, req api.MessageRequest) router.IntentResult {
	if req.ForceNew {
		return forcedNewIntent()
	}
	return d.classifyIntent(ctx, req.Content, req.Channel)
}

// queuedAdmission is the snapshot a queue row accepting req records. A mode
// that cannot be read refuses the row instead of freezing a guess into it.
func (d *Server) queuedAdmission(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest) (control.AdmissionSnapshot, error) {
	return d.coordinator().requestAdmission(ctx, identity, req, d.requestIntent(ctx, req))
}

// underAdmission makes every request field that carries authority read from
// the snapshot, so the scope, strategy and outcome paths see one answer.
func underAdmission(req api.MessageRequest, admission control.AdmissionSnapshot) api.MessageRequest {
	req.Admission = admission
	req.ApprovalMode = admission.ApprovalMode
	req.ExecutionProfile = admission.ExecutionProfile
	req.WatchID = admission.WatchID
	req.RecoveryMode = admission.RecoveryMode
	req.StandingGrantsAuthorizedAt = time.Time{}
	if admission.StandingGrantsAuthorizedAt > 0 {
		req.StandingGrantsAuthorizedAt = time.Unix(admission.StandingGrantsAuthorizedAt, 0)
	}
	return req
}

// snapshotIdentifierMaxBytes is the bound a snapshot places on identifiers. A
// channel name longer than it is a valid request, so the channel mode, which
// is the name for any channel the gateway has no special mode for and only
// labels the prompt, is cut to it rather than refusing the turn.
const snapshotIdentifierMaxBytes = 256

func admissionStrategy(strategy kernel.TaskStrategy) control.AdmissionStrategy {
	return control.AdmissionStrategy{
		Class: string(strategy.Class), ToolMode: string(strategy.ToolMode),
		PlanPolicy: string(strategy.PlanPolicy), WebPolicy: string(strategy.WebPolicy),
		ChannelMode:      textutil.TruncateBytes(strategy.ChannelMode, snapshotIdentifierMaxBytes),
		VerificationOnly: strategy.VerificationOnly,
	}
}

// admittedStrategy rebuilds the strategy a Run was admitted with.
func admittedStrategy(admission control.AdmissionSnapshot) kernel.TaskStrategy {
	s := admission.Strategy
	return kernel.AdmittedTaskStrategy(kernel.TaskClass(s.Class), kernel.ToolMode(s.ToolMode),
		kernel.PlanPolicy(s.PlanPolicy), kernel.WebPolicy(s.WebPolicy), s.ChannelMode, s.VerificationOnly)
}

// queuedTrigger names what started a queued row's hop: a daemon continuation
// trigger read from its key or approval edge, the cron queue class a scheduled
// turn is written with, or "" for the row's own source. The class and the
// snapshot's origin are written together from one rule (queuedClass and
// newAdmission both read runOrigin), so a scheduled turn's trigger and its
// recorded authority cannot disagree.
func queuedTrigger(row control.QueuedTask) string {
	switch key := row.IdempotencyKey; {
	case strings.HasPrefix(key, "external-watch:"):
		return runOriginWatch
	case strings.HasPrefix(key, "run-recovery:"):
		return runOriginRecovery
	case strings.HasPrefix(key, "external-resource:"), strings.HasPrefix(key, "execution-lane:"):
		return runOriginResource
	case strings.HasPrefix(key, "provider-wait:"):
		return runOriginProviderWait
	case strings.TrimSpace(row.ApprovalID) != "":
		return runOriginApproval
	case row.Class == control.QueueClassCron:
		return runOriginCron
	}
	return ""
}

// restoreQueuedAuthority gives a drained row's request the authority the row
// recorded. Its trigger still comes from the row (its key, its approval edge,
// or the cron queue class), so presentation and claim rules are unchanged: a
// scheduled turn queued behind busy work starts as a cron turn again. A
// transfer, choice or steering row of scheduled work keeps the schedule's
// authority but is still claimed like the person's own. A row with no snapshot
// (accepted before schema v26, deferred steering, or a continuation whose
// parent has none) keeps today's derivation from its key and class, and
// runMessage then freezes that derivation as the new Run's admission; a
// scheduled row with none has no authorisation instant to restore and consumes
// no remembered classes.
func (d *Server) restoreQueuedAuthority(ctx context.Context, req api.MessageRequest, row control.QueuedTask) api.MessageRequest {
	req.Origin = queuedTrigger(row)
	if !row.Admission.IsZero() {
		return underAdmission(req, row.Admission)
	}
	switch req.Origin {
	case runOriginWatch:
		req.WatchID = externalWatchIDFromFinalizationKey(row.IdempotencyKey)
		req.ExecutionProfile = tools.ExecutionProfileWatchFinalization
		if watch, err := d.Control.GetExternalWatch(ctx, row.TenantID, req.WatchID); err == nil && watch != nil {
			req.ExecutionProfile = externalWatchContinuationProfile(*watch)
		}
	case runOriginRecovery:
		req.RecoveryMode = recoveryModeFromQueueKey(row.IdempotencyKey)
	}
	return req
}

// continuesParkedWork reports a Run the daemon started to continue parked
// work. Its content is scheduling text, never the person's request.
func continuesParkedWork(ctx context.Context, req api.MessageRequest) bool {
	switch runOrigin(ctx, req) {
	case runOriginApproval, runOriginWatch, runOriginRecovery, runOriginResource, runOriginProviderWait:
		return true
	}
	return false
}

// exactContinuationEdge reports that the runtime itself resumes the claimed
// parent's work: an approval or clarification answer, or a daemon wake-up
// (watcher result, recovery, resource, provider wait). That is the same work
// continuing, so its Plan keeps the parent's progress. A person's turn and a
// continuation Main selected are not, however exact their parent is: Main
// decides there what still holds.
func exactContinuationEdge(ctx context.Context, req api.MessageRequest, attach taskAttach) bool {
	return attach.reason == taskAttachApprovalResume || attach.reason == taskAttachClarifyResume || continuesParkedWork(ctx, req)
}

// runInputSummary is what a Run row, /status and the work list say the work
// is: the admitted request for a daemon continuation, else the turn's words.
func runInputSummary(ctx context.Context, req api.MessageRequest) string {
	if continuesParkedWork(ctx, req) {
		if summary := strings.TrimSpace(req.Admission.Input.Summary); summary != "" {
			return truncate(summary, 240)
		}
	}
	return truncate(req.Content, 240)
}

// cronOriginID names the schedule a scheduled turn came from.
func cronOriginID(id int64) string {
	if id <= 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}

// daemonContinuationKey reports a queue row the daemon wrote to continue an
// exact parent whose authority it inherits. Approval and clarification resume
// rows copy the parked Run's snapshot inside the answer's transaction instead.
func daemonContinuationKey(key string) bool {
	for _, prefix := range []string{"provider-wait:", "execution-lane:", "external-resource:", "external-watch:", "run-recovery:"} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// continuationAdmission is the snapshot a daemon continuation row of an exact
// parent runs under: the parent's own, plus only what its trigger adds. A
// parent with none (admitted before schema v26) yields none, and the drain
// keeps today's derivation. An unreadable parent snapshot is returned as an
// error; the row then never starts.
func (d *Server) continuationAdmission(ctx context.Context, row control.QueuedTask) (control.AdmissionSnapshot, error) {
	if d == nil || d.Control == nil || !daemonContinuationKey(row.IdempotencyKey) {
		return control.AdmissionSnapshot{}, nil
	}
	parentRunID := strings.TrimSpace(row.ReplyToRunID)
	var watch *control.ExternalWatch
	if strings.HasPrefix(row.IdempotencyKey, "external-watch:") {
		found, err := d.Control.GetExternalWatch(ctx, row.TenantID, externalWatchIDFromFinalizationKey(row.IdempotencyKey))
		if err != nil || found == nil {
			return control.AdmissionSnapshot{}, err
		}
		watch = found
		// A finalization whose watcher Run already moved on is bound to its
		// Thread only; the Run that registered the watch still owns its
		// authority.
		parentRunID = strings.TrimSpace(watch.RunID)
	}
	if parentRunID == "" {
		return control.AdmissionSnapshot{}, nil
	}
	parent, err := d.Control.ExactParentAdmissionSnapshot(ctx, row.TenantID, row.PersonID, parentRunID)
	if errors.Is(err, control.ErrAdmissionRunNotFound) || (err == nil && parent.IsZero()) {
		return control.AdmissionSnapshot{}, nil
	}
	if err != nil {
		return control.AdmissionSnapshot{}, err
	}
	switch {
	case watch != nil:
		parent.WatchID = watch.ID
		// A watch whose receipt predates continuation keeps the unattended
		// finalization profile; a newer one keeps the parent's own.
		if profile := externalWatchContinuationProfile(*watch); profile != "" {
			parent.ExecutionProfile = profile
		}
	case strings.HasPrefix(row.IdempotencyKey, "run-recovery:"):
		parent = recoveryAdmission(parent, recoveryModeFromQueueKey(row.IdempotencyKey))
	}
	return parent, nil
}

// recoveryAdmission narrows the interrupted Run's authority for its recovery
// child. Verification-only recovery exposes observation tools only; a parent
// that already was verification-only never widens back.
func recoveryAdmission(parent control.AdmissionSnapshot, mode string) control.AdmissionSnapshot {
	if parent.RecoveryMode == control.RunRecoveryModeVerifyOnly {
		mode = control.RunRecoveryModeVerifyOnly
	}
	parent.RecoveryMode = mode
	if mode == control.RunRecoveryModeVerifyOnly {
		parent.Strategy.VerificationOnly = true
		if parent.Strategy.ToolMode != string(kernel.ToolModeNone) {
			parent.Strategy.ToolMode = string(kernel.ToolModeLocalRead)
		}
		// A verification turn need not produce a plan, so one that had to is
		// released from that; one with no planning tool does not gain it.
		if parent.Strategy.PlanPolicy == string(kernel.PlanPolicyRequired) {
			parent.Strategy.PlanPolicy = string(kernel.PlanPolicyOptional)
		}
	}
	return parent
}

// enqueueContinuationAdmission is what a daemon continuation row records when
// it is written. An unreadable parent records none here: the drain reads the
// parent again and holds the row with a notice instead of re-deriving it.
func (d *Server) enqueueContinuationAdmission(ctx context.Context, row control.QueuedTask) control.AdmissionSnapshot {
	admission, err := d.continuationAdmission(ctx, row)
	if err != nil {
		log.Warn("gateway: continuation parent admission unavailable; the drain will check it again",
			"key", row.IdempotencyKey, "error", err)
		return control.AdmissionSnapshot{}
	}
	return admission
}

// markHeldContinuations marks, for the /queue listing, a continuation the
// drain holds because its exact parent's snapshot cannot be read, so it is
// shown as unable to start like a row whose own snapshot cannot be read.
func (d *Server) markHeldContinuations(ctx context.Context, rows []control.QueuedTask) {
	for i := range rows {
		row := rows[i]
		if row.AdmissionError != nil || !row.Admission.IsZero() || !daemonContinuationKey(row.IdempotencyKey) {
			continue
		}
		if _, err := d.continuationAdmission(ctx, row); errors.Is(err, control.ErrAdmissionSnapshotUnreadable) {
			rows[i].AdmissionError = &control.QueuedAdmissionError{QueueID: row.ID, Err: err}
		}
	}
}

// modeNarrowing is the approval mode a Run under a restored admission executes
// with, and whether the person's tightening narrowed it.
type modeNarrowing struct {
	Mode tools.ApprovalMode
	// From is the admitted mode, set when Narrowed.
	From     tools.ApprovalMode
	Narrowed bool
	// SettingUpdatedAt is when the person's preference was last written (unix
	// seconds); zero when it could not be read.
	SettingUpdatedAt int64
	// Unreadable marks a preference that could not be read: the work then runs
	// under no more than the product default.
	Unreadable bool
}

// restoredApprovalMode is the mode work that waited — queued, or parked on a
// provider, an approval or a question — executes with: the narrower of the mode
// it was admitted with and the person's persisted /mode, and only when that
// preference was written at or after the work was first admitted. A preference
// the person already had says nothing about work they asked for under a mode of
// its own (an explicit per-run full-auto), so a bare minimum against the current
// mode would demote it after a wait. A loosening never widens. Nothing is
// written back: each hop of the lineage decides again from the original
// snapshot, so tightening and then loosening again leaves the admitted mode.
//
// When the preference cannot be read the person's current stance is unknown, so
// the work runs under no more than the product default rather than the mode it
// was admitted with. A snapshot with no admission time gives a preference no
// benefit of the doubt either. A preference written in the same second as the
// admission counts as after it: the preference store is second-granular, and
// the error that allows is on the narrowing side.
func (c *RunCoordinator) restoredApprovalMode(ctx context.Context, identity *control.IdentityContext, admission control.AdmissionSnapshot) modeNarrowing {
	admitted := tools.EffectiveApprovalMode(admission.ApprovalMode)
	if c == nil || c.srv == nil || c.srv.Control == nil || identity == nil {
		return modeNarrowing{Mode: admitted}
	}
	value, updatedAt, err := c.readApprovalModeSetting(ctx, identity)
	var current tools.ApprovalMode
	switch {
	case err != nil:
		log.Warn("gateway: the approval-mode preference could not be read; restored work runs under no more than the default",
			"person", identity.PersonID, "error", err)
		current = tools.DefaultApprovalMode
	case strings.TrimSpace(value) == "":
		return modeNarrowing{Mode: admitted}
	case admission.AdmittedAt > 0 && updatedAt < admission.AdmittedAt:
		return modeNarrowing{Mode: admitted, SettingUpdatedAt: updatedAt}
	default:
		current = tools.EffectiveApprovalMode(value)
	}
	narrowed := tools.NarrowerApprovalMode(admitted, current)
	if narrowed == admitted {
		return modeNarrowing{Mode: admitted, SettingUpdatedAt: updatedAt, Unreadable: err != nil}
	}
	return modeNarrowing{Mode: narrowed, From: admitted, Narrowed: true, SettingUpdatedAt: updatedAt, Unreadable: err != nil}
}

// noteNarrowedAuthority returns the mode the Run executes with and, when the
// person's tightening narrowed it, records that on the work's thread so the
// change from the mode the work was admitted with is visible.
func (c *RunCoordinator) noteNarrowedAuthority(ctx context.Context, identity *control.IdentityContext, task *control.Task, run *control.Run, req api.MessageRequest, narrowing modeNarrowing) tools.ApprovalMode {
	if !narrowing.Narrowed || c == nil || c.srv == nil || c.srv.Control == nil || run == nil {
		return narrowing.Mode
	}
	taskID := run.TaskID
	if task != nil {
		taskID = task.ID
	}
	reason := "approval_mode_tightened"
	if narrowing.Unreadable {
		reason = "approval_mode_unreadable"
	}
	_, err := c.srv.Control.AppendEvent(ctx, control.Event{TaskID: taskID, RunID: run.ID, TenantID: identity.TenantID, PersonID: identity.PersonID,
		Type: "run.authority_narrowed", Visibility: "task", Channel: run.Channel, IdempotencyKey: "authority-narrowed:" + run.ID,
		Payload: mustJSON(map[string]interface{}{"from": string(narrowing.From), "to": string(narrowing.Mode), "reason": reason,
			"admitted_at": req.Admission.AdmittedAt, "setting_updated_at": narrowing.SettingUpdatedAt})})
	if err != nil {
		log.Warn("gateway: the narrowing of a restored Run's mode could not be recorded", "run", run.ID, "error", err)
	}
	return narrowing.Mode
}

// defaultUnreadableNoticeRetry bounds how often a notice whose durable write or
// push failed is attempted again.
const defaultUnreadableNoticeRetry = 5 * time.Minute

// unreadableNotice is the state of one held row's notice: whether its thread
// record was written, whether its push is settled, whether the live notice
// went out, and when to try again. A push is settled once the outbox holds it
// (which sends it and retries it), or when there is nowhere to push it or the
// attached terminal was already told by the record.
type unreadableNotice struct {
	mu        sync.Mutex
	recorded  bool
	delivered bool
	published bool
	retryAt   time.Time
}

// complete reports that the notice needs no further attempt: its push is
// settled, and its thread record is written when the row belongs to a thread.
// A write the store refused, on either surface, is attempted again.
func (n *unreadableNotice) complete(onThread bool) bool {
	return n.delivered && (!onThread || n.recorded)
}

// queueNoticePush hands the push of a notice to the outbox and reports whether
// it is settled. It never sends: this runs inside the queue drain, which serves
// every person in turn and is also called inline by /approve and the watcher,
// so a messaging platform that never answers must not hold anyone's work. The
// outbox sends the message and retries it by its own bounded policy.
func (c *RunCoordinator) queueNoticePush(ctx context.Context, identity *control.IdentityContext, row control.QueuedTask, base delivery.Message, liveSurfaceInformed bool) bool {
	if c.srv == nil || c.srv.Delivery == nil {
		return true
	}
	route := c.srv.routeIdentityForPerson(ctx, row.TenantID, row.PersonID, row.Channel, row.Platform, identity)
	if route == nil {
		return true
	}
	msg := base
	if route.Platform == "cli" {
		if liveSurfaceInformed && c.srv.presenceTracker().IsAttached(route.PersonID, "cli") {
			return true
		}
		account := c.preferredIMAccount(ctx, route)
		if account == nil {
			return true
		}
		msg.Platform, msg.PlatformUserID, msg.Channel = account.Platform, account.PlatformUserID, account.PlatformUserID
	} else {
		msg.Platform, msg.PlatformUserID, msg.Channel = route.Platform, route.PlatformUserID, row.Channel
	}
	accepted, err := c.srv.Delivery.Enqueue(ctx, msg)
	if err != nil {
		log.Warn("gateway: the queued-work notice could not be recorded in the outbox; it will be retried",
			"queue_id", row.ID, "error", err)
	}
	return accepted
}

// noticeUnreadableAdmission tells the person that queued work will not start
// because the authority it was accepted with cannot be read. SelfMind never
// re-derives a possibly wider authority for it; the row holds its source until
// the person drops it.
//
// The notice is durable on two surfaces: an event on the work's own thread
// when the row has one, and a delivery to the person's preferred IM. A live
// notice reaches an attached terminal as well. A push is held back for an
// attached terminal only when the thread event was written, since only then is
// the terminal known to have a record of it; when that write fails the push
// goes out regardless, and the whole notice is attempted again after
// defaultUnreadableNoticeRetry. Both writes are idempotent, so an attempt that
// repeats one never duplicates it.
func (c *RunCoordinator) noticeUnreadableAdmission(ctx context.Context, identity *control.IdentityContext, row control.QueuedTask, cause error) {
	if _, logged := c.unreadableQueueReported.LoadOrStore(row.ID, true); !logged {
		log.Warn("gateway: queued work has an unreadable admission snapshot; its source waits until /queue drop removes it",
			"queue_id", row.ID, "person_id", row.PersonID, "error", cause)
	}
	if c.srv == nil || c.srv.Control == nil {
		return
	}
	value, _ := c.unreadableNotices.LoadOrStore(row.ID, &unreadableNotice{})
	state := value.(*unreadableNotice)
	onThread := strings.TrimSpace(row.TaskID) != ""
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.complete(onThread) || time.Now().Before(state.retryAt) {
		return
	}
	retry := c.unreadableNoticeRetry
	if retry <= 0 {
		retry = defaultUnreadableNoticeRetry
	}
	state.retryAt = time.Now().Add(retry)
	content := fmt.Sprintf("Queued work %q was not started: the authority it was accepted with cannot be read, so SelfMind will not guess one. "+
		"Use /queue to find it, remove it with /queue drop <n>, and send the request again.", textutil.Truncate(toOneLine(row.Content), 60))
	key := "admission-unreadable:" + row.ID
	if onThread && !state.recorded {
		_, err := c.srv.Control.AppendEvent(ctx, control.Event{TaskID: row.TaskID, TenantID: row.TenantID, PersonID: row.PersonID,
			Type: "queue.admission_unreadable", Visibility: "task", Channel: row.Channel, IdempotencyKey: key,
			Payload: mustJSON(map[string]string{"queue_id": row.ID, "message": content})})
		state.recorded = err == nil
		if err != nil {
			log.Warn("gateway: the unreadable-admission notice could not be recorded on its thread; it will be retried",
				"queue_id", row.ID, "task_id", row.TaskID, "error", err)
		}
	}
	if !state.published {
		state.published = true
		c.srv.events().publish(api.RunEvent{TenantID: row.TenantID, PersonID: row.PersonID, Type: "background.notice",
			Durability: api.EventEphemeral, CreatedAt: time.Now(),
			Payload: mustJSON(map[string]string{"message": content, "kind": "warning"})})
	}
	if !state.delivered {
		state.delivered = c.queueNoticePush(ctx, identity, row, delivery.Message{TenantID: row.TenantID, PersonID: row.PersonID,
			TaskID: row.TaskID, Kind: delivery.KindRecovery, Content: content, LogicalKey: key}, state.recorded)
	}
}
