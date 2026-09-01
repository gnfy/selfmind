package cliapp

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"selfmind/internal/kernel/memory"
)

// selfmind maintenance memory-audit [--archive-confirmed] [--partition P] [--data-dir DIR]
//
// Grades ACTIVE canonical memories with the two-tier transient classifier
// (memory.ClassifyTransientContent — the same rule the intake durability gate
// enforces going forward) and reports them. Only CONFIRMED transient
// instances may be archived, and only with --archive-confirmed; ambiguous
// candidates are reported for review and never auto-processed. Pinned and
// user-confirmed memories are never touched by any automatic path. Archiving
// is a reversible status change plus an audit event — never a physical
// delete. The acceptance bar: rather miss a transient candidate than wrongly
// archive one long-term rule.
func (a *App) runMaintenanceMemoryAudit(args []string) int {
	fs := flag.NewFlagSet("selfmind maintenance memory-audit", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	archiveConfirmed := fs.Bool("archive-confirmed", false, "archive CONFIRMED transient memories (reversible); candidates are always report-only")
	// --archive is the deprecated spelling; it never archived candidates
	// under the new semantics either.
	archiveLegacy := fs.Bool("archive", false, "deprecated alias of --archive-confirmed")
	partition := fs.String("partition", "", "single partition to audit (e.g. person_<id> or default); empty scans default + person_* partitions")
	dataDir := fs.String("data-dir", "", "memory data directory (default: the configured storage data dir)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	archive := *archiveConfirmed || *archiveLegacy
	dir := strings.TrimSpace(*dataDir)
	if dir == "" {
		dir = a.gatewayDataDir()
	}
	provider, err := memory.NewSQLiteProvider(dir)
	if err != nil {
		fmt.Fprintf(a.stderr, "memory-audit: cannot open memory provider: %v\n", err)
		return 1
	}
	defer provider.Close()

	partitions := []string{strings.TrimSpace(*partition)}
	if partitions[0] == "" {
		partitions = listMemoryAuditPartitions(dir)
	}
	confirmed, candidates, pinnedSkipped, archived := 0, 0, 0, 0
	for _, part := range partitions {
		memories, err := provider.ListCanonicalMemories(a.ctx, part, memory.CanonicalFilter{})
		if err != nil {
			fmt.Fprintf(a.stderr, "memory-audit: %s: %v\n", part, err)
			continue
		}
		var archiveIDs []string
		for _, m := range memories {
			verdict := memory.ClassifyTransientContent(m.Content)
			if verdict == memory.TransientNone {
				continue
			}
			if m.Pinned || m.UserConfirmed {
				// User decisions outrank every automatic ruling.
				pinnedSkipped++
				fmt.Fprintf(a.stdout, "[%s] %s PROTECTED (pinned/user-confirmed): %s\n", part, m.ID[:8], truncateAuditLine(m.Content))
				continue
			}
			switch verdict {
			case memory.TransientConfirmed:
				confirmed++
				archiveIDs = append(archiveIDs, m.ID)
				fmt.Fprintf(a.stdout, "[%s] %s CONFIRMED transient (instance + current-state semantics): %s\n", part, m.ID[:8], truncateAuditLine(m.Content))
			case memory.TransientCandidate:
				candidates++
				fmt.Fprintf(a.stdout, "[%s] %s candidate (status vocabulary only — review manually, never auto-archived): %s\n", part, m.ID[:8], truncateAuditLine(m.Content))
			}
		}
		if archive && len(archiveIDs) > 0 {
			if err := archiveAuditMemories(a.ctx, provider, part, archiveIDs); err != nil {
				fmt.Fprintf(a.stderr, "memory-audit: archive %s: %v\n", part, err)
				continue
			}
			archived += len(archiveIDs)
		}
	}
	if confirmed == 0 && candidates == 0 && pinnedSkipped == 0 {
		fmt.Fprintln(a.stdout, "No transient-state canonical memories found.")
		return 0
	}
	fmt.Fprintf(a.stdout, "Scan result: confirmed %d, candidates %d, protected %d.\n", confirmed, candidates, pinnedSkipped)
	if archive {
		fmt.Fprintf(a.stdout, "Archived %d confirmed transient memories (reversible via memory events). Candidates were left untouched.\n", archived)
	} else if confirmed > 0 {
		fmt.Fprintln(a.stdout, "Re-run with --archive-confirmed to shadow-archive the confirmed entries.")
	}
	return 0
}

func archiveAuditMemories(ctx context.Context, provider *memory.SQLiteProvider, partition string, ids []string) error {
	return provider.ArchiveCanonicals(ctx, partition, ids, "memory-audit", "confirmed transient run-state (instance + current-state semantics)")
}

// listMemoryAuditPartitions enumerates on-disk memory partitions: the legacy
// default partition plus every person partition that has a memory.db.
func listMemoryAuditPartitions(dataDir string) []string {
	out := []string{"default"}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return out
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "person_") {
			continue
		}
		if _, err := os.Stat(filepath.Join(dataDir, entry.Name(), "memory.db")); err == nil {
			out = append(out, entry.Name())
		}
	}
	return out
}

