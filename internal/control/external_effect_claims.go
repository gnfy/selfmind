package control

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
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
	CreatedAt      time.Time
	// Sequence orders the claim among claims, watchers, and events (see
	// nextEffectSequenceTx). Every target of one atomic claim shares it. Zero
	// means the claim was recorded before schema v26: it precedes every
	// sequenced row, and two zero rows have no recorded order.
	Sequence int64
	// The fields below are derived when the claim is read, never stored.
	// Disposition is in flight, watched, or left uncertain for an unobserved
	// claim (see external_effect_lifecycle.go). Summary and ToolName come from
	// the dispatch record; Outcome says how the call returned without proving
	// success. SameChain is set only in a blocker view: the claim belongs to the
	// requesting Run's exact chain.
	Disposition string
	Summary     string
	ToolName    string
	Outcome     string
	SameChain   bool
	// Background: the call started a background process, whose exit the
	// daemon that started it observes.
	Background bool
	// DispatchRecorded reports a decoded lifecycle record, not whether a
	// historical action did or did not happen. Absence never releases a claim.
	DispatchRecorded bool
}

type ExternalEffectClaimRequest struct {
	TenantID   string
	PersonID   string
	RunID      string
	EffectID   string
	TargetKeys []string
	// ToleratedClaimIDs are other Runs' left-uncertain claims the person
	// approved this exact call to pass once. They are never stored as grants
	// and leave those claims unresolved.
	ToleratedClaimIDs []string
	// DeferWait asks for the decision without writing the durable resource
	// wait while the caller waits in place for an in-flight claim.
	DeferWait bool
}

type ExternalEffectClaimDecision struct {
	Claims       []ExternalEffectClaim
	Granted      bool
	AlreadyKnown bool
	BlockedByRun string
	// NeedsObservation: the requesting Run's own chain holds an unresolved
	// claim. Only an observation that chain binds to the claim, or the person's
	// settlement, can release it; nothing is waited for.
	NeedsObservation bool
	// NeedsApproval: another Run left these claims uncertain. The call may pass
	// once with the person's approval or this Run's later bound observation.
	NeedsApproval  bool
	ApprovalClaims []ExternalEffectClaim
	// BlockingClaims lists every conflicting claim with its derived facts. When
	// neither flag above is set, the call waits for the claims in flight or
	// watched.
	BlockingClaims []ExternalEffectClaim
	// DecideAfterWait lists, for a call that waits, other Runs' left-uncertain
	// claims it has not passed: once the wait ends they still need the
	// person's decision, so the wait alone does not let the work continue.
	DecideAfterWait []ExternalEffectClaim
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
	return queryExternalEffectClaims(ctx, s.db, s.instance, `WHERE c.tenant_id = ? AND c.person_id = ? AND c.state <> 'observed'
		ORDER BY c.created_at, c.id LIMIT ?`, normalizeTenant(tenantID), personID, limit)
}

// ExternalEffectRef names one effect group: the Run that dispatched it and its
// effect id.
type ExternalEffectRef struct {
	RunID, EffectID string
}

