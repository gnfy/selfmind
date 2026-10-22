package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// At the daemon boundary, a SELFMIND_WORKERS value that cannot serve the
// configured capacity, or that is not a worker count at all, stops startup
// with an error naming the settings, before any store or agent worker exists.
func TestRunRefusesInvalidWorkerCountBeforeOpeningStores(t *testing.T) {
	for _, tc := range []struct {
		name     string
		workers  string
		gateway  string
		mentions []string
	}{
		{"explicit one below the default capacity", "1", "",
			[]string{"SELFMIND_WORKERS=1", "gateway.max_active_work_runs=2"}},
		{"explicit two below a configured capacity of three", "2", "  max_active_work_runs: 3\n",
			[]string{"SELFMIND_WORKERS=2", "gateway.max_active_work_runs=3"}},
		{"a value that is not a count", "zero", "",
			[]string{`SELFMIND_WORKERS="zero"`}},
		{"zero workers", "0", "  max_active_work_runs: 1\n",
			[]string{`SELFMIND_WORKERS="0"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateDaemonEnvironment(t)
			dataDir := filepath.Join(t.TempDir(), "data")
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			content := "storage:\n  data_dir: " + strconv.Quote(dataDir) + "\ngateway:\n  addr: \"127.0.0.1:0\"\n" + tc.gateway
			if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("SELFMIND_WORKERS", tc.workers)

			err := Run(context.Background(), Options{ConfigPath: configPath})
			if err == nil {
				t.Fatal("daemon started with an unusable worker count")
			}
			for _, want := range tc.mentions {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("startup error %q does not name %q", err, want)
				}
			}
			// Only the daemon's lock and state directory may exist: no control,
			// memory or cron store was opened.
			entries, readErr := os.ReadDir(dataDir)
			if readErr != nil {
				t.Fatal(readErr)
			}
			for _, entry := range entries {
				if entry.Name() != "gateway" {
					t.Fatalf("%s was created before the worker count was validated", entry.Name())
				}
			}
			state, readErr := os.ReadFile(filepath.Join(dataDir, "gateway", "gateway_state.json"))
			if readErr != nil || !strings.Contains(string(state), "SELFMIND_WORKERS") {
				t.Fatalf("the refused start did not record its reason: %s, %v", state, readErr)
			}
		})
	}
}

// isolateDaemonEnvironment keeps operator settings out of a test daemon start.
// Empty SELF_* values are ignored by the config loader, and t.Setenv restores
// whatever Run's environment bridging writes.
func isolateDaemonEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	for _, key := range []string{
		"SELF_STORAGE_DATA_DIR", "SELF_GATEWAY_MAX_ACTIVE_WORK_RUNS",
		"SELF_GATEWAY_TOKEN", "SELF_DAEMON_TOKEN", "SELF_GATEWAY_ADDR", "SELF_DAEMON_ADDR",
		"SELF_GATEWAY_URL", "SELF_GATEWAY_DRAIN_TIMEOUT",
		"SELF_FEISHU_ENCRYPT_KEY", "SELF_FEISHU_VERIFICATION_TOKEN", "SELF_TENANT_ID",
	} {
		t.Setenv(key, "")
	}
}
