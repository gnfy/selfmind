package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const gatewayKind = "selfmind-gateway"

var ErrAlreadyRunning = errors.New("selfmind gateway is already running")

type StatusRecord struct {
	PID             int      `json:"pid"`
	Kind            string   `json:"kind"`
	Addr            string   `json:"addr"`
	DataDir         string   `json:"data_dir"`
	DefaultTenantID string   `json:"default_tenant_id,omitempty"`
	Argv            []string `json:"argv,omitempty"`
	State           string   `json:"state"`
	StartedAt       string   `json:"started_at"`
	UpdatedAt       string   `json:"updated_at"`
	ExitReason      string   `json:"exit_reason,omitempty"`
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
	Paths   Paths
	Addr    string
	Started time.Time
	lock    *runtimeFileLock
}

func NewManager(dataDir, addr string) *Manager {
	return &Manager{
		Paths:   ResolvePaths(dataDir),
		Addr:    ResolveAddr(addr),
		Started: time.Now(),
	}
}

func (m *Manager) Acquire() error {
	if err := os.MkdirAll(m.Paths.RuntimeDir, 0755); err != nil {
		return fmt.Errorf("create gateway runtime dir: %w", err)
	}
	if rec, ok := m.RunningRecord(); ok {
		return AlreadyRunningError{Record: rec}
	}
	lock, err := acquireRuntimeLock(m.Paths.LockPath)
	if err != nil {
		if rec, ok := m.RunningRecord(); ok {
			return AlreadyRunningError{Record: rec}
		}
		return fmt.Errorf("acquire gateway runtime lock: %w", err)
	}
	m.lock = lock
	if rec, ok := m.RunningRecord(); ok {
		_ = m.Release()
		return AlreadyRunningError{Record: rec}
	}
	return nil
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

func (m *Manager) WriteStatus(state, defaultTenantID, exitReason string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	started := m.Started.UTC().Format(time.RFC3339)
	rec := StatusRecord{
		PID:             os.Getpid(),
		Kind:            gatewayKind,
		Addr:            m.Addr,
		DataDir:         m.Paths.DataDir,
		DefaultTenantID: defaultTenantID,
		Argv:            os.Args,
		State:           state,
		StartedAt:       started,
		UpdatedAt:       now,
		ExitReason:      exitReason,
	}
	if err := writeJSONFile(m.Paths.StatePath, rec); err != nil {
		return err
	}
	if state != "stopped" {
		return writeJSONFile(m.Paths.PIDPath, rec)
	}
	return nil
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
		_ = os.Remove(m.Paths.PIDPath)
		return StatusRecord{}, false
	}
	return rec, true
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
