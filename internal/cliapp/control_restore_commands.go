package cliapp

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"selfmind/internal/control"
)

func (a *App) runMaintenanceRestoreControl(args []string) int {
	fs := flag.NewFlagSet("selfmind maintenance restore-control", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	backup := fs.String("backup", "", "migration backup under the data directory backups folder")
	dataDir := fs.String("data-dir", "", "control data directory (default: configured data dir)")
	yes := fs.Bool("yes", false, "confirm replacement of control.db while preserving the failed copy")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	dir := strings.TrimSpace(*dataDir)
	if dir == "" {
		dir = a.gatewayDataDir()
	}
	if strings.TrimSpace(*backup) == "" {
		fmt.Fprintln(a.stderr, "restore-control: --backup is required")
		return 2
	}
	if !*yes {
		fmt.Fprintf(a.stderr, "restore-control: refusing without --yes; stop the daemon first, then restore %s into %s\n", *backup, filepath.Join(dir, "control.db"))
		return 2
	}
	if samePath(dir, a.gatewayDataDir()) && a.gatewayAppearsRunning() {
		fmt.Fprintln(a.stderr, "restore-control: the gateway appears to be running; run `selfmind gateway stop` first")
		return 1
	}
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()
	failed, err := control.RestoreControlDatabase(ctx, dir, *backup)
	if err != nil {
		fmt.Fprintf(a.stderr, "restore-control: %v\n", err)
		return 1
	}
	fmt.Fprintf(a.stdout, "Restored control.db from %s.\n", *backup)
	if failed != "" {
		fmt.Fprintf(a.stdout, "Previous database preserved at %s.\n", failed)
	}
	fmt.Fprintln(a.stdout, "Start the gateway and verify `selfmind gateway status` before resuming work.")
	return 0
}

func samePath(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	return errA == nil && errB == nil && filepath.Clean(absA) == filepath.Clean(absB)
}
