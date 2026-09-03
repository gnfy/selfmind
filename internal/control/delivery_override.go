package control

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// RunDeliveryTarget is the one explicitly selected final-result endpoint for
// a Run. SourceSteeringID proves the endpoint came from an authenticated live
// user input rather than model-supplied account data.
type RunDeliveryTarget struct {
	TenantID         string
	PersonID         string
	RunID            string
	Platform         string
	PlatformUserID   string
	Channel          string
	SourceSteeringID string
}

func (s *Store) SetRunDeliveryOverrideFromSteering(ctx context.Context, tenantID, personID, runID, steeringID string) (*RunDeliveryTarget, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("control store is unavailable")
	}
	tenantID = normalizeTenant(tenantID)
	personID = strings.TrimSpace(personID)
	runID = strings.TrimSpace(runID)
	steeringID = strings.TrimSpace(steeringID)
	if personID == "" || runID == "" || steeringID == "" {
		return nil, fmt.Errorf("person, run, and live input id are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var runPerson, runStatus string
	if err := tx.QueryRowContext(ctx, `SELECT person_id, status FROM runs WHERE tenant_id = ? AND id = ?`, tenantID, runID).Scan(&runPerson, &runStatus); err != nil {
		return nil, fmt.Errorf("load active run: %w", err)
	}
	if runPerson != personID || runStatus != "running" {
		return nil, fmt.Errorf("delivery target can change only for the current person's active run")
	}
	target := &RunDeliveryTarget{TenantID: tenantID, PersonID: personID, RunID: runID, SourceSteeringID: steeringID}
	var steeringPerson, steeringRun, steeringStatus string
	if err := tx.QueryRowContext(ctx, `SELECT person_id, COALESCE(run_id, ''), COALESCE(platform, ''),
		COALESCE(platform_user_id, ''), COALESCE(channel, ''), status
		FROM steering_mailbox WHERE tenant_id = ? AND id = ?`, tenantID, steeringID).
		Scan(&steeringPerson, &steeringRun, &target.Platform, &target.PlatformUserID, &target.Channel, &steeringStatus); err != nil {
		return nil, fmt.Errorf("load live input: %w", err)
	}
	if steeringPerson != personID || steeringRun != runID {
		return nil, fmt.Errorf("live input does not belong to this run")
	}
	switch steeringStatus {
	case SteeringAccepted, SteeringClaimed, SteeringConsumed:
	default:
		return nil, fmt.Errorf("live input is no longer eligible to select delivery")
	}
	target.Platform = strings.TrimSpace(target.Platform)
	target.PlatformUserID = strings.TrimSpace(target.PlatformUserID)
	target.Channel = strings.TrimSpace(target.Channel)
	if target.Platform == "" || target.PlatformUserID == "" || target.Channel == "" ||
		strings.EqualFold(target.Platform, "cli") || strings.EqualFold(target.Platform, "eval") {
		return nil, fmt.Errorf("the live input endpoint has no push delivery surface")
	}
	var bound int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts
		WHERE tenant_id = ? AND person_id = ? AND platform = ? AND platform_user_id = ?`,
		tenantID, personID, target.Platform, target.PlatformUserID).Scan(&bound); err != nil {
		return nil, err
	}
	if bound != 1 {
		return nil, fmt.Errorf("the live input endpoint is not bound to the current person")
	}
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `INSERT INTO run_delivery_overrides
		(tenant_id, person_id, run_id, platform, platform_user_id, channel, source_steering_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(tenant_id, run_id) DO UPDATE SET
			platform = excluded.platform,
			platform_user_id = excluded.platform_user_id,
			channel = excluded.channel,
			source_steering_id = excluded.source_steering_id,
			updated_at = excluded.updated_at`,
		tenantID, personID, runID, target.Platform, target.PlatformUserID, target.Channel, steeringID, now, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return target, nil
}

func (s *Store) RunDeliveryOverride(ctx context.Context, tenantID, personID, runID string) (*RunDeliveryTarget, error) {
	if s == nil || s.db == nil || strings.TrimSpace(personID) == "" || strings.TrimSpace(runID) == "" {
		return nil, nil
	}
	target := &RunDeliveryTarget{}
	err := s.db.QueryRowContext(ctx, `SELECT tenant_id, person_id, run_id, platform, platform_user_id, channel, source_steering_id
		FROM run_delivery_overrides WHERE tenant_id = ? AND person_id = ? AND run_id = ?`,
		normalizeTenant(tenantID), strings.TrimSpace(personID), strings.TrimSpace(runID)).
		Scan(&target.TenantID, &target.PersonID, &target.RunID, &target.Platform, &target.PlatformUserID, &target.Channel, &target.SourceSteeringID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return target, nil
}
