package cliapp

import (
	"fmt"
	"time"

	"selfmind/internal/doccheck"
)

func (a *App) runDocsCommandIfRequested() (bool, int) {
	if len(a.args) < 2 || a.args[1] != "docs" {
		return false, 0
	}
	subcommand := "check"
	if len(a.args) > 2 {
		subcommand = a.args[2]
	}
	if len(a.args) > 3 {
		fmt.Fprintln(a.stderr, "Usage: selfmind docs [check|index]")
		return true, 2
	}
	root := repoRoot()
	if root == "" {
		fmt.Fprintln(a.stderr, "documentation check requires a SelfMind source checkout (go.mod not found)")
		return true, 2
	}
	switch subcommand {
	case "check":
		report := doccheck.Check(root, time.Now())
		if !report.OK() {
			for _, issue := range report.Errors {
				fmt.Fprintf(a.stderr, "- %s\n", issue)
			}
			fmt.Fprintf(a.stderr, "docs check: FAIL (%d issue(s))\n", len(report.Errors))
			return true, 1
		}
		fmt.Fprintf(a.stdout, "docs check: OK (%d documents, %d active plan)\n", report.Documents, report.ActivePlans)
		return true, 0
	case "index":
		if err := doccheck.WriteIndex(root); err != nil {
			fmt.Fprintf(a.stderr, "docs index: %v\n", err)
			return true, 1
		}
		fmt.Fprintln(a.stdout, "docs index: updated docs/README.md")
		return true, 0
	case "-h", "--help", "help":
		fmt.Fprintln(a.stdout, "Usage: selfmind docs [check|index]")
		fmt.Fprintln(a.stdout, "  check  validate the documentation contract")
		fmt.Fprintln(a.stdout, "  index  regenerate docs/README.md from docs/manifest.yaml")
		return true, 0
	default:
		fmt.Fprintf(a.stderr, "unknown docs command %q\n", subcommand)
		fmt.Fprintln(a.stderr, "Usage: selfmind docs [check|index]")
		return true, 2
	}
}
