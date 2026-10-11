package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// External effect claim lifecycle.
//
// A claim row is the authority for a target; its state says how far the call
// got (reserved before dispatch, uncertain after it, observed once released).
// Two durable records, written as internal Run events, add what the row alone
// cannot say:
//
//   - external_effect.dispatched is written after the claim is granted and
//     before the tool body starts. It carries a bounded, redacted summary of
//     the call and whether the call starts a background process.
//   - external_effect.returned is written when the call returns without
//     proving success (failure, timeout, cancellation, an external tool error,
//     an asynchronous adapter, or a background process that exited non-zero).
//
// A successful return settles the claim instead (observation ref
// tool:completed:<call> or process:exited:0:...). The disposition of an
// unobserved claim is derived from these facts and the owner Run, and is never
// stored:
//
//   - in flight: reserved, or dispatched without a return record while the
//     owner Run is still running. Boot recovery marks a crashed daemon's
//     running Runs interrupted, so an unreturned call of a crashed owner is no
//     longer in flight. A background start is in flight until its exit is
//     recorded for as long as the daemon process that started it lives, even
//     after its owner Run stops: only that process can observe the exit. Boot
//     recovery records a return with an unknown exit for a background start of
//     an earlier daemon (RecordAbandonedBackgroundEffects).
//   - watched: not in flight, and a live watcher bound to the effect can still
//     release it.
//   - left uncertain: everything else that is unobserved: the call returned
//     without proof, or its owner stopped running before it returned.
//
// The requesting Run's own chain never treats its own foreground call as in
// flight: it is making another call, so that call has returned even if its
// return record could not be written.
const (
	ExternalClaimInFlight      = "in_flight"
	ExternalClaimWatched       = "watched"
	ExternalClaimLeftUncertain = "left_uncertain"
)

// Internal Run event types that record an effect's lifecycle.
const (
	ExternalEffectDispatchedEvent  = "external_effect.dispatched"
	ExternalEffectReturnedEvent    = "external_effect.returned"
	ExternalEffectObservedEvent    = "external_effect.observed"
	ExternalEffectObservationSpent = "external_effect.observation_consumed"
)

const externalEffectSummaryMaxRunes = 160

// ErrExternalEffectInFlight reports a claim whose call has not returned while
// its owner Run is live. Nothing can know its outcome yet.
var ErrExternalEffectInFlight = errors.New("external effect is still in flight")

func externalEffectDispatchKey(runID, effectID string) string {
	return "external-effect-dispatched:" + runID + ":" + effectID
}

func externalEffectReturnKey(runID, effectID string) string {
	return "external-effect-returned:" + runID + ":" + effectID
}

// ExternalEffectDispatch is what the claim gate records before a claimed call
// starts. Summary must already be redacted; it is bounded again here.
type ExternalEffectDispatch struct {
	TenantID, PersonID, RunID, EffectID string
	ToolCallID, ToolName, Summary       string
	Background                          bool
}

// ExternalEffectReturn records a return that did not prove success. ExitCode
// is nil when no exit status was observed.
type ExternalEffectReturn struct {
	TenantID, PersonID, RunID, EffectID string
	ToolCallID, Outcome                 string
	ExitCode                            *int
}

type externalEffectDispatchPayload struct {
	EffectID   string `json:"effect_id"`
	ToolCallID string `json:"tool_call_id"`
	ToolName   string `json:"tool_name"`
	Summary    string `json:"summary"`
	Background bool   `json:"background,omitempty"`
	// Instance names the daemon process that started a background effect.
	Instance string `json:"instance,omitempty"`
}

type externalEffectReturnPayload struct {
	EffectID   string `json:"effect_id"`
	ToolCallID string `json:"tool_call_id"`
	Outcome    string `json:"outcome"`
	ExitCode   *int   `json:"exit_code,omitempty"`
}

