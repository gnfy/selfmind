package cliapp

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/kernel/memory"
)

func (a *App) runMaintenanceResetWorkHistory(args []string) int {
	fs := flag.NewFlagSet("selfmind maintenance reset-work-history", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	apply := fs.Bool("apply", false, "back up control.db and remove work history")
	dataDir := fs.String("data-dir", "", "control data directory (default: configured data dir)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(a.stderr, "reset-work-history: unexpected positional arguments")
		return 2
	}
	dir := strings.TrimSpace(*dataDir)
	if dir == "" {
		dir = a.gatewayDataDir()
	}
	if *apply && samePath(dir, a.gatewayDataDir()) && a.gatewayAppearsRunning() {
		fmt.Fprintln(a.stderr, "reset-work-history: the gateway appears to be running; run `selfmind gateway stop` first")
		return 1
	}
	store, err := control.OpenStore(dir)
	if err != nil {
		fmt.Fprintf(a.stderr, "reset-work-history: cannot open control store: %v\n", err)
		return 1
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()
	preview, err := store.PreviewWorkHistoryReset(ctx, a.tenantID())
	if err != nil {
		fmt.Fprintf(a.stderr, "reset-work-history: %v\n", err)
		return 1
	}
	fmt.Fprintf(a.stdout, "Work-history reset: %d thread(s), %d run(s), %d queue row(s).\n",
		preview.Threads, preview.Runs, preview.QueueRows)
	if preview.HasLiveWork() {
		fmt.Fprintf(a.stdout, "Live work: %d running run(s), %d watcher(s), %d started queue row(s).\n",
			preview.LiveRuns, preview.LiveWatchers, preview.StartedQueues)
	}
	if !*apply {
		fmt.Fprintln(a.stdout, "Dry run only. Identity, accounts, workspaces, settings, memory preferences, grants, provider state, and published Skill packages would be preserved.")
		fmt.Fprintln(a.stdout, "In-flight Skill learning evidence (workflow observations and profiles, skill candidate refs, attributions, run skill activations, task skill bindings) and memory sessions keyed to removed work would be removed.")
		if preview.HasLiveWork() {
			fmt.Fprintln(a.stdout, "Stop or settle live work before applying the reset.")
		} else {
			fmt.Fprintln(a.stdout, "Stop the gateway, then re-run with --apply to create a backup and reset this history.")
		}
		return 0
	}
	if preview.HasLiveWork() {
		fmt.Fprintln(a.stderr, "reset-work-history: refusing while live work exists")
		return 1
	}
	backup, err := store.BackupWorkHistorySnapshot(ctx, dir)
	if err != nil {
		fmt.Fprintf(a.stderr, "reset-work-history: create backup: %v\n", err)
		return 1
	}
	removed, err := store.ResetWorkHistory(ctx, a.tenantID())
	if err != nil {
		if errors.Is(err, control.ErrLiveWorkPreventsReset) {
			fmt.Fprintf(a.stderr, "reset-work-history: live work appeared; nothing was removed. Backup preserved at %s.\n", backup)
		} else {
			fmt.Fprintf(a.stderr, "reset-work-history: %v. Backup preserved at %s.\n", err, backup)
		}
		return 1
	}
	fmt.Fprintf(a.stdout, "Backup: %s\n", backup)
	fmt.Fprintf(a.stdout, "Removed %d thread(s), %d run(s), and %d queue row(s).\n",
		removed.Threads, removed.Runs, removed.QueueRows)
	purge, err := purgeMemoryWorkHistoryReferences(ctx, dir)
	if err != nil {
		fmt.Fprintf(a.stderr, "reset-work-history: control history was reset, but memory cleanup failed: %v. Backup preserved at %s.\n", err, backup)
		return 1
	}
	fmt.Fprintf(a.stdout, "Memory: removed %d task session(s) and cleared %d run reference(s) in %d partition(s); preferences were kept.\n",
		purge.Sessions, purge.Provenance, purge.Partitions)
	fmt.Fprintln(a.stdout, "Identity, accounts, workspaces, settings, memory preferences, grants, provider state, and published Skill packages were preserved; in-flight Skill learning evidence was removed.")
	return 0
}

// memoryWorkHistoryPurge aggregates the memory cleanup that follows a control
// reset across every on-disk memory partition.
type memoryWorkHistoryPurge struct {
	Partitions int
	Sessions   int
	Provenance int
}

// purgeMemoryWorkHistoryReferences removes `task:<id>` sessions and clears run
// provenance in every memory partition under dataDir: the legacy default
// partition plus each person partition that already has a memory.db. Memory
// lives beside control.db in the same configured data directory, so no path
// is guessed, and no partition is created merely to be purged.
func purgeMemoryWorkHistoryReferences(ctx context.Context, dataDir string) (memoryWorkHistoryPurge, error) {
	var out memoryWorkHistoryPurge
	partitions := make([]string, 0)
	for _, partition := range listMemoryAuditPartitions(dataDir) {
		if _, err := os.Stat(filepath.Join(dataDir, partition, "memory.db")); err != nil {
			continue
		}
		partitions = append(partitions, partition)
	}
	if len(partitions) == 0 {
		return out, nil
	}
	provider, err := memory.NewSQLiteProvider(dataDir)
	if err != nil {
		return out, err
	}
	defer provider.Close()
	for _, partition := range partitions {
		sessions, provenance, err := provider.PurgeWorkHistoryReferences(ctx, partition)
		if err != nil {
			return out, fmt.Errorf("partition %s: %w", partition, err)
		}
		out.Partitions++
		out.Sessions += sessions
		out.Provenance += provenance
	}
	return out, nil
}
