package control

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// UnknownExternalTarget conflicts with every other target held by this person.
// Only a trusted tool adapter may supply a narrower canonical target key.
const UnknownExternalTarget = "*"

const (
	ExternalClaimReserved  = "reserved"
	ExternalClaimUncertain = "uncertain"
	ExternalClaimObserved  = "observed"
)

type ExternalEffectClaim struct {
	ID             string
	TenantID       string
	PersonID       string
	RunID          string
	EffectID       string
	TargetKey      string
	State          string
	ObservationRef string
}

type ExternalEffectClaimRequest struct {
	TenantID   string
	PersonID   string
	RunID      string
	EffectID   string
	TargetKeys []string
}

type ExternalEffectClaimDecision struct {
	Claims       []ExternalEffectClaim
	Granted      bool
	AlreadyKnown bool
	BlockedByRun string
}

type ExternalResourceWait struct {
	TenantID, PersonID, RunID, EffectID string
	TargetKeys                          []string
	Status                              string
}

// ListUnresolvedExternalEffects is the person-scoped read surface for resource
// waits and diagnostics. It never returns another person's claims or raw tool
// arguments, and callers cannot use it to release a target.
func (s *Store) ListUnresolvedExternalEffects(ctx context.Context, tenantID, personID string, limit int) ([]ExternalEffectClaim, error) {
	if s == nil || s.db == nil || strings.TrimSpace(personID) == "" {
		return nil, fmt.Errorf("external effect owner is required")
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, tenant_id, person_id, run_id, effect_id, target_key, state, observation_ref
		FROM external_effect_claims WHERE tenant_id = ? AND person_id = ? AND state <> 'observed'
		ORDER BY created_at, id LIMIT ?`, normalizeTenant(tenantID), personID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var claims []ExternalEffectClaim
	for rows.Next() {
		var claim ExternalEffectClaim
		if err := rows.Scan(&claim.ID, &claim.TenantID, &claim.PersonID, &claim.RunID,
			&claim.EffectID, &claim.TargetKey, &claim.State, &claim.ObservationRef); err != nil {
			return nil, err
		}
		claims = append(claims, claim)
	}
	return claims, rows.Err()
}

// ObserveExternalEffectWithWatch is the conservative fallback for an unknown
// external target. The person explicitly relates an exact effect to a
// successful, finalized daemon observation created by the same Run after the
// effect was claimed. Both identities and the release commit in one
// transaction; model prose and a merely finished Run are never evidence.
func (s *Store) ObserveExternalEffectWithWatch(ctx context.Context, tenantID, personID, claimID, watchID string) (ExternalEffectClaim, error) {
	if s == nil || s.db == nil || strings.TrimSpace(personID) == "" ||
		strings.TrimSpace(claimID) == "" || strings.TrimSpace(watchID) == "" {
		return ExternalEffectClaim{}, fmt.Errorf("external effect claim and watcher identities are required")
	}
	tenantID = normalizeTenant(tenantID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExternalEffectClaim{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var claim ExternalEffectClaim
	var claimedAt int64
	err = tx.QueryRowContext(ctx, `SELECT id, tenant_id, person_id, run_id, effect_id, target_key, state, observation_ref, created_at
		FROM external_effect_claims WHERE tenant_id = ? AND person_id = ? AND id = ?`,
		tenantID, personID, claimID).Scan(&claim.ID, &claim.TenantID, &claim.PersonID, &claim.RunID,
		&claim.EffectID, &claim.TargetKey, &claim.State, &claim.ObservationRef, &claimedAt)
	if err != nil {
		return ExternalEffectClaim{}, fmt.Errorf("external effect claim not found for this person: %w", err)
	}
	var watchRun, watchStatus, receiptJSON, command string
	var watchFinalized int
	var watchCreated int64
	var revision int
	err = tx.QueryRowContext(ctx, `SELECT run_id, status, finalized, created_at, verdict_revision,
		COALESCE(preflight_receipt_json, '{}'), command FROM external_watches
		WHERE tenant_id = ? AND person_id = ? AND id = ?`, tenantID, personID, watchID).
		Scan(&watchRun, &watchStatus, &watchFinalized, &watchCreated, &revision, &receiptJSON, &command)
	if err != nil {
		return ExternalEffectClaim{}, fmt.Errorf("watcher not found for this person: %w", err)
	}
	var receipt ExternalWatchPreflightReceipt
	if err := json.Unmarshal([]byte(receiptJSON), &receipt); err != nil {
		return ExternalEffectClaim{}, fmt.Errorf("watcher has no valid preflight receipt: %w", err)
	}
	if watchRun != claim.RunID || watchCreated < claimedAt || watchStatus != ExternalWatchSucceeded ||
		watchFinalized == 0 || receipt.Version < ExternalWatchContinuationReceiptVersion ||
		receipt.CommandHash != fmt.Sprintf("%x", sha256.Sum256([]byte(command))) {
		return ExternalEffectClaim{}, fmt.Errorf("watcher is not a finalized trusted observation of this exact run after the effect")
	}
	observationRef := fmt.Sprintf("watch:%s:r%d", watchID, revision)
	var total, different int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE WHEN state = 'observed' AND observation_ref <> ? THEN 1 ELSE 0 END), 0)
		FROM external_effect_claims WHERE tenant_id = ? AND person_id = ? AND run_id = ? AND effect_id = ?`,
		observationRef, tenantID, personID, claim.RunID, claim.EffectID).Scan(&total, &different)
	if err != nil {
		return ExternalEffectClaim{}, err
	}
	if total == 0 || different != 0 {
		return ExternalEffectClaim{}, fmt.Errorf("external effect cannot be released with a different observation")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE external_effect_claims SET state = 'observed', observation_ref = ?, updated_at = ?
		WHERE tenant_id = ? AND person_id = ? AND run_id = ? AND effect_id = ? AND state <> 'observed'`,
		observationRef, time.Now().Unix(), tenantID, personID, claim.RunID, claim.EffectID); err != nil {
		return ExternalEffectClaim{}, err
	}
	if err := tx.Commit(); err != nil {
		return ExternalEffectClaim{}, err
	}
	claim.State, claim.ObservationRef = ExternalClaimObserved, observationRef
	return claim, nil
}

// ClaimExternalEffects atomically reserves every target or none. A reservation
// is durable before the effect can start; a crash never silently frees it.
// Retrying one exact effect id reports AlreadyKnown and must not dispatch it
// again, even when its old claim has since been observed.
func (s *Store) ClaimExternalEffects(ctx context.Context, request ExternalEffectClaimRequest) (ExternalEffectClaimDecision, error) {
	if s == nil || s.db == nil {
		return ExternalEffectClaimDecision{}, fmt.Errorf("external effect store is unavailable")
	}
	request.TenantID = normalizeTenant(request.TenantID)
	request.PersonID = strings.TrimSpace(request.PersonID)
	request.RunID = strings.TrimSpace(request.RunID)
	request.EffectID = strings.TrimSpace(request.EffectID)
	if request.PersonID == "" || request.RunID == "" || request.EffectID == "" || len(request.TargetKeys) == 0 || len(request.TargetKeys) > 16 {
		return ExternalEffectClaimDecision{}, fmt.Errorf("external effect requires a person, run, effect and 1-16 targets")
	}
	targets := append([]string(nil), request.TargetKeys...)
	slices.Sort(targets)
	targets = slices.Compact(targets)
	for _, target := range targets {
		if !validExternalTargetKey(target) {
			return ExternalEffectClaimDecision{}, fmt.Errorf("invalid external target identity")
		}
	}
	for attempt := 0; attempt < 5; attempt++ {
		decision, err := s.claimExternalEffectsOnce(ctx, request, targets)
		if !isSQLiteBusy(err) || ctx.Err() != nil {
			return decision, err
		}
		select {
		case <-ctx.Done():
			return ExternalEffectClaimDecision{}, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 5 * time.Millisecond):
		}
	}
	return ExternalEffectClaimDecision{}, fmt.Errorf("external target claim remained busy")
}

func (s *Store) claimExternalEffectsOnce(ctx context.Context, request ExternalEffectClaimRequest, targets []string) (ExternalEffectClaimDecision, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExternalEffectClaimDecision{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var owner, status string
	err = tx.QueryRowContext(ctx, `SELECT person_id, status FROM runs WHERE tenant_id = ? AND id = ?`,
		request.TenantID, request.RunID).Scan(&owner, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return ExternalEffectClaimDecision{}, fmt.Errorf("external effect run is not active for this person")
	}
	if err != nil {
		return ExternalEffectClaimDecision{}, err
	}
	if owner != request.PersonID || status != "running" {
		return ExternalEffectClaimDecision{}, fmt.Errorf("external effect run is not active for this person")
	}
	existing, err := loadExternalEffectClaimsTx(ctx, tx, request.TenantID, request.RunID, request.EffectID)
	if err != nil {
		return ExternalEffectClaimDecision{}, err
	}
	if len(existing) != 0 {
		if len(existing) != len(targets) {
			return ExternalEffectClaimDecision{}, fmt.Errorf("external effect target set changed after its first claim")
		}
		for i := range targets {
			if existing[i].TargetKey != targets[i] {
				return ExternalEffectClaimDecision{}, fmt.Errorf("external effect target set changed after its first claim")
			}
		}
		return ExternalEffectClaimDecision{Claims: existing, AlreadyKnown: true}, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT run_id, target_key FROM external_effect_claims
		WHERE tenant_id = ? AND person_id = ? AND state <> 'observed'`,
		request.TenantID, request.PersonID)
	if err != nil {
		return ExternalEffectClaimDecision{}, err
	}
	var blockedBy string
	for rows.Next() {
		var heldRun, heldTarget string
		if err := rows.Scan(&heldRun, &heldTarget); err != nil {
			_ = rows.Close()
			return ExternalEffectClaimDecision{}, err
		}
		for _, target := range targets {
			if externalTargetsConflict(target, heldTarget) {
				blockedBy = heldRun
				break
			}
		}
		if blockedBy != "" {
			break
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return ExternalEffectClaimDecision{}, err
	}
	if err := rows.Close(); err != nil {
		return ExternalEffectClaimDecision{}, err
	}
	if blockedBy != "" {
		encoded, err := json.Marshal(targets)
		if err != nil {
			return ExternalEffectClaimDecision{}, err
		}
		now := time.Now().Unix()
		if _, err := tx.ExecContext(ctx, `INSERT INTO external_resource_waits
			(tenant_id, person_id, run_id, effect_id, targets_json, status, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, 'pending', ?, ?)
			ON CONFLICT(tenant_id, run_id, effect_id) DO NOTHING`,
			request.TenantID, request.PersonID, request.RunID, request.EffectID, string(encoded), now, now); err != nil {
			return ExternalEffectClaimDecision{}, err
		}
		if err := tx.Commit(); err != nil {
			return ExternalEffectClaimDecision{}, err
		}
		return ExternalEffectClaimDecision{BlockedByRun: blockedBy}, nil
	}
	now := time.Now().Unix()
	claims := make([]ExternalEffectClaim, 0, len(targets))
	for _, target := range targets {
		claim := ExternalEffectClaim{ID: "effectclaim_" + uuid.NewString(), TenantID: request.TenantID,
			PersonID: request.PersonID, RunID: request.RunID, EffectID: request.EffectID,
			TargetKey: target, State: ExternalClaimReserved}
		if _, err := tx.ExecContext(ctx, `INSERT INTO external_effect_claims
			(id, tenant_id, person_id, run_id, effect_id, target_key, state, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, claim.ID, claim.TenantID, claim.PersonID,
			claim.RunID, claim.EffectID, claim.TargetKey, claim.State, now, now); err != nil {
			return ExternalEffectClaimDecision{}, err
		}
		claims = append(claims, claim)
	}
	if err := tx.Commit(); err != nil {
		return ExternalEffectClaimDecision{}, err
	}
	return ExternalEffectClaimDecision{Claims: claims, Granted: true}, nil
}

func loadExternalEffectClaimsTx(ctx context.Context, tx *sql.Tx, tenantID, runID, effectID string) ([]ExternalEffectClaim, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, tenant_id, person_id, run_id, effect_id, target_key, state, observation_ref
		FROM external_effect_claims WHERE tenant_id = ? AND run_id = ? AND effect_id = ? ORDER BY target_key`, tenantID, runID, effectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var claims []ExternalEffectClaim
	for rows.Next() {
		var claim ExternalEffectClaim
		if err := rows.Scan(&claim.ID, &claim.TenantID, &claim.PersonID, &claim.RunID, &claim.EffectID,
			&claim.TargetKey, &claim.State, &claim.ObservationRef); err != nil {
			return nil, err
		}
		claims = append(claims, claim)
	}
	return claims, rows.Err()
}

func validExternalTargetKey(target string) bool {
	if target == UnknownExternalTarget {
		return true
	}
	if len(target) < 3 || len(target) > 256 {
		return false
	}
	kind, identity, ok := strings.Cut(target, ":")
	if !ok || kind == "" || identity == "" {
		return false
	}
	for _, char := range target {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' ||
			char == '.' || char == '_' || char == '-' || char == ':' {
			continue
		}
		return false
	}
	return true
}

// ValidExternalTargetKey lets an operator-owned command profile validate the
// same target grammar used by the durable claim ledger before it is saved.
func ValidExternalTargetKey(target string) bool { return validExternalTargetKey(target) }

func externalTargetsConflict(a, b string) bool {
	return a == UnknownExternalTarget || b == UnknownExternalTarget || a == b
}

// ListReadyExternalResourceWaits returns parked Runs whose whole requested
// target set has no unresolved effect. Readiness is advisory: the claim gate
// checks again before dispatch, so a competing Run can never gain authority
// merely because a wakeup was queued.
func (s *Store) ListReadyExternalResourceWaits(ctx context.Context, limit int) ([]ExternalResourceWait, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("external resource store is unavailable")
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `SELECT w.tenant_id, w.person_id, w.run_id, w.effect_id, w.targets_json, w.status
		FROM external_resource_waits w JOIN runs r ON r.tenant_id = w.tenant_id AND r.id = w.run_id
		WHERE w.status IN ('pending', 'queued') AND r.status = 'waiting_external' AND r.person_id = w.person_id
		AND NOT EXISTS (SELECT 1 FROM runs child WHERE child.tenant_id = w.tenant_id AND child.resumes_run_id = w.run_id)
		ORDER BY w.created_at, w.run_id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	var pending []ExternalResourceWait
	for rows.Next() {
		var wait ExternalResourceWait
		var encoded string
		if err := rows.Scan(&wait.TenantID, &wait.PersonID, &wait.RunID, &wait.EffectID, &encoded, &wait.Status); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := json.Unmarshal([]byte(encoded), &wait.TargetKeys); err != nil {
			_ = rows.Close()
			return nil, err
		}
		pending = append(pending, wait)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var ready []ExternalResourceWait
	for _, wait := range pending {
		if wait.Status == "queued" {
			ready = append(ready, wait)
			continue
		}
		claims, err := s.db.QueryContext(ctx, `SELECT target_key FROM external_effect_claims
			WHERE tenant_id = ? AND person_id = ? AND state <> 'observed'`, wait.TenantID, wait.PersonID)
		if err != nil {
			return nil, err
		}
		conflict := false
		for claims.Next() {
			var held string
			if err := claims.Scan(&held); err != nil {
				_ = claims.Close()
				return nil, err
			}
			for _, target := range wait.TargetKeys {
				if externalTargetsConflict(target, held) {
					conflict = true
					break
				}
			}
		}
		if err := claims.Err(); err != nil {
			_ = claims.Close()
			return nil, err
		}
		if err := claims.Close(); err != nil {
			return nil, err
		}
		if !conflict {
			ready = append(ready, wait)
		}
	}
	return ready, nil
}

// MarkExternalResourceWaitQueued records readiness before enqueue. Both pending
// and queued rows are scanned after restart: queued rows are retried with the
// same queue idempotency key until a durable continuation exists.
func (s *Store) MarkExternalResourceWaitQueued(ctx context.Context, wait ExternalResourceWait) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("external resource store is unavailable")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE external_resource_waits SET status = 'queued', updated_at = ?
		WHERE tenant_id = ? AND person_id = ? AND run_id = ? AND effect_id = ? AND status = 'pending'`,
		time.Now().Unix(), normalizeTenant(wait.TenantID), wait.PersonID, wait.RunID, wait.EffectID)
	return err
}

// IsRunExternalResourceWaitPending keeps an exact waiting_external parent
// reserved for its daemon wakeup while the target is still occupied.
func (s *Store) IsRunExternalResourceWaitPending(ctx context.Context, tenantID, runID string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM external_resource_waits
		WHERE tenant_id = ? AND run_id = ? AND status = 'pending'`, normalizeTenant(tenantID), runID).Scan(&count)
	return count > 0, err
}

// ObserveUndispatchedExternalEffects releases only pre-dispatch reservations
// whose owner Run is no longer executing. MarkExternalEffectPossible is
// durable before the tool body starts, so a reserved claim proves the effect
// never crossed the dispatch boundary. Uncertain claims are never inferred
// safe from Run status, even when the Run has finished.
func (s *Store) ObserveUndispatchedExternalEffects(ctx context.Context) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("external effect store is unavailable")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE external_effect_claims
		SET state = 'observed', observation_ref = 'recovery:never_dispatched', updated_at = ?
		WHERE state = 'reserved' AND EXISTS (
			SELECT 1 FROM runs r WHERE r.tenant_id = external_effect_claims.tenant_id
			AND r.id = external_effect_claims.run_id AND r.status <> 'running')`, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// MarkExternalEffectPossible crosses the dispatch boundary. Once set, a
// restart must treat the effect as uncertain until an observation resolves it.
// The update is idempotent for the exact Run and effect id.
func (s *Store) MarkExternalEffectPossible(ctx context.Context, tenantID, runID, effectID string) error {
	if s == nil || s.db == nil || strings.TrimSpace(runID) == "" || strings.TrimSpace(effectID) == "" {
		return fmt.Errorf("external effect claim identity is required")
	}
	tenantID = normalizeTenant(tenantID)
	result, err := s.db.ExecContext(ctx, `UPDATE external_effect_claims SET state = 'uncertain', updated_at = ?
		WHERE tenant_id = ? AND run_id = ? AND effect_id = ? AND state = 'reserved'`,
		time.Now().Unix(), tenantID, runID, effectID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 0 {
		return nil
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM external_effect_claims WHERE tenant_id = ? AND run_id = ? AND effect_id = ? AND state = 'uncertain'`,
		tenantID, runID, effectID).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("external effect claim was not reserved")
	}
	return nil
}

// ObserveExternalEffect closes an exact claim only with a durable reference
// produced by a trusted observer. It never infers safety from a Run finishing.
func (s *Store) ObserveExternalEffect(ctx context.Context, tenantID, personID, claimID, observationRef string) error {
	if s == nil || s.db == nil || strings.TrimSpace(personID) == "" || strings.TrimSpace(claimID) == "" || strings.TrimSpace(observationRef) == "" {
		return fmt.Errorf("external effect observation identity and evidence are required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE external_effect_claims SET state = 'observed', observation_ref = ?, updated_at = ?
		WHERE tenant_id = ? AND person_id = ? AND id = ? AND state <> 'observed'`,
		observationRef, time.Now().Unix(), normalizeTenant(tenantID), personID, claimID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 0 {
		return nil
	}
	var recorded string
	err = s.db.QueryRowContext(ctx, `SELECT observation_ref FROM external_effect_claims WHERE tenant_id = ? AND person_id = ? AND id = ? AND state = 'observed'`,
		normalizeTenant(tenantID), personID, claimID).Scan(&recorded)
	if err == nil && recorded == observationRef {
		return nil
	}
	return fmt.Errorf("external effect claim cannot be observed with this reference")
}

// ObserveExternalEffectGroup resolves every target of one tool effect in one
// transaction. A partial release would let another Run use one target while
// the same command's other targets still have uncertain effects.
func (s *Store) ObserveExternalEffectGroup(ctx context.Context, tenantID, personID, runID, effectID, observationRef string) error {
	if s == nil || s.db == nil || strings.TrimSpace(personID) == "" || strings.TrimSpace(runID) == "" ||
		strings.TrimSpace(effectID) == "" || strings.TrimSpace(observationRef) == "" {
		return fmt.Errorf("external effect observation identity and evidence are required")
	}
	tenantID = normalizeTenant(tenantID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var total, different int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE WHEN state = 'observed' AND observation_ref <> ? THEN 1 ELSE 0 END), 0)
		FROM external_effect_claims WHERE tenant_id = ? AND person_id = ? AND run_id = ? AND effect_id = ?`,
		observationRef, tenantID, personID, runID, effectID).Scan(&total, &different); err != nil {
		return err
	}
	if total == 0 || different != 0 {
		return fmt.Errorf("external effect claim cannot be observed with this reference")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE external_effect_claims SET state = 'observed', observation_ref = ?, updated_at = ?
		WHERE tenant_id = ? AND person_id = ? AND run_id = ? AND effect_id = ? AND state <> 'observed'`,
		observationRef, time.Now().Unix(), tenantID, personID, runID, effectID); err != nil {
		return err
	}
	return tx.Commit()
}
