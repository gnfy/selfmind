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

func (s *Store) SetRunDeliveryOverrideToBoundPlatform(ctx context.Context, tenantID, personID, runID, platform string) (*RunDeliveryTarget, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("control store is unavailable")
	}
	tenantID = normalizeTenant(tenantID)
	personID = strings.TrimSpace(personID)
	runID = strings.TrimSpace(runID)
	platform = strings.TrimSpace(platform)
	if personID == "" || runID == "" || platform == "" {
		return nil, fmt.Errorf("person, run, and target platform are required")
	}
	if !isPushCapablePlatformSelector(platform) {
		return nil, fmt.Errorf("platform %q has no push delivery surface", platform)
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
	rows, err := tx.QueryContext(ctx, `SELECT id, platform, platform_user_id
		FROM accounts
		WHERE tenant_id = ? AND person_id = ? AND status = 'active' AND lower(platform) NOT IN ('cli', 'eval')
		ORDER BY COALESCE(last_seen_at, 0) DESC, created_at ASC, rowid ASC`,
		tenantID, personID)
	if err != nil {
		return nil, err
	}
	var accountID, matchedPlatform, matchedUserID string
	for rows.Next() {
		var id, acctPlatform, acctUserID string
		if err := rows.Scan(&id, &acctPlatform, &acctUserID); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if matchesBoundDeliveryPlatform(platform, acctPlatform) {
			accountID = strings.TrimSpace(id)
			matchedPlatform = strings.TrimSpace(acctPlatform)
			matchedUserID = strings.TrimSpace(acctUserID)
			break
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if matchedPlatform == "" || matchedUserID == "" {
		return nil, fmt.Errorf("no bound %s account is available for the current person", platform)
	}
	sourceID := "bound:" + runID + ":" + accountID
	target := &RunDeliveryTarget{
		TenantID:         tenantID,
		PersonID:         personID,
		RunID:            runID,
		Platform:         matchedPlatform,
		PlatformUserID:   matchedUserID,
		Channel:          matchedUserID,
		SourceSteeringID: sourceID,
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
		tenantID, personID, runID, target.Platform, target.PlatformUserID, target.Channel, sourceID, now, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return target, nil
}

func isPushCapablePlatformSelector(selector string) bool {
	switch strings.ToLower(strings.TrimSpace(selector)) {
	case "", "cli", "eval", "terminal", "tui":
		return false
	default:
		return true
	}
}

func matchesBoundDeliveryPlatform(selector, accountPlatform string) bool {
	sel := strings.ToLower(strings.TrimSpace(selector))
	acct := strings.ToLower(strings.TrimSpace(accountPlatform))
	if acct == "" || acct == "cli" || acct == "eval" {
		return false
	}
	switch sel {
	case "im", "preferred", "default":
		return true
	case "weixin", "wechat", "微信":
		return acct == "weixin" || acct == "wechat"
	case "telegram", "tg":
		return acct == "telegram"
	default:
		return acct == sel
	}
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
