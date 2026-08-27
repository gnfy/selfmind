package gateway

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"
)

func TestWaitForOwnerReleaseUsesRuntimeLock(t *testing.T) {
	dataDir := t.TempDir()
	manager := NewManager(dataDir, "127.0.0.1:9999")
	if err := manager.Acquire(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	err := WaitForOwnerRelease(ctx, dataDir)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("owned runtime wait error = %v, want deadline", err)
	}
	if err := manager.Release(); err != nil {
		t.Fatal(err)
	}
	if err := WaitForOwnerRelease(context.Background(), dataDir); err != nil {
		t.Fatalf("released runtime remained owned: %v", err)
	}
}

func TestWaitForRuntimeAbsenceRequiresProcessLockAndListenerRelease(t *testing.T) {
	dataDir := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(dataDir, listener.Addr().String())
	if err := manager.Acquire(); err != nil {
		t.Fatal(err)
	}
	if err := manager.WriteStatus("running", "default", ""); err != nil {
		t.Fatal(err)
	}
	assertBlocked := func(stage string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		err := WaitForRuntimeAbsence(ctx, dataDir, listener.Addr().String(), os.Getpid())
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("%s absence error = %v, want deadline", stage, err)
		}
	}
	assertBlocked("process, lock, and listener present")
	if err := manager.Release(); err != nil {
		t.Fatal(err)
	}
	assertBlocked("process receipt and listener present")
	if err := os.Remove(manager.Paths.PIDPath); err != nil {
		t.Fatal(err)
	}
	assertBlocked("listener present")
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	assertBlocked("captured process remains alive after receipt and listener release")
	if err := WaitForRuntimeAbsence(context.Background(), dataDir, listener.Addr().String(), 0); err != nil {
		t.Fatalf("released runtime remained present: %v", err)
	}
}

func TestResolveGatewayEnvPrecedence(t *testing.T) {
	t.Setenv("SELF_DAEMON_ADDR", "127.0.0.1:1111")
	t.Setenv("SELF_GATEWAY_ADDR", "127.0.0.1:2222")
	if got := ResolveAddr(""); got != "127.0.0.1:2222" {
		t.Fatalf("addr = %q", got)
	}

	t.Setenv("SELF_DAEMON_URL", "http://127.0.0.1:1111")
	t.Setenv("SELF_GATEWAY_URL", "http://127.0.0.1:2222/")
	if got := ResolveURL(""); got != "http://127.0.0.1:2222" {
		t.Fatalf("url = %q", got)
	}

	t.Setenv("SELF_DAEMON_TOKEN", "old")
	t.Setenv("SELF_GATEWAY_TOKEN", "new")
	if got := ResolveToken(); got != "new" {
		t.Fatalf("token = %q", got)
	}
}

func TestResolveDrainTimeout(t *testing.T) {
	t.Setenv("SELF_GATEWAY_DRAIN_TIMEOUT", "2s")
	if got := ResolveDrainTimeout(); got != 2*time.Second {
		t.Fatalf("duration = %s", got)
	}
	t.Setenv("SELF_GATEWAY_DRAIN_TIMEOUT", "3")
	if got := ResolveDrainTimeout(); got != 3*time.Second {
		t.Fatalf("duration = %s", got)
	}
}

func TestManagerLockAndRunningRecord(t *testing.T) {
	manager := NewManager(t.TempDir(), "127.0.0.1:9999")
	if err := manager.Acquire(); err != nil {
		t.Fatal(err)
	}
	defer manager.Release()
	if err := manager.WriteStatus("running", "default", ""); err != nil {
		t.Fatal(err)
	}
	if rec, ok := manager.RunningRecord(); !ok {
		t.Fatal("expected running record")
	} else if rec.PID != os.Getpid() || rec.State != "running" {
		t.Fatalf("record = %+v", rec)
	}

	second := NewManager(manager.Paths.DataDir, manager.Addr)
	if err := second.Acquire(); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second acquire err = %v", err)
	}
}

func TestAcquireTrustsRuntimeLockOverRecycledPIDRecord(t *testing.T) {
	manager := NewManager(t.TempDir(), "127.0.0.1:9999")
	legacy := StatusRecord{
		PID: os.Getpid(), Kind: gatewayKind, State: "running",
		StartedAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano),
	}
	if err := writeJSONFile(manager.Paths.PIDPath, legacy); err != nil {
		t.Fatal(err)
	}
	// The PID is alive because it is this test process, but nobody owns the
	// gateway flock. This is the exact PID-reuse shape that used to reject boot.
	if err := manager.Acquire(); err != nil {
		t.Fatalf("stale live PID metadata must not override an available runtime lock: %v", err)
	}
	defer manager.Release()
}

func TestRuntimeOwnerRecordUsesLockAsAuthority(t *testing.T) {
	owner := NewManager(t.TempDir(), "127.0.0.1:9999")
	if err := owner.Acquire(); err != nil {
		t.Fatal(err)
	}
	defer owner.Release()
	if err := owner.WriteStatus("running", "default", ""); err != nil {
		t.Fatal(err)
	}
	probe := NewManager(owner.Paths.DataDir, owner.Addr)
	rec, owned, err := probe.RuntimeOwnerRecord()
	if err != nil || !owned || rec.PID != os.Getpid() {
		t.Fatalf("owner probe = %+v owned=%v err=%v", rec, owned, err)
	}
}

