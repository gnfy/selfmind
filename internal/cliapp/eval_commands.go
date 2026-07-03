package cliapp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"selfmind/internal/control"
	selfeval "selfmind/internal/eval"
)

func (a *App) runEvalCommandIfRequested() (bool, int) {
	if len(a.args) < 2 || a.args[1] != "eval" {
		return false, 0
	}
	args := a.args[2:]
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		a.printEvalHelp()
		return true, 0
	}
	switch args[0] {
	case "list":
		return true, a.evalList(args[1:])
	case "run":
		return true, a.evalRun(args[1:])
	case "report":
		return true, a.evalReport(args[1:])
	case "repair":
		return true, a.evalRepair(args[1:])
	case "scorecard":
		return true, a.evalScorecard(args[1:])
	case "capture":
		return true, a.evalCapture(args[1:])
	case "clean":
		return true, a.evalClean(args[1:])
	default:
		fmt.Fprintln(a.stderr, "usage: selfmind eval [list|run|report|repair|scorecard|capture|clean]")
		return true, 2
	}
}

func (a *App) printEvalHelp() {
	fmt.Fprintln(a.stdout, "SelfMind eval")
	fmt.Fprintln(a.stdout)
	fmt.Fprintln(a.stdout, "Usage:")
	fmt.Fprintln(a.stdout, "  selfmind eval list [path]")
	fmt.Fprintln(a.stdout, "  selfmind eval run [case-or-dir] [--suite NAME] [--provider ID] [--model ID] [--live]")
	fmt.Fprintln(a.stdout, "  selfmind eval report <jsonl-or-dir>")
	fmt.Fprintln(a.stdout, "  selfmind eval repair [case-or-dir] [--worktree]")
	fmt.Fprintln(a.stdout, "  selfmind eval capture [turn-id|latest] [--title \"...\"] [--suite NAME]")
	fmt.Fprintln(a.stdout, "  selfmind eval clean [--yes]   remove eval residue from the configured control.db (dry-run by default)")
	fmt.Fprintln(a.stdout)
	fmt.Fprintln(a.stdout, "Examples:")
	fmt.Fprintln(a.stdout, "  selfmind eval run evalcases/daily-dev/chat_basic.yaml")
	fmt.Fprintln(a.stdout, "  selfmind eval run --suite daily-dev --provider kimi-coding")
}

func (a *App) evalList(args []string) int {
	root := "evalcases"
	if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
		root = args[0]
	}
	files, err := selfeval.ListCaseFiles(root)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	if len(files) == 0 {
		fmt.Fprintf(a.stdout, "No eval cases found under %s\n", root)
		return 0
	}
	for _, file := range files {
		c, err := selfeval.LoadCase(file)
		if err != nil {
			fmt.Fprintf(a.stdout, "- %s  (invalid: %v)\n", file, err)
			continue
		}
		fmt.Fprintf(a.stdout, "- %s  %s\n", c.ID, file)
	}
	return 0
}

func (a *App) evalRun(args []string) int {
	target, opts, err := a.parseEvalRunArgs(args)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 2
	}
	files, err := selfeval.ListCaseFiles(target)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	if len(files) == 0 {
		fmt.Fprintf(a.stderr, "no eval cases found: %s\n", target)
		return 1
	}
	if len(files) > 1 && strings.TrimSpace(opts.OutputPath) != "" {
		fmt.Fprintln(a.stderr, "--output can only be used with a single case file")
		return 2
	}
	failed := 0
	for _, file := range files {
		start := time.Now()
		fmt.Fprintf(a.stdout, "Running %s\n", file)
		result, err := selfeval.RunCaseFile(a.ctx, file, opts)
		if err != nil {
			failed++
			fmt.Fprintf(a.stdout, "  failed: %v\n", err)
			continue
		}
		if result.Status != "passed" {
			failed++
		}
		fmt.Fprintf(a.stdout, "  %s in %s  tools=%d errors=%d  log=%s\n", result.Status, time.Since(start).Round(time.Millisecond), result.ToolCalls, result.ToolErrors, result.OutputPath)
	}
	if failed > 0 {
		return 1
	}
	return 0
}

func (a *App) parseEvalRunArgs(args []string) (string, selfeval.RunOptions, error) {
	target := ""
	opts := selfeval.RunOptions{ConfigPath: a.configPath}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--suite":
			i++
			if i >= len(args) {
				return "", opts, fmt.Errorf("--suite requires a name")
			}
			target = filepath.Join("evalcases", args[i])
		case "--provider":
			i++
			if i >= len(args) {
				return "", opts, fmt.Errorf("--provider requires a value")
			}
			opts.Provider = args[i]
		case "--model":
			i++
			if i >= len(args) {
				return "", opts, fmt.Errorf("--model requires a value")
			}
			opts.Model = args[i]
		case "--tenant":
			i++
			if i >= len(args) {
				return "", opts, fmt.Errorf("--tenant requires a value")
			}
			opts.TenantID = args[i]
		case "--workspace":
			i++
			if i >= len(args) {
				return "", opts, fmt.Errorf("--workspace requires a path")
			}
			opts.Workspace = args[i]
		case "--output":
			i++
			if i >= len(args) {
				return "", opts, fmt.Errorf("--output requires a path")
			}
			opts.OutputPath = args[i]
		case "--record-content":
			opts.RecordContent = true
		case "--live":
			opts.ProgressWriter = a.stdout
		default:
			if strings.HasPrefix(arg, "-") {
				return "", opts, fmt.Errorf("unknown flag: %s", arg)
			}
			if target != "" {
				return "", opts, fmt.Errorf("multiple case paths provided")
			}
			target = arg
		}
	}
	if strings.TrimSpace(target) == "" {
		target = "evalcases"
	}
	return target, opts, nil
}

