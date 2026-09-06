package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"selfmind/internal/modelchange"
)

// TestBackgroundServicesStartWhenReadinessRecovers pins the transition that used
// to be unreachable. Maintenance and memory governance were started once, inline
// at startup: a daemon that came up with an unready background route logged a
// hint and never reconsidered, so they stayed parked for its whole lifetime even
// after the person repaired or cancelled the model change. Observed on a daemon
// whose foreground routes were verified while background_verified_at was still
// its zero value.
func TestBackgroundServicesStartWhenReadinessRecovers(t *testing.T) {
	daemon, _, _, _, _ := newApprovalTestServer(t)
	ctx := context.Background()

	unreadyConfig := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(unreadyConfig, []byte("models:\n  primary:\n    provider: none\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	daemon.ModelChanges = &modelchange.Service{ConfigPath: unreadyConfig}
	if daemon.backgroundReadyForWork() {
		t.Fatal("fixture error: the background route must be unready to start")
	}

	daemon.EnsureBackgroundServices(ctx)
	if daemon.background.started {
		t.Fatal("an unready background route must not start the workers")
	}

	// Readiness recovers — the person cancelled the pending change or repaired
	// the route. Nothing restarts the daemon, so this call is the only chance.
	daemon.ModelChanges = nil // a server without transaction state reads as ready
	if !daemon.backgroundReadyForWork() {
		t.Fatal("fixture error: expected readiness to have recovered")
	}
	daemon.EnsureBackgroundServices(ctx)
	if !daemon.background.started {
		t.Fatal("recovered readiness must start maintenance and memory governance")
	}

	// Idempotent: a second transition must not stack a second set of workers.
	before := len(daemon.background.stops)
	daemon.EnsureBackgroundServices(ctx)
	if len(daemon.background.stops) != before {
		t.Fatalf("a repeated readiness signal started another set of workers: %d → %d",
			before, len(daemon.background.stops))
	}

	daemon.StopBackgroundServices()
	if daemon.background.started || len(daemon.background.stops) != 0 {
		t.Fatal("shutdown must release what it started")
	}
}