func TestLegacyStatusRecordHasStableInstanceID(t *testing.T) {
	rec := StatusRecord{PID: 42, StartedAt: "2026-08-05T01:02:03Z", Addr: "127.0.0.1:7777", DataDir: "/tmp/selfmind"}
	first := rec.StableInstanceID()
	if first == "" || first != rec.StableInstanceID() || first[:15] != "gateway_legacy_" {
		t.Fatalf("legacy id is not stable: %q", first)
	}
	rec.InstanceID = "gateway_explicit"
	if got := rec.StableInstanceID(); got != rec.InstanceID {
		t.Fatalf("explicit instance id changed: %q", got)
	}
}

func TestManagerCleansStalePID(t *testing.T) {
	manager := NewManager(t.TempDir(), "127.0.0.1:9999")
	if err := writeJSONFile(manager.Paths.PIDPath, StatusRecord{
		PID:   99999999,
		Kind:  gatewayKind,
		Addr:  manager.Addr,
		State: "running",
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := manager.RunningRecord(); ok {
		t.Fatal("expected stale record to be ignored")
	}
	if _, err := os.Stat(manager.Paths.PIDPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pid file should be removed, err = %v", err)
	}
	state, err := ReadStatusRecord(manager.Paths.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if state.State != "crashed" || state.ExitReason == "" {
		t.Fatalf("stale state was not reconciled: %+v", state)
	}
}

func TestManagerCrashPreservesReasonAndReleasesLease(t *testing.T) {
	manager := NewManager(t.TempDir(), "127.0.0.1:9999")
	if err := manager.Acquire(); err != nil {
		t.Fatal(err)
	}
	if err := manager.WriteStatus("starting", "default", ""); err != nil {
		t.Fatal(err)
	}
	manager.Crash("bind failed")

	rec, err := ReadStatusRecord(manager.Paths.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != "crashed" || rec.ExitReason != "bind failed" {
		t.Fatalf("crash record = %+v", rec)
	}
	if _, err := os.Stat(manager.Paths.PIDPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("live lease survived crash: %v", err)
	}
	probe := NewManager(manager.Paths.DataDir, manager.Addr)
	if err := probe.Acquire(); err != nil {
		t.Fatalf("crash did not release owner lock: %v", err)
	}
	_ = probe.Release()
}

func TestReconcilePreviousStateUsesLatestLeaseHeartbeat(t *testing.T) {
	manager := NewManager(t.TempDir(), "127.0.0.1:9999")
	previous := StatusRecord{
		PID: 12345, Kind: gatewayKind, InstanceID: "gateway_previous", State: "running",
		StartedAt: "2026-08-01T00:00:00Z", HeartbeatAt: "2026-08-01T00:00:01Z",
	}
	lease := previous
	lease.HeartbeatAt = "2026-08-01T00:15:00Z"
	lease.UpdatedAt = lease.HeartbeatAt
	if err := writeJSONFile(manager.Paths.StatePath, previous); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONFile(manager.Paths.PIDPath, lease); err != nil {
		t.Fatal(err)
	}
	if err := manager.Acquire(); err != nil {
		t.Fatal(err)
	}
	defer manager.Release()
	reconciled, ok := manager.ReconcilePreviousState()
	if !ok || reconciled.HeartbeatAt != lease.HeartbeatAt {
		t.Fatalf("reconciled = %+v, ok=%v", reconciled, ok)
	}
}

func TestHeartbeatOnlyRefreshesLiveLease(t *testing.T) {
	manager := NewManager(t.TempDir(), "127.0.0.1:9999")
	if err := manager.WriteStatus("running", "default", ""); err != nil {
		t.Fatal(err)
	}
	stateBefore, err := os.ReadFile(manager.Paths.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := manager.Heartbeat(); err != nil {
		t.Fatal(err)
	}
	stateAfter, err := os.ReadFile(manager.Paths.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(stateAfter) != string(stateBefore) {
		t.Fatal("heartbeat rewrote lifecycle state")
	}
	lease, err := ReadStatusRecord(manager.Paths.PIDPath)
	if err != nil {
		t.Fatal(err)
	}
	if lease.InstanceID != manager.InstanceID || lease.HeartbeatAt == "" {
		t.Fatalf("lease = %+v", lease)
	}
}

func TestReconcilePreviousActiveStateAfterLock(t *testing.T) {
	manager := NewManager(t.TempDir(), "127.0.0.1:9999")
	previous := StatusRecord{
		PID: 12345, Kind: gatewayKind, InstanceID: "gateway_previous",
		State: "running", StartedAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	}
	if err := writeJSONFile(manager.Paths.StatePath, previous); err != nil {
		t.Fatal(err)
	}
	if err := manager.Acquire(); err != nil {
		t.Fatal(err)
	}
	defer manager.Release()
	reconciled, ok := manager.ReconcilePreviousState()
	if !ok || reconciled.State != "crashed" || reconciled.InstanceID != previous.InstanceID {
		t.Fatalf("reconciled = %+v, ok=%v", reconciled, ok)
	}
}

func TestStatusRecordHeartbeatStaleness(t *testing.T) {
	now := time.Now().UTC()
	rec := StatusRecord{State: "running", HeartbeatAt: now.Add(-time.Minute).Format(time.RFC3339Nano)}
	if !rec.HeartbeatStale(now) {
		t.Fatal("expected stale heartbeat")
	}
	rec.HeartbeatAt = now.Add(-time.Second).Format(time.RFC3339Nano)
	if rec.HeartbeatStale(now) {
		t.Fatal("fresh heartbeat reported stale")
	}
}