// RecordExternalEffectDispatch writes the dispatch record of a claimed effect.
// It runs before MarkExternalEffectPossible; a failure leaves the reservation
// undispatched, so the caller releases it and does not run the tool.
func (s *Store) RecordExternalEffectDispatch(ctx context.Context, dispatch ExternalEffectDispatch) error {
	if s == nil || s.db == nil || strings.TrimSpace(dispatch.RunID) == "" || strings.TrimSpace(dispatch.EffectID) == "" {
		return fmt.Errorf("external effect dispatch identity is required")
	}
	record := externalEffectDispatchPayload{EffectID: dispatch.EffectID, ToolCallID: dispatch.ToolCallID,
		ToolName: dispatch.ToolName, Summary: boundEffectSummary(dispatch.Summary), Background: dispatch.Background}
	if dispatch.Background {
		record.Instance = s.instance
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.recordExternalEffectEvent(ctx, dispatch.TenantID, dispatch.PersonID, dispatch.RunID, dispatch.EffectID,
		ExternalEffectDispatchedEvent, externalEffectDispatchKey(dispatch.RunID, dispatch.EffectID), payload, ExternalClaimReserved)
}

// RecordExternalEffectReturn marks a dispatched call as returned without proof
// of success, which ends its in-flight period. The claim stays uncertain.
func (s *Store) RecordExternalEffectReturn(ctx context.Context, ret ExternalEffectReturn) error {
	if s == nil || s.db == nil || strings.TrimSpace(ret.RunID) == "" || strings.TrimSpace(ret.EffectID) == "" {
		return fmt.Errorf("external effect return identity is required")
	}
	payload, err := json.Marshal(externalEffectReturnPayload{EffectID: ret.EffectID, ToolCallID: ret.ToolCallID,
		Outcome: boundEffectSummary(ret.Outcome), ExitCode: ret.ExitCode})
	if err != nil {
		return err
	}
	return s.recordExternalEffectEvent(ctx, ret.TenantID, ret.PersonID, ret.RunID, ret.EffectID,
		ExternalEffectReturnedEvent, externalEffectReturnKey(ret.RunID, ret.EffectID), payload, ExternalClaimUncertain)
}

// abandonedBackgroundOutcome is how a background start ends when the daemon
// that could observe its exit is gone.
const abandonedBackgroundOutcome = "background process exit unknown: the daemon that started it stopped"

// RecordAbandonedBackgroundEffects is boot recovery for background starts: an
// unreturned background effect started by an earlier daemon process can no
// longer have its exit observed, so it is recorded as returned with an unknown
// exit and stays uncertain. Recording is idempotent.
func (s *Store) RecordAbandonedBackgroundEffects(ctx context.Context) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("external effect store is unavailable")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT c.tenant_id, c.person_id, c.run_id, c.effect_id, d.payload_json
		FROM external_effect_claims c
		JOIN task_events d ON d.idempotency_key='external-effect-dispatched:'||c.run_id||':'||c.effect_id
		WHERE c.state='uncertain' AND json_extract(d.payload_json,'$.background')=1
		AND NOT EXISTS (SELECT 1 FROM task_events x WHERE x.idempotency_key='external-effect-returned:'||c.run_id||':'||c.effect_id)`)
	if err != nil {
		return 0, err
	}
	var abandoned []ExternalEffectReturn
	for rows.Next() {
		var ret ExternalEffectReturn
		var payload string
		if err := rows.Scan(&ret.TenantID, &ret.PersonID, &ret.RunID, &ret.EffectID, &payload); err != nil {
			_ = rows.Close()
			return 0, err
		}
		var dispatch externalEffectDispatchPayload
		if json.Unmarshal([]byte(payload), &dispatch) == nil && dispatch.Instance == s.instance {
			continue
		}
		ret.ToolCallID, ret.Outcome = dispatch.ToolCallID, abandonedBackgroundOutcome
		abandoned = append(abandoned, ret)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, ret := range abandoned {
		if err := s.RecordExternalEffectReturn(ctx, ret); err != nil {
			return 0, err
		}
	}
	return len(abandoned), nil
}

func (s *Store) recordExternalEffectEvent(ctx context.Context, tenantID, personID, runID, effectID, eventType, key string, payload []byte, wantState string) error {
	tenantID = normalizeTenant(tenantID)
	for attempt := 0; attempt < 5; attempt++ {
		err := func() error {
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			var matching int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM external_effect_claims
				WHERE tenant_id=? AND person_id=? AND run_id=? AND effect_id=? AND state=?`,
				tenantID, personID, runID, effectID, wantState).Scan(&matching); err != nil {
				return err
			}
			if matching == 0 {
				return fmt.Errorf("external effect claim is not %s", wantState)
			}
			if _, _, err := insertEffectEventTx(ctx, tx, tenantID, runID, eventType, payload, key); err != nil {
				return err
			}
			return tx.Commit()
		}()
		if !isSQLiteBusy(err) || ctx.Err() != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 5 * time.Millisecond):
		}
	}
	return fmt.Errorf("external effect record remained busy")
}

