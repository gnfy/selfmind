package control

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const (
	ProviderRouteClosed = "closed"
	ProviderRouteOpen   = "open"
)

// ProviderRouteHealth is durable quota-circuit state for one physical model
// route. route_id is a one-way digest; credentials are never persisted.
type ProviderRouteHealth struct {
	TenantID           string
	RouteID            string
	Provider           string
	Model              string
	State              string
	FailureClass       string
	ConsecutiveFailure int
	OpenedAt           time.Time
	NextProbeAt        time.Time
	ProbeLeaseUntil    time.Time
	LastError          string
	LastRequestID      string
	UpdatedAt          time.Time
}

// ClaimProviderRoute allows normal calls while closed and exactly one
// half-open probe after the cooldown. Store failures are returned to the
// caller, which may choose to fail open so background governance is not lost.
func (s *Store) ClaimProviderRoute(ctx context.Context, tenantID, routeID, provider, model string, now time.Time, probeLease time.Duration) (allowed, probe bool, nextProbe time.Time, err error) {
	if s == nil || s.db == nil || routeID == "" {
		return true, false, time.Time{}, nil
	}
	if probeLease <= 0 {
		probeLease = 2 * time.Minute
	}
	tenantID = normalizeTenant(tenantID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, false, time.Time{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var state string
	var nextProbeUnix, leaseUnix int64
	err = tx.QueryRowContext(ctx, `SELECT state, next_probe_at, probe_lease_until
		FROM provider_route_health WHERE tenant_id = ? AND route_id = ?`, tenantID, routeID).
		Scan(&state, &nextProbeUnix, &leaseUnix)
	if err == sql.ErrNoRows {
		_, err = tx.ExecContext(ctx, `INSERT INTO provider_route_health
			(tenant_id, route_id, provider, model, state, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`, tenantID, routeID, provider, model, ProviderRouteClosed, now.Unix())
		if err != nil {
			return false, false, time.Time{}, err
		}
		if err = tx.Commit(); err != nil {
			return false, false, time.Time{}, err
		}
		return true, false, time.Time{}, nil
	}
	if err != nil {
		return false, false, time.Time{}, err
	}
	if state != ProviderRouteOpen {
		if err = tx.Commit(); err != nil {
			return false, false, time.Time{}, err
		}
		return true, false, time.Time{}, nil
	}
	nextProbe = unixTime(nextProbeUnix)
	if nextProbeUnix > now.Unix() || leaseUnix > now.Unix() {
		if err = tx.Commit(); err != nil {
			return false, false, nextProbe, err
		}
		return false, false, nextProbe, nil
	}
	res, err := tx.ExecContext(ctx, `UPDATE provider_route_health
		SET probe_lease_until = ?, updated_at = ?
		WHERE tenant_id = ? AND route_id = ? AND state = ? AND probe_lease_until <= ?`,
		now.Add(probeLease).Unix(), now.Unix(), tenantID, routeID, ProviderRouteOpen, now.Unix())
	if err != nil {
		return false, false, nextProbe, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, false, nextProbe, err
	}
	if err = tx.Commit(); err != nil {
		return false, false, nextProbe, err
	}
	return n > 0, n > 0, nextProbe, nil
}

// OpenProviderRoute records a quota failure and exponentially delays the next
// probe. Repeated 403s therefore cost one request per cooldown, not one request
// per queued maintenance job.
func (s *Store) OpenProviderRoute(ctx context.Context, tenantID, routeID, provider, model, failureClass, lastError, requestID string, now time.Time, initial, maximum time.Duration) (time.Time, error) {
	if s == nil || s.db == nil || routeID == "" {
		return time.Time{}, nil
	}
	if initial <= 0 {
		initial = 15 * time.Minute
	}
	if maximum < initial {
		maximum = initial
	}
	lastError = boundedText(lastError, 500)
	requestID = boundedText(requestID, 160)
	tenantID = normalizeTenant(tenantID)
	var failures int
	var openedAt int64
	err := s.db.QueryRowContext(ctx, `SELECT consecutive_failures, opened_at
		FROM provider_route_health WHERE tenant_id = ? AND route_id = ?`, tenantID, routeID).
		Scan(&failures, &openedAt)
	if err != nil && err != sql.ErrNoRows {
		return time.Time{}, err
	}
	failures++
	delay := initial
	for i := 1; i < failures && delay < maximum; i++ {
		if delay > maximum/2 {
			delay = maximum
			break
		}
		delay *= 2
	}
	if delay > maximum {
		delay = maximum
	}
	if openedAt == 0 {
		openedAt = now.Unix()
	}
	next := now.Add(delay)
	_, err = s.db.ExecContext(ctx, `INSERT INTO provider_route_health
		(tenant_id, route_id, provider, model, state, failure_class, consecutive_failures,
		 opened_at, next_probe_at, probe_lease_until, last_error, last_request_id, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?)
		ON CONFLICT(tenant_id, route_id) DO UPDATE SET
		 provider = excluded.provider, model = excluded.model, state = excluded.state,
		 failure_class = excluded.failure_class, consecutive_failures = excluded.consecutive_failures,
		 opened_at = excluded.opened_at, next_probe_at = excluded.next_probe_at,
		 probe_lease_until = 0, last_error = excluded.last_error,
		 last_request_id = excluded.last_request_id, updated_at = excluded.updated_at`,
		tenantID, routeID, provider, model, ProviderRouteOpen, failureClass, failures,
		openedAt, next.Unix(), lastError, requestID, now.Unix())
	return next, err
}

// CloseProviderRoute closes a successful half-open route and requeues only
// maintenance jobs that were paused by that route.
func (s *Store) CloseProviderRoute(ctx context.Context, tenantID, routeID string, now time.Time) (int, error) {
	if s == nil || s.db == nil || routeID == "" {
		return 0, nil
	}
	tenantID = normalizeTenant(tenantID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `UPDATE provider_route_health SET state = ?, failure_class = '',
		consecutive_failures = 0, next_probe_at = 0, probe_lease_until = 0,
		last_error = '', last_request_id = '', updated_at = ?
		WHERE tenant_id = ? AND route_id = ?`, ProviderRouteClosed, now.Unix(), tenantID, routeID); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE maintenance_jobs SET status = ?, attempts = 0,
		next_retry_at = 0, blocked_route_id = '', last_error = '', updated_at = ?
		WHERE tenant_id = ? AND status = ? AND blocked_route_id = ?`,
		MaintenanceJobPending, now.Unix(), tenantID, MaintenanceJobBlockedProvider, routeID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return int(n), nil
}

// DeferProviderRouteProbe keeps the circuit open when a half-open request
// failed for a transient reason rather than quota. It avoids a tight probe
// loop without increasing the quota backoff exponent.
func (s *Store) DeferProviderRouteProbe(ctx context.Context, tenantID, routeID string, now time.Time, delay time.Duration) error {
	if s == nil || s.db == nil || routeID == "" {
		return nil
	}
	if delay <= 0 {
		delay = 15 * time.Minute
	}
	_, err := s.db.ExecContext(ctx, `UPDATE provider_route_health
		SET next_probe_at = ?, probe_lease_until = 0, updated_at = ?
		WHERE tenant_id = ? AND route_id = ? AND state = ?`,
		now.Add(delay).Unix(), now.Unix(), normalizeTenant(tenantID), routeID, ProviderRouteOpen)
	return err
}

// RequeueDueProviderRouteProbes promotes at most one blocked job per due route.
// The provider chain still owns the half-open CAS, so two worker passes cannot
// emit two probes concurrently.
func (s *Store) RequeueDueProviderRouteProbes(ctx context.Context, now time.Time) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT tenant_id, route_id FROM provider_route_health
		WHERE state = ? AND next_probe_at <= ? AND probe_lease_until <= ?`,
		ProviderRouteOpen, now.Unix(), now.Unix())
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	type route struct{ tenant, id string }
	var routes []route
	for rows.Next() {
		var item route
		if err := rows.Scan(&item.tenant, &item.id); err != nil {
			return 0, err
		}
		routes = append(routes, item)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	total := 0
	for _, item := range routes {
		res, err := s.db.ExecContext(ctx, `UPDATE maintenance_jobs SET status = ?, next_retry_at = 0, updated_at = ?
			WHERE rowid = (SELECT rowid FROM maintenance_jobs WHERE tenant_id = ? AND status = ?
			AND blocked_route_id = ? ORDER BY updated_at ASC LIMIT 1)`,
			MaintenanceJobPending, now.Unix(), item.tenant, MaintenanceJobBlockedProvider, item.id)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += int(n)
	}
	return total, nil
}

// RequeueBlockedJobsForInactiveProviderRoutes migrates jobs that were paused
// by a route no longer present in configuration. Changing a credential or
// endpoint creates a new route id, so the old circuit must not strand durable
// maintenance evidence forever. Active route circuits remain untouched.
func (s *Store) RequeueBlockedJobsForInactiveProviderRoutes(ctx context.Context, tenantID string, activeRouteIDs []string, now time.Time) (int, error) {
	if s == nil || s.db == nil || len(activeRouteIDs) == 0 {
		return 0, nil
	}
	unique := make([]string, 0, len(activeRouteIDs))
	seen := make(map[string]struct{}, len(activeRouteIDs))
	for _, routeID := range activeRouteIDs {
		routeID = strings.TrimSpace(routeID)
		if routeID == "" {
			continue
		}
		if _, exists := seen[routeID]; exists {
			continue
		}
		seen[routeID] = struct{}{}
		unique = append(unique, routeID)
	}
	if len(unique) == 0 {
		return 0, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(unique)), ",")
	args := make([]interface{}, 0, len(unique)+4)
	args = append(args, MaintenanceJobPending, now.Unix(), normalizeTenant(tenantID), MaintenanceJobBlockedProvider)
	for _, routeID := range unique {
		args = append(args, routeID)
	}
	query := fmt.Sprintf(`UPDATE maintenance_jobs SET status = ?, attempts = 0,
		next_retry_at = 0, blocked_route_id = '', last_error = '', updated_at = ?
		WHERE tenant_id = ? AND status = ? AND TRIM(COALESCE(blocked_route_id, '')) != ''
		AND blocked_route_id NOT LIKE 'policy:%%'
		AND blocked_route_id NOT LIKE 'network:%%'
		AND blocked_route_id NOT IN (%s)`, placeholders)
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// RequeueBlockedJobsForHealthyProviderRoutes releases one maintenance class
// when its configured chain contains a route that is not currently open. A
// job may have been blocked by the primary route while a later fallback is
// healthy; keeping it tied to the primary circuit would strand durable work
// even though the configured chain can execute it. Analyzer versions keep
// post-run and skill-review jobs isolated from each other.
func (s *Store) RequeueBlockedJobsForHealthyProviderRoutes(ctx context.Context, tenantID string, analyzerVersion int, routeIDs []string, now time.Time) (int, error) {
	return s.requeueBlockedJobsForHealthyProviderRoutes(ctx, tenantID, tenantID, analyzerVersion, routeIDs, now)
}

// RequeueBlockedJobsForHealthyProviderRoutesAcrossTenants releases durable
// jobs owned by any person when the daemon-wide maintenance chain has a
// healthy route. Provider circuits currently belong to the configured daemon
// chain (normally tenant "default"), while jobs retain their person tenant for
// isolation and replay. Keeping those two scopes separate prevents a healthy
// fallback from leaving person-owned jobs stranded behind the primary route.
func (s *Store) RequeueBlockedJobsForHealthyProviderRoutesAcrossTenants(ctx context.Context, healthTenantID string, analyzerVersion int, routeIDs []string, now time.Time) (int, error) {
	return s.requeueBlockedJobsForHealthyProviderRoutes(ctx, healthTenantID, "", analyzerVersion, routeIDs, now)
}

func (s *Store) requeueBlockedJobsForHealthyProviderRoutes(ctx context.Context, healthTenantID, jobTenantID string, analyzerVersion int, routeIDs []string, now time.Time) (int, error) {
	if s == nil || s.db == nil || analyzerVersion <= 0 {
		return 0, nil
	}
	unique := make([]string, 0, len(routeIDs))
	seen := make(map[string]struct{}, len(routeIDs))
	healthy := false
	for _, routeID := range routeIDs {
		routeID = strings.TrimSpace(routeID)
		if routeID == "" {
			continue
		}
		if _, exists := seen[routeID]; exists {
			continue
		}
		seen[routeID] = struct{}{}
		unique = append(unique, routeID)
		health, err := s.GetProviderRouteHealth(ctx, healthTenantID, routeID)
		if err != nil {
			return 0, err
		}
		// A route with no failure record is healthy by default.
		if health == nil || health.State != ProviderRouteOpen {
			healthy = true
		}
	}
	if !healthy || len(unique) == 0 {
		return 0, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(unique)), ",")
	args := make([]interface{}, 0, len(unique)+5)
	args = append(args, MaintenanceJobPending, now.Unix(), analyzerVersion, MaintenanceJobBlockedProvider)
	tenantPredicate := ""
	if strings.TrimSpace(jobTenantID) != "" {
		tenantPredicate = " AND tenant_id = ?"
		args = append(args, normalizeTenant(jobTenantID))
	}
	for _, routeID := range unique {
		args = append(args, routeID)
	}
	query := fmt.Sprintf(`UPDATE maintenance_jobs SET status = ?, attempts = 0,
		next_retry_at = 0, blocked_route_id = '', last_error = '', updated_at = ?
		WHERE analyzer_version = ? AND status = ?%s
		AND blocked_route_id IN (%s)`, tenantPredicate, placeholders)
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func (s *Store) GetProviderRouteHealth(ctx context.Context, tenantID, routeID string) (*ProviderRouteHealth, error) {
	row := s.db.QueryRowContext(ctx, `SELECT tenant_id, route_id, provider, model, state, failure_class,
		consecutive_failures, opened_at, next_probe_at, probe_lease_until, last_error,
		last_request_id, updated_at FROM provider_route_health WHERE tenant_id = ? AND route_id = ?`,
		normalizeTenant(tenantID), routeID)
	var health ProviderRouteHealth
	var opened, next, lease, updated int64
	if err := row.Scan(&health.TenantID, &health.RouteID, &health.Provider, &health.Model, &health.State,
		&health.FailureClass, &health.ConsecutiveFailure, &opened, &next, &lease,
		&health.LastError, &health.LastRequestID, &updated); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	health.OpenedAt = unixTime(opened)
	health.NextProbeAt = unixTime(next)
	health.ProbeLeaseUntil = unixTime(lease)
	health.UpdatedAt = unixTime(updated)
	return &health, nil
}

func boundedText(value string, limit int) string {
	if limit > 0 && len(value) > limit {
		return value[:limit]
	}
	return value
}

func unixTime(value int64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	return time.Unix(value, 0)
}
