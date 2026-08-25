package cliapp

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/control"
)

func (a *App) runMaintenancePruneSkillCandidateRefs(args []string) int {
	fs := flag.NewFlagSet("selfmind maintenance prune-skill-candidate-refs", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	apply := fs.Bool("apply", false, "delete only terminal or orphan candidate refs")
	dataDir := fs.String("data-dir", "", "control data directory (default: configured gateway data dir)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	dir := strings.TrimSpace(*dataDir)
	if dir == "" {
		dir = a.gatewayDataDir()
	}
	store, err := control.OpenStore(dir)
	if err != nil {
		fmt.Fprintf(a.stderr, "maintenance prune-skill-candidate-refs: cannot open control store: %v\n", err)
		return 1
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()
	report, err := store.PruneSkillCandidateRefs(ctx, a.tenantID(), *apply)
	if err != nil {
		fmt.Fprintf(a.stderr, "maintenance prune-skill-candidate-refs: %v\n", err)
		return 1
	}
	mode := "DRY-RUN"
	if *apply {
		mode = "APPLIED"
	}
	fmt.Fprintf(a.stdout, "%s terminal=%d orphan=%d deleted=%d\n", mode, report.Terminal, report.Orphan, report.Deleted)
	for _, owner := range report.Owners {
		fmt.Fprintf(a.stdout, "- %s\n", owner)
	}
	if !*apply && report.Terminal+report.Orphan > 0 {
		fmt.Fprintln(a.stdout, "Re-run with --apply to delete exactly these terminal/orphan refs.")
	}
	if *apply {
		verify, verifyErr := store.PruneSkillCandidateRefs(ctx, a.tenantID(), false)
		if verifyErr != nil {
			fmt.Fprintf(a.stderr, "maintenance prune-skill-candidate-refs: post-repair verification failed: %v\n", verifyErr)
			return 1
		}
		fmt.Fprintf(a.stdout, "VERIFY terminal=%d orphan=%d; run `selfmind doctor --verbose` to verify the full presentation contract.\n", verify.Terminal, verify.Orphan)
		if verify.Terminal+verify.Orphan != 0 {
			return 1
		}
	}
	return 0
}
