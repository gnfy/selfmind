package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// externalEffectSettlementRefPrefix marks a claim released by its owner's
// settlement rather than by a daemon observation.
const externalEffectSettlementRefPrefix = "settlement:"

const (
	externalEffectSettlementReasonMaxRunes  = 500
	externalEffectSettlementChannelMaxBytes = 256
)

var (
	// ErrExternalEffectClaimNotFound is returned for an unknown claim and for
	// another person's claim alike, so a settle never reveals what exists.
	ErrExternalEffectClaimNotFound = errors.New("external effect claim not found for this person")
	// ErrExternalEffectNotSettleable reports a claim that is not an uncertain,
	// dispatched effect: it is already observed, or it never crossed dispatch
	// and its own Run releases it.
	ErrExternalEffectNotSettleable = errors.New("external effect is not an uncertain dispatched effect")
	// ErrExternalEffectSettlementActor reports an actor that is not an active
	// account of the settling person, or a channel that is not a bounded
	// endpoint of that account.
	ErrExternalEffectSettlementActor = errors.New("settlement actor is not an active endpoint of this person")
)

// ExternalEffectSettlement is the audit record of a person settling one
// uncertain external effect (owner decision D2). The person's confirmation is
// the judgment that the effect's outcome is known; the record names who
// settled it, from which endpoint, and why.
type ExternalEffectSettlement struct {
	ID             string
	TenantID       string
	PersonID       string
	ClaimID        string
	RunID          string
	EffectID       string
	ActorAccountID string
	ActorChannel   string
	Reason         string
	CreatedAt      time.Time
}

// ObservationRef is the reference every claim row of the settled effect
// carries, resolvable back to this record.
func (s ExternalEffectSettlement) ObservationRef() string {
	return externalEffectSettlementRefPrefix + s.ID
}

// ExternalEffectSettlementRequest names the claim the person settles and the
// authenticated endpoint the settle came from.
type ExternalEffectSettlementRequest struct {
	TenantID       string
	PersonID       string
	ClaimID        string
	ActorAccountID string
	ActorChannel   string
	Reason         string
}

// ExternalEffectSettlementResult is the effect's single settlement and its
// claim rows. Replayed reports that an earlier settle of the same effect
// already recorded it; nothing new was written.
type ExternalEffectSettlementResult struct {
	Settlement ExternalEffectSettlement
	Claims     []ExternalEffectClaim
	Replayed   bool
}

// SettleExternalEffect records the person's settlement of one uncertain effect
// in one transaction: it checks that the actor is the person's own active
// account and the claim is the person's, marks every unobserved claim row of
// the effect group observed with a reference to the settlement, and writes the
// audit row. Earlier observations of sibling rows keep their references. A
// retry, or a settle naming another target of the same effect, returns the
// recorded settlement; another person can neither settle nor see the claim.
func (s *Store) SettleExternalEffect(ctx context.Context, request ExternalEffectSettlementRequest) (ExternalEffectSettlementResult, error) {
	if s == nil || s.db == nil {
		return ExternalEffectSettlementResult{}, fmt.Errorf("external effect store is unavailable")
	}
	request.TenantID = normalizeTenant(request.TenantID)
	request.PersonID = strings.TrimSpace(request.PersonID)
	request.ClaimID = strings.TrimSpace(request.ClaimID)
	request.ActorAccountID = strings.TrimSpace(request.ActorAccountID)
	request.ActorChannel = strings.TrimSpace(strings.ToValidUTF8(request.ActorChannel, ""))
	request.Reason = strings.TrimSpace(strings.ToValidUTF8(request.Reason, ""))
	if utf8.RuneCountInString(request.Reason) > externalEffectSettlementReasonMaxRunes {
		request.Reason = string([]rune(request.Reason)[:externalEffectSettlementReasonMaxRunes])
	}
	if request.PersonID == "" || request.ClaimID == "" {
		return ExternalEffectSettlementResult{}, ErrExternalEffectClaimNotFound
	}
	if request.ActorAccountID == "" || request.ActorChannel == "" || len(request.ActorChannel) > externalEffectSettlementChannelMaxBytes {
		return ExternalEffectSettlementResult{}, ErrExternalEffectSettlementActor
	}
	for attempt := 0; attempt < 8; attempt++ {
		result, err := s.settleExternalEffectOnce(ctx, request)
		if (!isSQLiteBusy(err) && !isExternalEffectSettlementConflict(err)) || ctx.Err() != nil {
			return result, err
		}
		select {
		case <-ctx.Done():
			return ExternalEffectSettlementResult{}, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 5 * time.Millisecond):
		}
	}
	return ExternalEffectSettlementResult{}, fmt.Errorf("external effect settlement remained busy")
}

