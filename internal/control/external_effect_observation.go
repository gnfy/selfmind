package control

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ExternalEffectObservation is a successful read-only observation that the
// model of ObserverRunID explicitly bound to one claim. The model judges
// whether the observation shows the effect's outcome; the store checks the
// identities, the chain, and that the observation started after the effect.
type ExternalEffectObservation struct {
	TenantID, PersonID, ObserverRunID, ClaimID, ToolCallID string
	// StartSequence was allocated before the observation ran.
	StartSequence int64
}

// ExternalEffectObservationResult says what the bound observation did.
// Released: the claim belonged to the observer's chain and its whole effect
// group is now observed. Acknowledged: the claim belongs to another Run; it
// stays unresolved and the observer may pass it once.
type ExternalEffectObservationResult struct {
	Claim        ExternalEffectClaim
	Released     bool
	Acknowledged bool
}

// ErrExternalEffectAlreadyReleased reports a claim that no longer needs an
// observation.
var ErrExternalEffectAlreadyReleased = errors.New("external effect claim is already released")

// observationFollowsEffect reports whether an observation recorded with
// sequence obsSeq (created at obsAt) started after the effect claimed with
// claimSeq (created at claimAt) and, when a return was recorded, after the
// call returned. A zero sequence was recorded before schema v26 and precedes
// every sequenced row; two zero rows fall back to strictly later timestamps.
func observationFollowsEffect(claimSeq, claimAt, returnCursor, obsSeq, obsAt int64) bool {
	switch {
	case obsSeq == 0 && claimSeq == 0:
		return obsAt > claimAt
	case obsSeq == 0 || obsSeq <= claimSeq:
		return false
	}
	return returnCursor == 0 || obsSeq > returnCursor
}

// watchFollowsEffectTx reports whether a watcher registered with the given
// sequence and creation time started after the effect group's claim and its
// recorded return.
func watchFollowsEffectTx(ctx context.Context, tx *sql.Tx, tenantID, runID, effectID string, watchSequence, watchCreated int64) (bool, error) {
	var claimSequence, claimCreated, returnCursor int64
	err := tx.QueryRowContext(ctx, `SELECT MAX(sequence), MIN(created_at),
		COALESCE((SELECT x.cursor FROM task_events x WHERE x.idempotency_key='external-effect-returned:'||?||':'||?),0)
		FROM external_effect_claims WHERE tenant_id=? AND run_id=? AND effect_id=?`,
		runID, effectID, tenantID, runID, effectID).Scan(&claimSequence, &claimCreated, &returnCursor)
	if err != nil {
		return false, err
	}
	return observationFollowsEffect(claimSequence, claimCreated, returnCursor, watchSequence, watchCreated), nil
}

