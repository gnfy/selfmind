package cliapp

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"selfmind/internal/tools"
)

func (a *App) runMaintenanceMigrateSkills(args []string) int {
	fs := flag.NewFlagSet("selfmind maintenance migrate-skills", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	apply := fs.Bool("apply", false, "apply the migration; default is dry-run")
	root := fs.String("root", "", "SelfMind home containing person_* partitions")
	grace := fs.Duration("governance-grace", tools.DefaultSkillMigrationGrace, "curator grace period for migrated skills")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintf(a.stderr, "maintenance migrate-skills: resolve home: %v\n", err)
			return 1
		}
		*root = filepath.Join(home, ".selfmind")
	}
	if *grace < 0 || *grace > 365*24*time.Hour {
		fmt.Fprintln(a.stderr, "maintenance migrate-skills: --governance-grace must be between 0 and 365d")
		return 2
	}
	report, err := tools.MigratePersonSkillsToControl(*root, a.tenantID(), *apply, *grace)
	if err != nil {
		fmt.Fprintf(a.stderr, "maintenance migrate-skills: %v\n", err)
		return 1
	}
	fmt.Fprint(a.stdout, tools.FormatSkillMigrationReport(report))
	if report.Conflicts > 0 {
		fmt.Fprintln(a.stdout, "Conflicting person copies were left in place and remain read-only during the compatibility window.")
	}
	return 0
}
