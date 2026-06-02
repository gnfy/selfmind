package gateway

import (
	"errors"
	"os"
	"testing"
	"time"
)

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
}