func truncateAuditLine(content string) string {
	content = strings.Join(strings.Fields(content), " ")
	runes := []rune(content)
	if len(runes) > 120 {
		return string(runes[:120]) + "..."
	}
	return content
}

// runMaintenanceMemoryArchiveEnvironment retires the pre-P3 environment half
// of person memory: person memory is preference-only now
// (docs/task-run-memory-simplification.zh-CN.md P3), so historical ACTIVE
// target="memory" canonical rows stop serving prompts by moving to `archived`
// (reversible; nothing is deleted, and pinned or user-confirmed rows are
// never touched). Dry-run by default.
func (a *App) runMaintenanceMemoryArchiveEnvironment(args []string) int {
	fs := flag.NewFlagSet("selfmind maintenance memory-archive-environment", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	apply := fs.Bool("apply", false, "archive the listed rows (default: report only)")
	partition := fs.String("partition", "", "single partition to process (e.g. person_<id> or default); empty scans default + person_* partitions")
	dataDir := fs.String("data-dir", "", "memory data directory (default: the configured storage data dir)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	dir := strings.TrimSpace(*dataDir)
	if dir == "" {
		dir = a.gatewayDataDir()
	}
	provider, err := memory.NewSQLiteProvider(dir)
	if err != nil {
		fmt.Fprintf(a.stderr, "memory-archive-environment: cannot open memory provider: %v\n", err)
		return 1
	}
	defer provider.Close()

	partitions := []string{strings.TrimSpace(*partition)}
	if partitions[0] == "" {
		partitions = listMemoryAuditPartitions(dir)
	}
	found, protected, archived := 0, 0, 0
	for _, part := range partitions {
		rows, err := provider.ListCanonicalMemories(a.ctx, part, memory.CanonicalFilter{Target: "memory"})
		if err != nil {
			fmt.Fprintf(a.stderr, "memory-archive-environment: %s: %v\n", part, err)
			continue
		}
		for _, row := range rows {
			if row.Status != memory.CanonicalActive {
				continue
			}
			if row.Pinned || row.UserConfirmed {
				protected++
				continue
			}
			found++
			preview := row.Content
			if len([]rune(preview)) > 90 {
				preview = string([]rune(preview)[:90]) + "…"
			}
			fmt.Fprintf(a.stdout, "%s %s [%s] %s\n", part, row.ID[:8], row.Scope, preview)
			if *apply {
				if err := provider.SetCanonicalStatus(a.ctx, part, row.ID, memory.CanonicalArchived, "maintenance"); err != nil {
					fmt.Fprintf(a.stderr, "memory-archive-environment: archive %s: %v\n", row.ID, err)
					continue
				}
				archived++
			}
		}
	}
	if *apply {
		fmt.Fprintf(a.stdout, "Archived %d environment memory row(s); %d protected (pinned/user-confirmed) left untouched.\n", archived, protected)
	} else {
		fmt.Fprintf(a.stdout, "Dry run: %d environment memory row(s) would be archived; %d protected (pinned/user-confirmed) untouched. Re-run with --apply.\n", found, protected)
	}
	return 0
}