// ReleaseExternalEffectForWatch releases the claim a watcher was explicitly
// bound to (watch_external with effect_claim_id) once the watcher's success is
// durable. The watcher must belong to the claim's Run or a later Run of its
// exact chain, have been registered after the effect, and still run the
// command it was registered with. It reports false when there is nothing to
// release; a failed or unrelated watcher never releases anything.
func (s *Store) ReleaseExternalEffectForWatch(ctx context.Context, tenantID, watchID string) (ExternalEffectClaim, bool, error) {
	if s == nil || s.db == nil || strings.TrimSpace(watchID) == "" {
		return ExternalEffectClaim{}, false, nil
	}
	tenantID = normalizeTenant(tenantID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExternalEffectClaim{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var personID, runID, status, command, receiptJSON, waitGroup string
	var finalized, revision int
	var watchSequence, watchCreated int64
	err = tx.QueryRowContext(ctx, `SELECT person_id, run_id, status, command, COALESCE(preflight_receipt_json,'{}'),
		COALESCE(wait_group_id,''), COALESCE(finalized,0), verdict_revision, sequence, created_at
		FROM external_watches WHERE tenant_id=? AND id=?`, tenantID, watchID).
		Scan(&personID, &runID, &status, &command, &receiptJSON, &waitGroup, &finalized, &revision, &watchSequence, &watchCreated)
	if errors.Is(err, sql.ErrNoRows) {
		return ExternalEffectClaim{}, false, nil
	}
	if err != nil {
		return ExternalEffectClaim{}, false, err
	}
	var receipt ExternalWatchPreflightReceipt
	if json.Unmarshal([]byte(receiptJSON), &receipt) != nil || receipt.RecoveryClaimID == "" ||
		receipt.Version < ExternalWatchRecoveryReceiptVersion || waitGroup != "" || status != ExternalWatchSucceeded ||
		receipt.CommandHash != fmt.Sprintf("%x", sha256.Sum256([]byte(command))) {
		return ExternalEffectClaim{}, false, nil
	}
	if finalized == 0 {
		var event int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM task_events WHERE run_id=? AND idempotency_key=? LIMIT 1`,
			runID, fmt.Sprintf("external-watch:%s:r%d:completed", watchID, revision)).Scan(&event)
		if errors.Is(err, sql.ErrNoRows) {
			return ExternalEffectClaim{}, false, nil
		}
		if err != nil {
			return ExternalEffectClaim{}, false, err
		}
	}
	claim, err := externalEffectForObservationTx(ctx, tx, tenantID, personID, runID, receipt.RecoveryClaimID)
	if err != nil {
		return ExternalEffectClaim{}, false, nil
	}
	follows, err := watchFollowsEffectTx(ctx, tx, tenantID, claim.RunID, claim.EffectID, watchSequence, watchCreated)
	if err != nil || !follows {
		return ExternalEffectClaim{}, false, err
	}
	ref := fmt.Sprintf("watch:%s:r%d:bound", watchID, revision)
	result, err := tx.ExecContext(ctx, `UPDATE external_effect_claims SET state='observed', observation_ref=?, updated_at=?
		WHERE tenant_id=? AND person_id=? AND run_id=? AND effect_id=? AND state='uncertain'`,
		ref, time.Now().Unix(), tenantID, personID, claim.RunID, claim.EffectID)
	if err != nil {
		return ExternalEffectClaim{}, false, err
	}
	if changed, _ := result.RowsAffected(); changed == 0 {
		return ExternalEffectClaim{}, false, nil
	}
	if err := tx.Commit(); err != nil {
		return ExternalEffectClaim{}, false, err
	}
	claim.State, claim.ObservationRef = ExternalClaimObserved, ref
	return claim, true, nil
}

// ExternalEffectObservationTarget checks, before an observation runs, that it
// may be bound to the claim: the claim is the person's, still unobserved, and
// its call is not in flight. SameChain on the result says whether a success
// will release the claim or only acknowledge it.
func (s *Store) ExternalEffectObservationTarget(ctx context.Context, tenantID, personID, observerRun, claimID string) (ExternalEffectClaim, error) {
	if s == nil || s.db == nil {
		return ExternalEffectClaim{}, fmt.Errorf("external effect store is unavailable")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExternalEffectClaim{}, err
	}
	defer func() { _ = tx.Rollback() }()
	claim, _, err := externalEffectObservationTargetTx(ctx, tx, s.instance, normalizeTenant(tenantID), personID, observerRun, claimID)
	return claim, err
}

func externalEffectObservationTargetTx(ctx context.Context, tx *sql.Tx, instance, tenantID, personID, observerRun, claimID string) (ExternalEffectClaim, externalClaimFacts, error) {
	if strings.TrimSpace(personID) == "" || strings.TrimSpace(observerRun) == "" || strings.TrimSpace(claimID) == "" {
		return ExternalEffectClaim{}, externalClaimFacts{}, fmt.Errorf("exact person, observer Run and effect claim are required")
	}
	rows, err := tx.QueryContext(ctx, externalClaimFactsSQL+` WHERE c.tenant_id=? AND c.person_id=? AND c.id=?`, tenantID, personID, claimID)
	if err != nil {
		return ExternalEffectClaim{}, externalClaimFacts{}, err
	}
	facts, err := scanExternalClaimFacts(rows, instance)
	if err != nil {
		return ExternalEffectClaim{}, externalClaimFacts{}, err
	}
	if len(facts) != 1 {
		return ExternalEffectClaim{}, externalClaimFacts{}, ErrExternalEffectClaimNotFound
	}
	f := facts[0]
	if f.claim.State == ExternalClaimObserved {
		return f.claim, f, ErrExternalEffectAlreadyReleased
	}
	lineage, err := runLineage(ctx, tx, tenantID, personID, observerRun)
	if err != nil {
		return ExternalEffectClaim{}, externalClaimFacts{}, err
	}
	claim := f.claim
	claim.SameChain = lineage[claim.RunID]
	disposition, skip := f.disposition(claim.SameChain)
	if skip {
		return claim, f, fmt.Errorf("external effect claim never crossed dispatch; recovery releases it")
	}
	claim.Disposition = disposition
	if disposition == ExternalClaimInFlight {
		return claim, f, ErrExternalEffectInFlight
	}
	return claim, f, nil
}

// BindExternalEffectObservation records a successful bound observation. For a
// claim of the observer's own chain it releases the whole effect group with an
// observation ref naming the observing call. For another Run's claim it only
// records that the observer may pass that claim once; it never releases it.
func (s *Store) BindExternalEffectObservation(ctx context.Context, observation ExternalEffectObservation) (ExternalEffectObservationResult, error) {
	if s == nil || s.db == nil {
		return ExternalEffectObservationResult{}, fmt.Errorf("external effect store is unavailable")
	}
	tenantID := normalizeTenant(observation.TenantID)
	if strings.TrimSpace(observation.ToolCallID) == "" || observation.StartSequence <= 0 {
		return ExternalEffectObservationResult{}, fmt.Errorf("a bound observation needs its call and start sequence")
	}
	for attempt := 0; attempt < 5; attempt++ {
		result, err := s.bindExternalEffectObservationOnce(ctx, tenantID, observation)
		if !isSQLiteBusy(err) || ctx.Err() != nil {
			return result, err
		}
		select {
		case <-ctx.Done():
			return ExternalEffectObservationResult{}, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 5 * time.Millisecond):
		}
	}
	return ExternalEffectObservationResult{}, fmt.Errorf("external effect observation remained busy")
}

func (s *Store) bindExternalEffectObservationOnce(ctx context.Context, tenantID string, observation ExternalEffectObservation) (ExternalEffectObservationResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExternalEffectObservationResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var observerStatus string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM runs WHERE tenant_id=? AND person_id=? AND id=?`,
		tenantID, observation.PersonID, observation.ObserverRunID).Scan(&observerStatus); err != nil {
		return ExternalEffectObservationResult{}, fmt.Errorf("observer Run not found for this person: %w", err)
	}
	if observerStatus != "running" {
		return ExternalEffectObservationResult{}, fmt.Errorf("observer Run is not running")
	}
	claim, facts, err := externalEffectObservationTargetTx(ctx, tx, s.instance, tenantID, observation.PersonID, observation.ObserverRunID, observation.ClaimID)
	if err != nil {
		return ExternalEffectObservationResult{Claim: claim}, err
	}
	var claimedAt int64
	if err := tx.QueryRowContext(ctx, `SELECT created_at FROM external_effect_claims WHERE tenant_id=? AND id=?`,
		tenantID, claim.ID).Scan(&claimedAt); err != nil {
		return ExternalEffectObservationResult{}, err
	}
	if !observationFollowsEffect(claim.Sequence, claimedAt, facts.returnCursor, observation.StartSequence, time.Now().Unix()) {
		return ExternalEffectObservationResult{Claim: claim}, fmt.Errorf("the observation did not start after the effect's call returned")
	}
	if claim.SameChain {
		ref := "observation:" + observation.ObserverRunID + ":" + observation.ToolCallID
		if _, err := tx.ExecContext(ctx, `UPDATE external_effect_claims SET state='observed', observation_ref=?, updated_at=?
			WHERE tenant_id=? AND person_id=? AND run_id=? AND effect_id=? AND state <> 'observed'`,
			ref, time.Now().Unix(), tenantID, observation.PersonID, claim.RunID, claim.EffectID); err != nil {
			return ExternalEffectObservationResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return ExternalEffectObservationResult{}, err
		}
		claim.State, claim.ObservationRef, claim.Disposition = ExternalClaimObserved, ref, ""
		return ExternalEffectObservationResult{Claim: claim, Released: true}, nil
	}
	payload, err := json.Marshal(externalEffectAcknowledgment{ClaimID: claim.ID, ClaimRunID: claim.RunID,
		EffectID: claim.EffectID, ToolCallID: observation.ToolCallID, StartSequence: observation.StartSequence})
	if err != nil {
		return ExternalEffectObservationResult{}, err
	}
	key := fmt.Sprintf("external-effect-observed:%s:%s:%x", observation.ObserverRunID, observation.ToolCallID,
		sha256.Sum256([]byte(claim.RunID+"\x00"+claim.EffectID)))
	if _, _, err := insertEffectEventTx(ctx, tx, tenantID, observation.ObserverRunID, ExternalEffectObservedEvent, payload, key); err != nil {
		return ExternalEffectObservationResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ExternalEffectObservationResult{}, err
	}
	return ExternalEffectObservationResult{Claim: claim, Acknowledged: true}, nil
}

// externalEffectAcknowledgment is an unspent observation one Run bound to
// another Run's claim. It lets that Run's next conflicting effect pass the
// claim once.
type externalEffectAcknowledgment struct {
	EventID       string `json:"-"`
	ClaimID       string `json:"claim_id"`
	ClaimRunID    string `json:"claim_run_id"`
	EffectID      string `json:"effect_id"`
	ToolCallID    string `json:"tool_call_id"`
	StartSequence int64  `json:"start_sequence"`
}

type externalEffectAcknowledgments []externalEffectAcknowledgment

func externalEffectAcknowledgmentsTx(ctx context.Context, db externalClaimQuerier, tenantID, runID string) (externalEffectAcknowledgments, error) {
	rows, err := db.QueryContext(ctx, `SELECT e.id, e.payload_json FROM task_events e JOIN runs r ON r.id=e.run_id
		WHERE r.tenant_id=? AND e.run_id=? AND e.type=?
		AND NOT EXISTS (SELECT 1 FROM task_events spent WHERE spent.idempotency_key='external-effect-observation-consumed:'||e.id)
		ORDER BY e.cursor`, tenantID, runID, ExternalEffectObservedEvent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var acks externalEffectAcknowledgments
	for rows.Next() {
		var id, payload string
		if err := rows.Scan(&id, &payload); err != nil {
			return nil, err
		}
		var ack externalEffectAcknowledgment
		if json.Unmarshal([]byte(payload), &ack) != nil {
			continue
		}
		ack.EventID = id
		acks = append(acks, ack)
	}
	return acks, rows.Err()
}

// forClaim finds the oldest unspent acknowledgment of the claim's effect that
// started after the effect and its recorded return.
func (a externalEffectAcknowledgments) forClaim(claim ExternalEffectClaim, returnCursor int64) (externalEffectAcknowledgment, bool) {
	for _, ack := range a {
		if ack.ClaimRunID == claim.RunID && ack.EffectID == claim.EffectID &&
			observationFollowsEffect(claim.Sequence, 0, returnCursor, ack.StartSequence, 0) {
			return ack, true
		}
	}
	return externalEffectAcknowledgment{}, false
}
