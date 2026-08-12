package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"selfmind/internal/buildinfo"
)

const gatewayKind = "selfmind-gateway"

const gatewayHeartbeatStaleAfter = 45 * time.Second

var ErrAlreadyRunning = errors.New("selfmind gateway is already running")

type StatusRecord struct {
	PID             int      `json:"pid"`
	Kind            string   `json:"kind"`
	InstanceID      string   `json:"instance_id,omitempty"`
	Version         string   `json:"version,omitempty"`
	Addr            string   `json:"addr"`
	DataDir         string   `json:"data_dir"`
	DefaultTenantID string   `json:"default_tenant_id,omitempty"`
	Argv            []string `json:"argv,omitempty"`
	State           string   `json:"state"`
	StartedAt       string   `json:"started_at"`
	UpdatedAt       string   `json:"updated_at"`
	HeartbeatAt     string   `json:"heartbeat_at,omitempty"`
	ExitReason      string   `json:"exit_reason,omitempty"`
	HostBootID      string   `json:"host_boot_id,omitempty"`
}

func (r StatusRecord) HeartbeatStale(now time.Time) bool {
	if r.State != "running" || strings.TrimSpace(r.HeartbeatAt) == "" {
		return false
	}
	heartbeatAt, err := time.Parse(time.RFC3339Nano, r.HeartbeatAt)
	if err != nil {
		heartbeatAt, err = time.Parse(time.RFC3339, r.HeartbeatAt)
	}
	return err == nil && now.Sub(heartbeatAt) > gatewayHeartbeatStaleAfter
}

type AlreadyRunningError struct {
	Record StatusRecord
}

func (e AlreadyRunningError) Error() string {
	if e.Record.PID > 0 {
		return fmt.Sprintf("%s (pid %d)", ErrAlreadyRunning, e.Record.PID)
	}
	return ErrAlreadyRunning.Error()
}

func (e AlreadyRunningError) Unwrap() error {
	return ErrAlreadyRunning
}

type Manager struct {
	Paths      Paths
	Addr       string
	Started    time.Time
	InstanceID string
	lock       *runtimeFileLock
	mu         sync.Mutex
	state      string
	tenantID   string
	exitReason string
	hostBootID string
}

func NewManager(dataDir, addr string) *Manager {
	return &Manager{
		Paths:      ResolvePaths(dataDir),
		Addr:       ResolveAddr(addr),
		Started:    time.Now(),
		InstanceID: "gateway_" + uuid.NewString(),
		hostBootID: hostBootID(),
	}
}

func (m *Manager) Acquire() error {
	if err := os.MkdirAll(m.Paths.RuntimeDir, 0755); err != nil {
		return fmt.Errorf("create gateway runtime dir: %w", err)
	}
	lock, err := acquireRuntimeLock(m.Paths.LockPath)
	if err != nil {
		rec, _ := ReadStatusRecord(m.Paths.PIDPath)
		return AlreadyRunningError{Record: rec}
	}
	m.lock = lock
	return nil
}

// RuntimeOwnerRecord probes the process-ownership lock without trusting a PID
// record first. PID files are health metadata: a recycled PID can look alive
// after an unclean exit, while the flock is the authoritative single-owner
// boundary. The probe is deliberately side-effect free apart from creating the
// lock file itself.
func (m *Manager) RuntimeOwnerRecord() (StatusRecord, bool, error) {
	if err := os.MkdirAll(m.Paths.RuntimeDir, 0755); err != nil {
		return StatusRecord{}, false, fmt.Errorf("create gateway runtime dir: %w", err)
	}
	lock, err := acquireRuntimeLock(m.Paths.LockPath)
	if err == nil {
		if releaseErr := lock.Release(); releaseErr != nil {
			return StatusRecord{}, false, releaseErr
		}
		return StatusRecord{}, false, nil
	}
	rec, _ := ReadStatusRecord(m.Paths.PIDPath)
	return rec, true, nil
}

func (m *Manager) Release() error {
	if m.lock == nil {
		return nil
	}
	err := m.lock.Release()
	m.lock = nil
	return err
}

func (m *Manager) Cleanup(exitReason string) {
	_ = m.WriteStatus("stopped", "", exitReason)
	_ = os.Remove(m.Paths.PIDPath)
	_ = m.Release()
}

// Crash preserves an actionable terminal lifecycle record while releasing the
// single-owner lock. It is used for startup failures and top-level panics; a
// clean signal/drain path continues to use Cleanup.
func (m *Manager) Crash(exitReason string) {
	if strings.TrimSpace(exitReason) == "" {
		exitReason = "gateway exited unexpectedly"
	}
	_ = m.WriteStatus("crashed", "", exitReason)
	_ = os.Remove(m.Paths.PIDPath)
	_ = m.Release()
}

func (m *Manager) WriteStatus(state, defaultTenantID, exitReason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state = state
	m.tenantID = defaultTenantID
	m.exitReason = exitReason
	rec := m.statusRecordLocked(time.Now())
	if err := writeJSONFile(m.Paths.StatePath, rec); err != nil {
		return err
	}
	if state != "stopped" && state != "crashed" {
		return writeJSONFile(m.Paths.PIDPath, rec)
	}
	return nil
}