func (s *Store) settleExternalEffectOnce(ctx context.Context, request ExternalEffectSettlementRequest) (ExternalEffectSettlementResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExternalEffectSettlementResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var accountPerson, accountStatus, accountKey string
	err = tx.QueryRowContext(ctx, `SELECT person_id, status, platform_user_id FROM accounts WHERE tenant_id = ? AND id = ?`,
		request.TenantID, request.ActorAccountID).Scan(&accountPerson, &accountStatus, &accountKey)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (accountPerson != request.PersonID || accountStatus != "active") {
		return ExternalEffectSettlementResult{}, ErrExternalEffectSettlementActor
	}
	if err != nil {
		return ExternalEffectSettlementResult{}, err
	}
	// An IM channel is keyed by its account's platform user id. A channel
	// that is another account's key is not this actor's endpoint; a CLI
	// session or a group chat is no account's key.
	if request.ActorChannel != accountKey {
		var others int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE tenant_id = ? AND platform_user_id = ? AND id <> ?`,
			request.TenantID, request.ActorChannel, request.ActorAccountID).Scan(&others); err != nil {
			return ExternalEffectSettlementResult{}, err
		}
		if others != 0 {
			return ExternalEffectSettlementResult{}, ErrExternalEffectSettlementActor
		}
	}
	var claim ExternalEffectClaim
	err = tx.QueryRowContext(ctx, `SELECT id, tenant_id, person_id, run_id, effect_id, target_key, state, observation_ref, sequence
		FROM external_effect_claims WHERE tenant_id = ? AND id = ?`, request.TenantID, request.ClaimID).Scan(
		&claim.ID, &claim.TenantID, &claim.PersonID, &claim.RunID, &claim.EffectID, &claim.TargetKey,
		&claim.State, &claim.ObservationRef, &claim.Sequence)
	if errors.Is(err, sql.ErrNoRows) || err == nil && claim.PersonID != request.PersonID {
		return ExternalEffectSettlementResult{}, ErrExternalEffectClaimNotFound
	}
	if err != nil {
		return ExternalEffectSettlementResult{}, err
	}
	settlement, found, err := externalEffectSettlementTx(ctx, tx, request.TenantID, claim.RunID, claim.EffectID)
	if err != nil {
		return ExternalEffectSettlementResult{}, err
	}
	if found {
		if settlement.PersonID != request.PersonID {
			return ExternalEffectSettlementResult{}, ErrExternalEffectClaimNotFound
		}
		claims, err := loadExternalEffectClaimsTx(ctx, tx, request.TenantID, claim.RunID, claim.EffectID)
		if err != nil {
			return ExternalEffectSettlementResult{}, err
		}
		return ExternalEffectSettlementResult{Settlement: settlement, Claims: claims, Replayed: true}, nil
	}
	if claim.State == ExternalClaimUncertain {
		// Nobody can know the outcome of a call that has not returned while its
		// owner is live; the person settles only a left-uncertain effect.
		rows, err := tx.QueryContext(ctx, externalClaimFactsSQL+` WHERE c.tenant_id=? AND c.id=?`, request.TenantID, claim.ID)
		if err != nil {
			return ExternalEffectSettlementResult{}, err
		}
		facts, err := scanExternalClaimFacts(rows, s.instance)
		if err != nil {
			return ExternalEffectSettlementResult{}, err
		}
		if len(facts) == 1 {
			if disposition, _ := facts[0].disposition(false); disposition == ExternalClaimInFlight {
				return ExternalEffectSettlementResult{}, ErrExternalEffectInFlight
			}
		}
	}
	if claim.State != ExternalClaimUncertain {
		return ExternalEffectSettlementResult{}, fmt.Errorf("%w: claim is %s", ErrExternalEffectNotSettleable, claim.State)
	}
	now := time.Now()
	settlement = ExternalEffectSettlement{
		ID: "effectsettle_" + uuid.NewString(), TenantID: request.TenantID, PersonID: request.PersonID,
		ClaimID: claim.ID, RunID: claim.RunID, EffectID: claim.EffectID,
		ActorAccountID: request.ActorAccountID, ActorChannel: request.ActorChannel, Reason: request.Reason,
		CreatedAt: time.Unix(now.Unix(), 0),
	}
	if _, err := tx.ExecContext(ctx, `UPDATE external_effect_claims SET state = 'observed', observation_ref = ?, updated_at = ?
		WHERE tenant_id = ? AND person_id = ? AND run_id = ? AND effect_id = ? AND state <> 'observed'`,
		settlement.ObservationRef(), now.Unix(), request.TenantID, request.PersonID, claim.RunID, claim.EffectID); err != nil {
		return ExternalEffectSettlementResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO external_effect_settlements
		(id, tenant_id, person_id, claim_id, run_id, effect_id, actor_account_id, actor_channel, reason, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, settlement.ID, settlement.TenantID, settlement.PersonID,
		settlement.ClaimID, settlement.RunID, settlement.EffectID, settlement.ActorAccountID,
		settlement.ActorChannel, settlement.Reason, now.Unix()); err != nil {
		return ExternalEffectSettlementResult{}, err
	}
	claims, err := loadExternalEffectClaimsTx(ctx, tx, request.TenantID, claim.RunID, claim.EffectID)
	if err != nil {
		return ExternalEffectSettlementResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ExternalEffectSettlementResult{}, err
	}
	return ExternalEffectSettlementResult{Settlement: settlement, Claims: claims}, nil
}

func externalEffectSettlementTx(ctx context.Context, tx *sql.Tx, tenantID, runID, effectID string) (ExternalEffectSettlement, bool, error) {
	var settlement ExternalEffectSettlement
	var created int64
	err := tx.QueryRowContext(ctx, `SELECT id, tenant_id, person_id, claim_id, run_id, effect_id,
		actor_account_id, actor_channel, reason, created_at
		FROM external_effect_settlements WHERE tenant_id = ? AND run_id = ? AND effect_id = ?`,
		tenantID, runID, effectID).Scan(&settlement.ID, &settlement.TenantID, &settlement.PersonID,
		&settlement.ClaimID, &settlement.RunID, &settlement.EffectID, &settlement.ActorAccountID,
		&settlement.ActorChannel, &settlement.Reason, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return ExternalEffectSettlement{}, false, nil
	}
	if err != nil {
		return ExternalEffectSettlement{}, false, err
	}
	settlement.CreatedAt = time.Unix(created, 0)
	return settlement, true, nil
}

// isExternalEffectSettlementConflict recognizes a concurrent settle of the same
// effect committing first; the retry then returns that settlement.
func isExternalEffectSettlementConflict(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique constraint failed") && strings.Contains(message, "external_effect_settlements")
}
