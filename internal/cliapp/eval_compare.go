package cliapp

import (
	"fmt"
	selfeval "selfmind/internal/eval"
)

func (a *App) evalCanary(args []string) int {
	if len(args) != 2 || args[0] != "init" {
		fmt.Fprintln(a.stderr, "usage: selfmind eval canary init <new-private-directory>")
		return 2
	}
	paths, err := selfeval.InitCanary(args[1])
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	fmt.Fprintf(a.stdout, "Created %d isolated canary fixtures. No model calls were made.\n", len(paths))
	fmt.Fprintln(a.stdout, "Run explicitly with `selfmind eval scorecard <directory> --live --out <report.md>`; this uses the configured route and incurs provider usage. Compare both builds using the same fixtures, route, reasoning and policy. Each case requests three samples; retain failures.")
	return 0
}

func (a *App) evalCompare(args []string) int {
	if len(args) != 2 {
		fmt.Fprintln(a.stderr, "usage: selfmind eval compare <before> <after> (scorecard JSON, JSONL, or JSONL directory)")
		return 2
	}
	before, err := selfeval.LoadResultSet(args[0])
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	after, err := selfeval.LoadResultSet(args[1])
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	fmt.Fprint(a.stdout, selfeval.CompareResultSets(*before, *after))
	return 0
}