// evalCapture promotes a recorded flight turn into a replayable eval case.
func (a *App) evalCapture(args []string) int {
	turnID := "latest"
	opts := selfeval.CaptureOptions{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--title":
			i++
			if i >= len(args) {
				fmt.Fprintln(a.stderr, "--title requires a value")
				return 2
			}
			opts.Title = args[i]
		case "--suite":
			i++
			if i >= len(args) {
				fmt.Fprintln(a.stderr, "--suite requires a value")
				return 2
			}
			opts.Suite = args[i]
		default:
			if strings.HasPrefix(args[i], "-") {
				fmt.Fprintf(a.stderr, "unknown flag: %s\n", args[i])
				return 2
			}
			turnID = args[i]
		}
	}
	res, err := selfeval.CaptureFromFlight(turnID, opts)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	fmt.Fprintf(a.stdout, "Captured eval case: %s\n  case:     %s\n  cassette: %s (%d files)\n", res.CaseID, res.CasePath, res.VCRPath, res.Cassettes)
	fmt.Fprintf(a.stdout, "Next: edit %s to add `assert_state` (what should have happened), then run `selfmind selfcheck`.\n", res.CasePath)
	return 0
}

// evalClean removes eval residue (persons whose ONLY accounts have platform
// `eval`, plus all rows keyed to them) from the CONFIGURED data dir's
// control.db. Historic eval/record runs against the shared data dir leaked
// this state; new runs are isolated by default, so this is a one-shot cleanup.
// The default is a dry run; --yes applies the deletion.
func (a *App) evalClean(args []string) int {
	apply := false
	for _, arg := range args {
		switch arg {
		case "--yes", "-y":
			apply = true
		case "-h", "--help", "help":
			fmt.Fprintln(a.stdout, "usage: selfmind eval clean [--yes]")
			fmt.Fprintln(a.stdout)
			fmt.Fprintln(a.stdout, "Removes leftover eval identities (persons whose only accounts have platform")
			fmt.Fprintln(a.stdout, "'eval') and their accounts, tasks, runs, events, handoffs, artifacts, and")
			fmt.Fprintln(a.stdout, "current-task pointers from the configured data dir's control.db.")
			fmt.Fprintln(a.stdout, "Without --yes this is a dry run that only prints what would be deleted.")
			return 0
		default:
			fmt.Fprintf(a.stderr, "unknown flag: %s\n", arg)
			return 2
		}
	}

	dataDir := a.gatewayDataDir()
	fmt.Fprintf(a.stdout, "control.db data dir: %s\n", dataDir)
	fmt.Fprintln(a.stdout, "WARNING: stop the gateway daemon first (`selfmind gateway stop`). Two processes")
	fmt.Fprintln(a.stdout, "writing one control.db is the exact concurrency anti-pattern SelfMind bans.")
	fmt.Fprintln(a.stdout)

	store, err := control.OpenStore(dataDir)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	defer store.Close()

	report, err := store.CleanEvalResidue(a.ctx, apply)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	if report.Empty() {
		fmt.Fprintln(a.stdout, "No eval residue found.")
		return 0
	}
	verb := "would delete"
	if apply {
		verb = "deleted"
	}
	fmt.Fprintf(a.stdout, "Eval residue (%s):\n", verb)
	for _, row := range []struct {
		label string
		count int
	}{
		{"persons (eval-only accounts)", report.Persons},
		{"accounts", report.Accounts},
		{"workspaces", report.Workspaces},
		{"current_workspace rows", report.CurrentWorkspace},
		{"tasks", report.Tasks},
		{"current_task rows", report.CurrentTask},
		{"task_runs", report.Runs},
		{"task_events", report.Events},
		{"task_handoffs", report.Handoffs},
		{"task_artifacts", report.Artifacts},
		{"channel_messages", report.ChannelMessages},
		{"approval_requests", report.Approvals},
		{"notifications", report.Notifications},
		{"outbound_messages", report.Outbound},
		{"emptied eval tenants", report.Tenants},
	} {
		if row.count > 0 {
			fmt.Fprintf(a.stdout, "  %-30s %d\n", row.label, row.count)
		}
	}
	if !apply {
		fmt.Fprintln(a.stdout)
		fmt.Fprintln(a.stdout, "Dry run only. Re-run with `selfmind eval clean --yes` to delete.")
	}
	return 0
}

func (a *App) evalReport(args []string) int {
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		fmt.Fprintln(a.stderr, "usage: selfmind eval report <jsonl-or-dir>")
		return 2
	}
	path := args[0]
	info, err := os.Stat(path)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	if !info.IsDir() {
		report, err := selfeval.LoadReport(path)
		if err != nil {
			fmt.Fprintln(a.stderr, err)
			return 1
		}
		fmt.Fprintln(a.stdout, report.String())
		return 0
	}
	files, err := filepath.Glob(filepath.Join(path, "*.jsonl"))
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	if len(files) == 0 {
		fmt.Fprintf(a.stderr, "no jsonl files found: %s\n", path)
		return 1
	}
	for i, file := range files {
		report, err := selfeval.LoadReport(file)
		if err != nil {
			fmt.Fprintf(a.stderr, "%s: %v\n", file, err)
			return 1
		}
		if i > 0 {
			fmt.Fprintln(a.stdout)
		}
		fmt.Fprintln(a.stdout, report.String())
	}
	return 0
}