// Heartbeat refreshes only the live PID lease. Lifecycle transitions remain
// in gateway-state.json, so a 15-second lease does not masquerade as a stream
// of state changes and an unclean exit remains inspectable after the PID file
// is reconciled.
func (m *Manager) Heartbeat() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == "" || m.state == "stopped" || m.state == "crashed" {
		return nil
	}
	return writeJSONFile(m.Paths.PIDPath, m.statusRecordLocked(time.Now()))
}

func (m *Manager) Snapshot() StatusRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.statusRecordLocked(time.Now())
}

func (m *Manager) statusRecordLocked(now time.Time) StatusRecord {
	now = now.UTC()
	return StatusRecord{
		PID:             os.Getpid(),
		Kind:            gatewayKind,
		InstanceID:      m.InstanceID,
		Version:         buildinfo.Version,
		Addr:            m.Addr,
		DataDir:         m.Paths.DataDir,
		DefaultTenantID: m.tenantID,
		Argv:            os.Args,
		State:           m.state,
		StartedAt:       m.Started.UTC().Format(time.RFC3339Nano),
		UpdatedAt:       now.Format(time.RFC3339Nano),
		HeartbeatAt:     now.Format(time.RFC3339Nano),
		ExitReason:      m.exitReason,
		HostBootID:      m.hostBootID,
	}
}

func (m *Manager) RunningRecord() (StatusRecord, bool) {
	rec, err := ReadStatusRecord(m.Paths.PIDPath)
	if err != nil {
		return StatusRecord{}, false
	}
	if rec.Kind != gatewayKind || rec.PID <= 0 {
		return StatusRecord{}, false
	}
	if !processAlive(rec.PID) {
		m.reconcileCrashedRecord(rec, "process disappeared without a clean shutdown")
		_ = os.Remove(m.Paths.PIDPath)
		return StatusRecord{}, false
	}
	return rec, true
}

// StableInstanceID preserves idempotent lifecycle events for status files
// written before InstanceID existed. It intentionally derives only from stable,
// non-secret process metadata, so replaying the same legacy crash yields the
// same event key instead of silently dropping the event.
func (r StatusRecord) StableInstanceID() string {
	if id := strings.TrimSpace(r.InstanceID); id != "" {
		return id
	}
	seed := fmt.Sprintf("%d\x00%s\x00%s\x00%s", r.PID, r.StartedAt, r.Addr, r.DataDir)
	sum := sha256.Sum256([]byte(seed))
	return "gateway_legacy_" + hex.EncodeToString(sum[:8])
}

// ReconcilePreviousState runs only after this process owns gateway.lock. Any
// previous active lifecycle record is therefore an orphan even if its PID was
// recycled by the OS. The returned record can be copied into control.db once
// that store is open.
func (m *Manager) ReconcilePreviousState() (StatusRecord, bool) {
	rec, err := ReadStatusRecord(m.Paths.StatePath)
	if err != nil || rec.InstanceID == m.InstanceID {
		return StatusRecord{}, false
	}
	if rec.State == "crashed" {
		return rec, true
	}
	if !activeGatewayState(rec.State) {
		return StatusRecord{}, false
	}
	// The lifecycle record changes only on state transitions. Merge the live
	// lease before diagnosing an orphan so last_heartbeat reports when the
	// process actually disappeared rather than when it first started.
	if lease, leaseErr := ReadStatusRecord(m.Paths.PIDPath); leaseErr == nil && lease.InstanceID == rec.InstanceID {
		rec.PID = lease.PID
		rec.HeartbeatAt = lease.HeartbeatAt
		rec.UpdatedAt = lease.UpdatedAt
		rec.Argv = lease.Argv
		if lease.HostBootID != "" {
			rec.HostBootID = lease.HostBootID
		}
	}
	reason := "previous gateway instance did not shut down cleanly"
	if rec.HostBootID != "" && m.hostBootID != "" && rec.HostBootID != m.hostBootID {
		reason = "host or WSL session restarted before gateway shutdown"
	}
	m.reconcileCrashedRecord(rec, reason)
	_ = os.Remove(m.Paths.PIDPath)
	rec.State = "crashed"
	rec.ExitReason = reason
	rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return rec, true
}

func (m *Manager) reconcileCrashedRecord(rec StatusRecord, reason string) {
	rec.State = "crashed"
	rec.ExitReason = reason
	rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	_ = writeJSONFile(m.Paths.StatePath, rec)
}

func activeGatewayState(state string) bool {
	switch state {
	case "starting", "running", "draining":
		return true
	default:
		return false
	}
}

func ReadStatusRecord(path string) (StatusRecord, error) {
	var rec StatusRecord
	data, err := os.ReadFile(path)
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return rec, err
	}
	return rec, nil
}

func writeJSONFile(path string, payload interface{}) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	_ = os.Remove(path)
	return os.Rename(tmp, path)
}
