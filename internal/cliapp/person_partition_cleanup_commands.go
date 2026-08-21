package cliapp

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"time"

	appcore "selfmind/internal/app"
	"selfmind/internal/control"
	"selfmind/internal/platform/config"
	"selfmind/internal/tools"
)

func (a *App) runMaintenanceCleanupPersonPartitions(args []string) int {
	fs := flag.NewFlagSet("selfmind maintenance cleanup-person-partitions", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	apply := fs.Bool("apply", false, "move orphan partitions into recoverable quarantine; default is dry-run")
	root := fs.String("root", "", "Skill asset root containing person_* partitions; a non-configured root also requires --data-dir")
	dataDirOverride := fs.String("data-dir", "", "explicit control-store data dir paired with a --root override")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.LoadConfig(config.Options{Path: a.configPath})
	if err != nil {
		fmt.Fprintf(a.stderr, "maintenance cleanup-person-partitions: load config: %v\n", err)
		return 1
	}
	storage, err := appcore.ResolveSkillStorage(cfg)
	if err != nil {
		fmt.Fprintf(a.stderr, "maintenance cleanup-person-partitions: resolve skill storage: %v\n", err)
		return 1
	}
	configuredRoot := storage.BaseDir()
	if *root == "" {
		*root = storage.BaseDir()
	}
	dataDir := appcore.ResolveDataDir(cfg)
	if *dataDirOverride != "" {
		dataDir, err = filepath.Abs(filepath.Clean(*dataDirOverride))
		if err != nil {
			fmt.Fprintf(a.stderr, "maintenance cleanup-person-partitions: resolve --data-dir: %v\n", err)
			return 1
		}
	}
	rootAbs, rootErr := filepath.Abs(filepath.Clean(*root))
	configuredAbs, configuredErr := filepath.Abs(filepath.Clean(configuredRoot))
	if rootErr != nil || configuredErr != nil {
		fmt.Fprintln(a.stderr, "maintenance cleanup-person-partitions: cannot resolve cleanup root")
		return 1
	}
	if rootAbs != configuredAbs && *dataDirOverride == "" {
		fmt.Fprintln(a.stderr, "maintenance cleanup-person-partitions: --root differs from evolution.skills_dir; pass --data-dir explicitly to pair it with the authoritative control.db")
		return 1
	}
	*root = rootAbs
	controlDB := filepath.Join(dataDir, "control.db")
	if *apply && a.gatewayAppearsRunning() {
		fmt.Fprintln(a.stderr, "maintenance cleanup-person-partitions: stop the gateway before applying cleanup")
		fmt.Fprintln(a.stderr, "Run: selfmind gateway stop")
		return 1
	}
	store, err := control.OpenExistingStoreReadOnly(dataDir)
	if err != nil {
		fmt.Fprintf(a.stderr, "maintenance cleanup-person-partitions: cannot open control store: %v\n", err)
		return 1
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()
	known, err := store.ListPersonIDs(ctx)
	if err != nil {
		fmt.Fprintf(a.stderr, "maintenance cleanup-person-partitions: list known persons: %v\n", err)
		return 1
	}
	report, err := tools.CleanupOrphanPersonPartitions(*root, known, *apply)
	report.ControlDB = controlDB
	if err != nil {
		fmt.Fprintf(a.stderr, "maintenance cleanup-person-partitions: %v\n", err)
		return 1
	}
	fmt.Fprint(a.stdout, tools.FormatPersonPartitionCleanupReport(report))
	if report.Quarantined > 0 {
		fmt.Fprintln(a.stdout, "Nothing was deleted. Restore a partition by stopping the gateway and moving it back from the quarantine path.")
	}
	return 0
}