// FindUnresolvedExternalEffectForTargets binds an owner-reviewed observation
// to an effect that has already crossed dispatch in the observing Run or an
// earlier Run of its exact chain. It returns no identity when zero or multiple
// exact groups match; a watcher cannot infer causal ownership from a target
// name alone.
func (s *Store) FindUnresolvedExternalEffectForTargets(ctx context.Context, tenantID, personID, runID string, targets []string) (ExternalEffectRef, error) {
	if s == nil || s.db == nil || personID == "" || runID == "" || len(targets) == 0 {
		return ExternalEffectRef{}, nil
	}
	tenantID = normalizeTenant(tenantID)
	lineage, err := runLineage(ctx, s.db, tenantID, personID, runID)
	if err != nil {
		return ExternalEffectRef{}, err
	}
	want := append([]string(nil), targets...)
	slices.Sort(want)
	args := []any{tenantID, personID}
	for id := range lineage {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT run_id, effect_id, target_key, state FROM external_effect_claims
		WHERE tenant_id = ? AND person_id = ? AND run_id IN (`+placeholders(len(lineage))+`) ORDER BY run_id, effect_id, target_key`,
		args...)
	if err != nil {
		return ExternalEffectRef{}, err
	}
	defer rows.Close()
	type group struct {
		targets      []string
		allUncertain bool
	}
	groups := map[ExternalEffectRef]*group{}
	for rows.Next() {
		var ref ExternalEffectRef
		var target, state string
		if err := rows.Scan(&ref.RunID, &ref.EffectID, &target, &state); err != nil {
			return ExternalEffectRef{}, err
		}
		if !lineage[ref.RunID] {
			continue
		}
		g := groups[ref]
		if g == nil {
			g = &group{allUncertain: true}
			groups[ref] = g
		}
		g.targets = append(g.targets, target)
		g.allUncertain = g.allUncertain && state == ExternalClaimUncertain
	}
	if err := rows.Err(); err != nil {
		return ExternalEffectRef{}, err
	}
	var matched ExternalEffectRef
	for ref, g := range groups {
		if !g.allUncertain || !slices.Equal(g.targets, want) {
			continue
		}
		if matched.EffectID != "" {
			return ExternalEffectRef{}, nil
		}
		matched = ref
	}
	return matched, nil
}

// ObserveEffectClaimsForWatch releases only the exact effect group frozen at
// watcher registration. The finalized event and typed successful observation
// must already be durable. A revoked owner assertion or different target set
// leaves the claim occupied for explicit review.
func (s *Store) ObserveEffectClaimsForWatch(ctx context.Context, tenantID, watchID string) (bool, error) {
	if s == nil || s.db == nil || watchID == "" {
		return false, nil
	}
	tenantID = normalizeTenant(tenantID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var personID, workspaceID, runID, status, adapter, command, output, receiptJSON string
	var revision int
	var watchSequence, watchCreated int64
	err = tx.QueryRowContext(ctx, `SELECT person_id, COALESCE(workspace_id,''), run_id, status,
		COALESCE(observation_adapter,''), command, COALESCE(last_output,''), verdict_revision,
		COALESCE(preflight_receipt_json,'{}'), sequence, created_at FROM external_watches WHERE tenant_id=? AND id=?`, tenantID, watchID).
		Scan(&personID, &workspaceID, &runID, &status, &adapter, &command, &output, &revision, &receiptJSON, &watchSequence, &watchCreated)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var receipt ExternalWatchPreflightReceipt
	if json.Unmarshal([]byte(receiptJSON), &receipt) != nil || receipt.RecoveryClaimID != "" || receipt.EffectID == "" ||
		receipt.EffectRuleKey == "" || receipt.ObservationRuleKey == "" || len(receipt.EffectTargetKeys) == 0 ||
		receipt.EffectScriptRoot == "" || receipt.EffectScriptPath == "" || receipt.EffectScriptDigest == "" ||
		status != ExternalWatchSucceeded || adapter != "status_json.v1" ||
		receipt.Version < ExternalWatchContinuationReceiptVersion ||
		receipt.CommandHash != fmt.Sprintf("%x", sha256.Sum256([]byte(command))) {
		return false, nil
	}
	const rulePrefix = "rule:effect_script:v1:"
	if !strings.HasPrefix(receipt.EffectRuleKey, rulePrefix) {
		return false, nil
	}
	rawRule, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(receipt.EffectRuleKey, rulePrefix))
	if err != nil || len(rawRule) > 8192 {
		return false, nil
	}
	var rule struct {
		WorkspaceID         string   `json:"w"`
		Targets             []string `json:"t"`
		ObservationRelative string   `json:"or"`
		ObservationDigest   string   `json:"od"`
	}
	if json.Unmarshal(rawRule, &rule) != nil || rule.WorkspaceID != workspaceID ||
		!slices.Equal(rule.Targets, receipt.EffectTargetKeys) || rule.ObservationDigest != receipt.EffectScriptDigest ||
		rule.ObservationRelative == "" {
		return false, nil
	}
	rel, err := filepath.Rel(filepath.Clean(receipt.EffectScriptRoot), filepath.Clean(receipt.EffectScriptPath))
	if err != nil || filepath.ToSlash(rel) != rule.ObservationRelative {
		return false, nil
	}
	var observed struct {
		Status string `json:"status"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(output)), &observed) != nil || observed.Status != "succeeded" {
		return false, nil
	}
	var event int
	eventKey := fmt.Sprintf("external-watch:%s:r%d:completed", watchID, revision)
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM task_events e JOIN runs r ON r.id=e.run_id
		WHERE r.tenant_id=? AND r.person_id=? AND e.run_id=? AND e.idempotency_key=? LIMIT 1`,
		tenantID, personID, runID, eventKey).Scan(&event); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if !strings.HasPrefix(receipt.ObservationRuleKey, "rule:observation_script:") {
		return false, nil
	}
	var grants int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT pattern_key) FROM approval_grants WHERE tenant_id=? AND person_id=?
		AND pattern_key IN (?,?) AND revoked_at=0 AND (expires_at=0 OR expires_at>?)
		AND ((scope_kind='person' AND scope_id=?) OR (scope_kind='workspace' AND scope_id=?))`,
		tenantID, personID, receipt.EffectRuleKey, receipt.ObservationRuleKey, time.Now().Unix(), personID, workspaceID).Scan(&grants)
	if err != nil {
		return false, err
	}
	if grants != 2 {
		return false, nil
	}
	var trust string
	err = tx.QueryRowContext(ctx, `SELECT trust_level FROM workspaces WHERE tenant_id=? AND owner_person_id=? AND id=?`,
		tenantID, personID, workspaceID).Scan(&trust)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if trust != "trusted" {
		return false, nil
	}
	// The effect may belong to an earlier Run of the watcher's exact chain.
	effectRun := runID
	if receipt.EffectRunID != "" && receipt.EffectRunID != runID {
		lineage, err := runLineage(ctx, tx, tenantID, personID, runID)
		if err != nil {
			return false, err
		}
		if !lineage[receipt.EffectRunID] {
			return false, nil
		}
		effectRun = receipt.EffectRunID
	}
	claims, err := loadExternalEffectClaimsTx(ctx, tx, tenantID, effectRun, receipt.EffectID)
	if err != nil {
		return false, err
	}
	if len(claims) != len(receipt.EffectTargetKeys) {
		return false, nil
	}
	for i, claim := range claims {
		if claim.PersonID != personID || claim.TargetKey != receipt.EffectTargetKeys[i] || claim.State != ExternalClaimUncertain {
			return false, nil
		}
	}
	follows, err := watchFollowsEffectTx(ctx, tx, tenantID, effectRun, receipt.EffectID, watchSequence, watchCreated)
	if err != nil || !follows {
		return false, err
	}
	ref := fmt.Sprintf("watch:%s:r%d:owner-bound", watchID, revision)
	if _, err := tx.ExecContext(ctx, `UPDATE external_effect_claims SET state='observed', observation_ref=?, updated_at=?
		WHERE tenant_id=? AND person_id=? AND run_id=? AND effect_id=? AND state='uncertain'`,
		ref, time.Now().Unix(), tenantID, personID, effectRun, receipt.EffectID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// ObserveExternalEffectWithWatch is the conservative fallback for an unknown
// external target. The person explicitly relates an exact effect to a
// successful, finalized daemon observation created by the same Run or an explicitly bound exact continuation after the
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
	err = tx.QueryRowContext(ctx, `SELECT id, tenant_id, person_id, run_id, effect_id, target_key, state, observation_ref, sequence
		FROM external_effect_claims WHERE tenant_id = ? AND person_id = ? AND id = ?`,
		tenantID, personID, claimID).Scan(&claim.ID, &claim.TenantID, &claim.PersonID, &claim.RunID,
		&claim.EffectID, &claim.TargetKey, &claim.State, &claim.ObservationRef, &claim.Sequence)
	if err != nil {
		return ExternalEffectClaim{}, fmt.Errorf("external effect claim not found for this person: %w", err)
	}
	var watchRun, watchStatus, receiptJSON, command string
	var watchFinalized int
	var watchCreated, watchSequence int64
	var revision int
	err = tx.QueryRowContext(ctx, `SELECT run_id, status, finalized, created_at, verdict_revision,
		COALESCE(preflight_receipt_json, '{}'), command, sequence FROM external_watches
		WHERE tenant_id = ? AND person_id = ? AND id = ?`, tenantID, personID, watchID).
		Scan(&watchRun, &watchStatus, &watchFinalized, &watchCreated, &revision, &receiptJSON, &command, &watchSequence)
	if err != nil {
		return ExternalEffectClaim{}, fmt.Errorf("watcher not found for this person: %w", err)
	}
	var receipt ExternalWatchPreflightReceipt
	if err := json.Unmarshal([]byte(receiptJSON), &receipt); err != nil {
		return ExternalEffectClaim{}, fmt.Errorf("watcher has no valid preflight receipt: %w", err)
	}
	if watchRun != claim.RunID {
		if receipt.Version < ExternalWatchRecoveryReceiptVersion || receipt.RecoveryClaimID != claim.ID {
			return ExternalEffectClaim{}, fmt.Errorf("continuation observation requires an explicit versioned binding to this effect claim")
		}
		if _, err := externalEffectForObservationTx(ctx, tx, tenantID, personID, watchRun, claim.ID); err != nil {
			return ExternalEffectClaim{}, err
		}
	}
	if receipt.RecoveryClaimID != "" && receipt.RecoveryClaimID != claim.ID {
		return ExternalEffectClaim{}, fmt.Errorf("watcher is bound to another effect claim")
	}
	follows, err := watchFollowsEffectTx(ctx, tx, tenantID, claim.RunID, claim.EffectID, watchSequence, watchCreated)
	if err != nil {
		return ExternalEffectClaim{}, err
	}
	if !follows || watchStatus != ExternalWatchSucceeded ||
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
	conflicts, err := classifyExternalBlockers(ctx, tx, s.instance, request.TenantID, request.PersonID, request.RunID, targets)
	if err != nil {
		return ExternalEffectClaimDecision{}, err
	}
	// The Run's own chain must observe or have the person settle its effect;
	// waiting on itself could never end.
	if len(conflicts.Own) > 0 {
		return ExternalEffectClaimDecision{BlockedByRun: conflicts.Own[0].RunID, NeedsObservation: true, BlockingClaims: conflicts.All}, nil
	}
	// Another Run's left-uncertain effect passes once: by the person's approval
	// of this exact call, or by this Run's own later observation bound to it.
	// Neither releases that claim.
	tolerated := map[string]bool{}
	for _, id := range request.ToleratedClaimIDs {
		tolerated[strings.TrimSpace(id)] = true
	}
	var unpassed []ExternalEffectClaim
	for _, blocker := range conflicts.Foreign {
		if !tolerated[blocker.ID] {
			unpassed = append(unpassed, blocker)
		}
	}
	// An in-flight or watched claim can still settle without a decision; an
	// unpassed left-uncertain claim is decided after the wait.
	if len(conflicts.Waiting) > 0 {
		blockedBy := conflicts.Waiting[0].RunID
		if request.DeferWait {
			return ExternalEffectClaimDecision{BlockedByRun: blockedBy, BlockingClaims: conflicts.All, DecideAfterWait: unpassed}, nil
		}
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
		return ExternalEffectClaimDecision{BlockedByRun: blockedBy, BlockingClaims: conflicts.All, DecideAfterWait: unpassed}, nil
	}
	if len(unpassed) > 0 {
		return ExternalEffectClaimDecision{BlockedByRun: unpassed[0].RunID, NeedsApproval: true,
			ApprovalClaims: unpassed, BlockingClaims: conflicts.All}, nil
	}
	if err := spendExternalEffectAcknowledgmentsTx(ctx, tx, request, conflicts, tolerated); err != nil {
		return ExternalEffectClaimDecision{}, err
	}
	now := time.Now().Unix()
	sequence, err := nextEffectSequenceTx(ctx, tx)
	if err != nil {
		return ExternalEffectClaimDecision{}, err
	}
	claims := make([]ExternalEffectClaim, 0, len(targets))
	for _, target := range targets {
		claim := ExternalEffectClaim{ID: "effectclaim_" + uuid.NewString(), TenantID: request.TenantID,
			PersonID: request.PersonID, RunID: request.RunID, EffectID: request.EffectID,
			TargetKey: target, State: ExternalClaimReserved, Sequence: sequence}
		if _, err := tx.ExecContext(ctx, `INSERT INTO external_effect_claims
			(id, tenant_id, person_id, run_id, effect_id, target_key, state, sequence, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, claim.ID, claim.TenantID, claim.PersonID,
			claim.RunID, claim.EffectID, claim.TargetKey, claim.State, claim.Sequence, now, now); err != nil {
			return ExternalEffectClaimDecision{}, err
		}
		claims = append(claims, claim)
	}
	if err := tx.Commit(); err != nil {
		return ExternalEffectClaimDecision{}, err
	}
	return ExternalEffectClaimDecision{Claims: claims, Granted: true}, nil
}

// spendExternalEffectAcknowledgmentsTx records, in the claim's transaction,
// that this call used each observation that let it pass another Run's claim,
// so one observation passes one call.
func spendExternalEffectAcknowledgmentsTx(ctx context.Context, tx *sql.Tx, request ExternalEffectClaimRequest,
	conflicts ExternalEffectConflicts, tolerated map[string]bool) error {
	spent := map[string]bool{}
	for _, blocker := range conflicts.Acknowledged {
		ack := conflicts.acks[blocker.ID]
		if tolerated[blocker.ID] || spent[ack.EventID] {
			continue
		}
		spent[ack.EventID] = true
		payload, err := json.Marshal(map[string]string{"observation_event_id": ack.EventID, "effect_id": request.EffectID})
		if err != nil {
			return err
		}
		inserted, _, err := insertEffectEventTx(ctx, tx, request.TenantID, request.RunID, ExternalEffectObservationSpent,
			payload, "external-effect-observation-consumed:"+ack.EventID)
		if err != nil {
			return err
		}
		if !inserted {
			return fmt.Errorf("external effect observation was already used")
		}
	}
	return nil
}

func loadExternalEffectClaimsTx(ctx context.Context, tx *sql.Tx, tenantID, runID, effectID string) ([]ExternalEffectClaim, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, tenant_id, person_id, run_id, effect_id, target_key, state, observation_ref, sequence
		FROM external_effect_claims WHERE tenant_id = ? AND run_id = ? AND effect_id = ? ORDER BY target_key`, tenantID, runID, effectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var claims []ExternalEffectClaim
	for rows.Next() {
		var claim ExternalEffectClaim
		if err := rows.Scan(&claim.ID, &claim.TenantID, &claim.PersonID, &claim.RunID, &claim.EffectID,
			&claim.TargetKey, &claim.State, &claim.ObservationRef, &claim.Sequence); err != nil {
			return nil, err
		}
		claims = append(claims, claim)
	}
	return claims, rows.Err()
}

// nextEffectSequenceTx takes the next value of the daemon-wide append counter
// that also stamps event cursors (AppendEvent). Claims, watchers, and events
// therefore share one order. SQLite admits one writer at a time and a rolled
// back transaction returns its value, so a larger committed value was
// committed later.
func nextEffectSequenceTx(ctx context.Context, tx *sql.Tx) (int64, error) {
	var next int64
	if err := tx.QueryRowContext(ctx,
		`UPDATE event_sequence SET next_cursor = next_cursor + 1 WHERE id = 1 RETURNING next_cursor`,
	).Scan(&next); err != nil {
		return 0, fmt.Errorf("allocate effect sequence: %w", err)
	}
	return next, nil
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
	waits, err := s.ListActiveExternalResourceWaits(ctx, limit)
	if err != nil {
		return nil, err
	}
	var ready []ExternalResourceWait
	for _, wait := range waits {
		if wait.Ready && wait.RunStatus == "waiting_external" {
			ready = append(ready, wait.ExternalResourceWait)
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
		WHERE tenant_id = ? AND run_id = ? AND status = 'pending' AND EXISTS (SELECT 1 FROM runs r WHERE r.tenant_id=external_resource_waits.tenant_id AND r.id=external_resource_waits.run_id AND r.status='waiting_external')`, normalizeTenant(tenantID), runID).Scan(&count)
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