// insertEffectEventTx appends one internal event to the Run's thread inside
// the caller's transaction. The cursor comes from the shared effect sequence,
// so lifecycle records order with claims and watchers. A duplicate key keeps
// the first record.
func insertEffectEventTx(ctx context.Context, tx *sql.Tx, tenantID, runID, eventType string, payload []byte, key string) (bool, int64, error) {
	var threadID string
	if err := tx.QueryRowContext(ctx, `SELECT thread_id FROM runs WHERE tenant_id=? AND id=?`, tenantID, runID).Scan(&threadID); err != nil {
		return false, 0, fmt.Errorf("effect record needs its Run: %w", err)
	}
	cursor, err := nextEffectSequenceTx(ctx, tx)
	if err != nil {
		return false, 0, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO task_events
		(id, cursor, thread_id, run_id, type, visibility, channel, payload_json, idempotency_key, created_at)
		VALUES (?, ?, ?, ?, ?, 'internal', '', ?, ?, ?)
		ON CONFLICT(idempotency_key) WHERE idempotency_key IS NOT NULL AND idempotency_key != '' DO NOTHING`,
		"event_"+uuid.NewString(), cursor, threadID, runID, eventType, string(payload), key, time.Now().Unix())
	if err != nil {
		return false, 0, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, 0, err
	}
	return inserted == 1, cursor, nil
}

func boundEffectSummary(text string) string {
	text = strings.Join(strings.Fields(strings.ToValidUTF8(text, "")), " ")
	if runes := []rune(text); len(runes) > externalEffectSummaryMaxRunes {
		text = string(runes[:externalEffectSummaryMaxRunes-1]) + "…"
	}
	return text
}

// AllocateEffectSequence takes one value of the shared claim/watcher/event
// order. An observation takes it before it runs, so it can later prove that it
// started after the effect it is bound to.
func (s *Store) AllocateEffectSequence(ctx context.Context) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("external effect store is unavailable")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	sequence, err := nextEffectSequenceTx(ctx, tx)
	if err != nil {
		return 0, err
	}
	return sequence, tx.Commit()
}

type externalClaimQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// externalClaimFacts is one unobserved claim row with the facts its
// disposition is derived from.
type externalClaimFacts struct {
	claim        ExternalEffectClaim
	ownerStatus  string
	background   bool
	returned     bool
	returnCursor int64
	watched      bool
	// startedHere: a background effect whose dispatch record names this
	// daemon process's instance id. Only the instance ids are compared; the
	// process registry is not consulted. That process's exit observer records
	// the exit, so until then the effect counts as running.
	startedHere bool
}

// externalClaimFactsSQL selects unobserved claim rows with their dispatch and
// return records and whether a live watcher is bound to the effect. A watcher
// is bound by an owner-reviewed effect profile (effect_id) or by an explicit
// claim binding (recovery_claim_id); either may come from the claim's Run or
// a later Run of its chain.
const externalClaimFactsSQL = `SELECT c.id,c.tenant_id,c.person_id,c.run_id,c.effect_id,c.target_key,c.state,c.observation_ref,c.sequence,c.created_at,
	COALESCE(r.status,''),
	COALESCE((SELECT d.payload_json FROM task_events d WHERE d.idempotency_key='external-effect-dispatched:'||c.run_id||':'||c.effect_id),''),
	COALESCE((SELECT x.payload_json FROM task_events x WHERE x.idempotency_key='external-effect-returned:'||c.run_id||':'||c.effect_id),''),
	COALESCE((SELECT x.cursor FROM task_events x WHERE x.idempotency_key='external-effect-returned:'||c.run_id||':'||c.effect_id),0),
	EXISTS (SELECT 1 FROM external_watches w
	 WHERE w.tenant_id=c.tenant_id AND w.person_id=c.person_id
	 AND w.status IN ('pending','running','succeeded') AND COALESCE(w.finalized,0)=0
	 AND ((json_extract(w.preflight_receipt_json,'$.effect_id')=c.effect_id
	       AND COALESCE(json_extract(w.preflight_receipt_json,'$.effect_rule_key'),'') <> ''
	       AND COALESCE(NULLIF(json_extract(w.preflight_receipt_json,'$.effect_run_id'),''), w.run_id)=c.run_id)
	   OR json_extract(w.preflight_receipt_json,'$.recovery_claim_id') IN
	      (SELECT g.id FROM external_effect_claims g WHERE g.tenant_id=c.tenant_id AND g.run_id=c.run_id AND g.effect_id=c.effect_id)))
	FROM external_effect_claims c LEFT JOIN runs r ON r.tenant_id=c.tenant_id AND r.id=c.run_id`

// scanExternalClaimFacts reads claim facts as the daemon process named by
// instance sees them.
func scanExternalClaimFacts(rows *sql.Rows, instance string) ([]externalClaimFacts, error) {
	defer rows.Close()
	var out []externalClaimFacts
	for rows.Next() {
		var f externalClaimFacts
		var dispatched, returned string
		var createdAt int64
		if err := rows.Scan(&f.claim.ID, &f.claim.TenantID, &f.claim.PersonID, &f.claim.RunID, &f.claim.EffectID,
			&f.claim.TargetKey, &f.claim.State, &f.claim.ObservationRef, &f.claim.Sequence, &createdAt, &f.ownerStatus,
			&dispatched, &returned, &f.returnCursor, &f.watched); err != nil {
			return nil, err
		}
		f.claim.CreatedAt = time.Unix(createdAt, 0)
		if dispatched != "" {
			var payload externalEffectDispatchPayload
			if json.Unmarshal([]byte(dispatched), &payload) == nil {
				f.claim.DispatchRecorded = true
				f.claim.Summary = payload.Summary
				f.claim.ToolName = payload.ToolName
				f.background = payload.Background
				f.claim.Background = payload.Background
				f.startedHere = payload.Background && payload.Instance != "" && payload.Instance == instance
			}
		}
		if returned != "" {
			f.returned = true
			var payload externalEffectReturnPayload
			if json.Unmarshal([]byte(returned), &payload) == nil {
				f.claim.Outcome = payload.Outcome
			}
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// disposition derives the claim's lifecycle position. ownChain is true when
// the requesting Run is the claim's Run or a later Run of its exact chain.
// skip reports a reservation that never crossed dispatch and whose owner no
// longer runs: recovery releases it and it cannot have caused an effect.
func (f externalClaimFacts) disposition(ownChain bool) (string, bool) {
	ownerLive := f.ownerStatus == "running"
	if f.claim.State == ExternalClaimReserved {
		if !ownerLive {
			return "", true
		}
		return ExternalClaimInFlight, false
	}
	switch {
	case f.returned:
	case f.background:
		// Only the process that started it observes its exit, whether or
		// not its owner Run still runs.
		if f.startedHere {
			return ExternalClaimInFlight, false
		}
	case ownerLive && !ownChain:
		return ExternalClaimInFlight, false
	}
	if f.watched {
		return ExternalClaimWatched, false
	}
	return ExternalClaimLeftUncertain, false
}

// runLineage returns the requesting Run and its ancestors along the exact
// resume edge, bounded like every other upward walk of that edge.
func runLineage(ctx context.Context, db externalClaimQuerier, tenantID, personID, runID string) (map[string]bool, error) {
	lineage := map[string]bool{}
	if strings.TrimSpace(runID) == "" {
		return lineage, nil
	}
	rows, err := db.QueryContext(ctx, `WITH RECURSIVE lineage(id,parent,depth) AS (
		SELECT id,COALESCE(resumes_run_id,''),0 FROM runs WHERE tenant_id=? AND person_id=? AND id=?
		UNION ALL SELECT r.id,COALESCE(r.resumes_run_id,''),l.depth+1 FROM runs r JOIN lineage l ON r.id=l.parent
		WHERE r.tenant_id=? AND r.person_id=? AND l.depth<?)
		SELECT id FROM lineage`, tenantID, personID, runID, tenantID, personID, resumeChainMaxHops)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		lineage[id] = true
	}
	lineage[runID] = true
	return lineage, rows.Err()
}

// ExternalEffectConflicts is the blocker view one Run sees for a target
// set. Own holds unobserved claims of its own chain that only an observation
// bound by that chain or the person's settlement can release. Waiting holds
// claims that can still resolve without anyone deciding: in flight, or
// watched. Foreign holds other Runs' left-uncertain claims, which this Run
// may pass only once with the person's approval. Acknowledged holds the ones
// this Run already observed after the effect and may pass once without
// asking; passing spends that observation.
type ExternalEffectConflicts struct {
	All          []ExternalEffectClaim
	Own          []ExternalEffectClaim
	Waiting      []ExternalEffectClaim
	Foreign      []ExternalEffectClaim
	Acknowledged []ExternalEffectClaim
	facts        map[string]externalClaimFacts
	acks         map[string]externalEffectAcknowledgment
}

func classifyExternalBlockers(ctx context.Context, db externalClaimQuerier, instance, tenantID, personID, requestingRun string, targets []string) (ExternalEffectConflicts, error) {
	var out ExternalEffectConflicts
	lineage, err := runLineage(ctx, db, tenantID, personID, requestingRun)
	if err != nil {
		return out, err
	}
	var acks externalEffectAcknowledgments
	if requestingRun != "" {
		if acks, err = externalEffectAcknowledgmentsTx(ctx, db, tenantID, requestingRun); err != nil {
			return out, err
		}
	}
	out.acks = map[string]externalEffectAcknowledgment{}
	rows, err := db.QueryContext(ctx, externalClaimFactsSQL+`
		WHERE c.tenant_id=? AND c.person_id=? AND c.state <> 'observed' ORDER BY c.created_at,c.id`, tenantID, personID)
	if err != nil {
		return out, err
	}
	facts, err := scanExternalClaimFacts(rows, instance)
	if err != nil {
		return out, err
	}
	out.facts = map[string]externalClaimFacts{}
	for _, f := range facts {
		conflicts := false
		for _, target := range targets {
			if externalTargetsConflict(target, f.claim.TargetKey) {
				conflicts = true
				break
			}
		}
		if !conflicts {
			continue
		}
		ownChain := lineage[f.claim.RunID]
		disposition, skip := f.disposition(ownChain)
		if skip {
			continue
		}
		claim := f.claim
		claim.Disposition, claim.SameChain = disposition, ownChain
		out.facts[claim.ID] = f
		out.All = append(out.All, claim)
		switch {
		case disposition == ExternalClaimWatched:
			out.Waiting = append(out.Waiting, claim)
		case ownChain:
			out.Own = append(out.Own, claim)
		case disposition == ExternalClaimInFlight:
			out.Waiting = append(out.Waiting, claim)
		default:
			if ack, ok := acks.forClaim(claim, f.returnCursor); ok {
				out.acks[claim.ID] = ack
				out.Acknowledged = append(out.Acknowledged, claim)
				continue
			}
			out.Foreign = append(out.Foreign, claim)
		}
	}
	return out, nil
}

// needsDecision reports that the next conflicting call cannot proceed by
// waiting, in the claim gate's order: the Run's own chain holds an unresolved
// claim nothing observes, or nothing is left to wait for and another Run's
// left-uncertain claim must be passed by the person. While a claim is still
// waited for, another Run's left-uncertain claim is decided after the wait.
func (c ExternalEffectConflicts) needsDecision() bool {
	return len(c.Own) > 0 || len(c.Waiting) == 0 && len(c.Foreign) > 0
}

// Description names a claim for a model or a person: its Run, the redacted
// summary of the call that made it, and where that call stands.
func (c ExternalEffectClaim) Description() string {
	summary := "an earlier call"
	if c.Summary != "" {
		summary = "`" + c.Summary + "`"
	}
	state := c.Outcome
	switch {
	case c.State == ExternalClaimObserved:
		state = "released"
	case c.Disposition == ExternalClaimInFlight:
		state = "still in flight"
	case c.Disposition == ExternalClaimWatched && state == "":
		state = "awaiting its bound watcher"
	case state == "":
		state = "effect outcome not established"
		if !c.DispatchRecorded {
			state += "; dispatch details unavailable"
		}
	}
	return fmt.Sprintf("claim %s from run %s: %s, %s", c.ID, c.RunID, summary, state)
}

// ExternalEffectClaimsForEffect returns one effect group's claim rows with
// their derived lifecycle facts, for the person who owns them.
func (s *Store) ExternalEffectClaimsForEffect(ctx context.Context, tenantID, personID, runID, effectID string) ([]ExternalEffectClaim, error) {
	if s == nil || s.db == nil || strings.TrimSpace(personID) == "" {
		return nil, fmt.Errorf("external effect owner is required")
	}
	return queryExternalEffectClaims(ctx, s.db, s.instance, `WHERE c.tenant_id=? AND c.person_id=? AND c.run_id=? AND c.effect_id=? ORDER BY c.target_key`,
		normalizeTenant(tenantID), personID, runID, effectID)
}

// ExternalEffectClaimDetail returns one claim, visible only to its person.
func (s *Store) ExternalEffectClaimDetail(ctx context.Context, tenantID, personID, claimID string) (ExternalEffectClaim, error) {
	if s == nil || s.db == nil || strings.TrimSpace(personID) == "" || strings.TrimSpace(claimID) == "" {
		return ExternalEffectClaim{}, ErrExternalEffectClaimNotFound
	}
	claims, err := queryExternalEffectClaims(ctx, s.db, s.instance, `WHERE c.tenant_id=? AND c.person_id=? AND c.id=?`,
		normalizeTenant(tenantID), personID, claimID)
	if err != nil {
		return ExternalEffectClaim{}, err
	}
	if len(claims) != 1 {
		return ExternalEffectClaim{}, ErrExternalEffectClaimNotFound
	}
	return claims[0], nil
}

func queryExternalEffectClaims(ctx context.Context, db externalClaimQuerier, instance, where string, args ...any) ([]ExternalEffectClaim, error) {
	rows, err := db.QueryContext(ctx, externalClaimFactsSQL+" "+where, args...)
	if err != nil {
		return nil, err
	}
	facts, err := scanExternalClaimFacts(rows, instance)
	if err != nil {
		return nil, err
	}
	claims := make([]ExternalEffectClaim, 0, len(facts))
	for _, f := range facts {
		claim := f.claim
		if claim.State != ExternalClaimObserved {
			// Without a requesting Run every unreturned call of a live owner
			// counts as in flight.
			if disposition, skip := f.disposition(false); !skip {
				claim.Disposition = disposition
			}
		}
		claims = append(claims, claim)
	}
	return claims, nil
}
