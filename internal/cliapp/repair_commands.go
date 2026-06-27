package cliapp

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	selfeval "selfmind/internal/eval"
)

// evalRepair (W0.4 — bounded self-repair) re-runs failing eval cases offline and
// emits a structured "repair brief" for each: the failed checks, the trace
// path, and the case file. The brief is consumable by a follow-up agent run or a
// human. Repair is intentionally NOT auto-applied: a bad automated fix wastes
// more time than it saves, so the apply step stays human/approval-gated. With
// --worktree it also provisions an isolated git worktree so a fix can be
// attempted without touching the working tree.
func (a *App) evalRepair(args []string) int {
	target := "evalcases"
	useWorktree := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--worktree":
			useWorktree = true
		default:
			if strings.HasPrefix(args[i], "-") {
				fmt.Fprintf(a.stderr, "unknown flag: %s\n", args[i])
				return 2
			}
			target = args[i]
		}
	}

	// Strictly offline: a repair pass must never call out or burn quota.
	restore := setEnvScoped(map[string]string{
		"SELFMIND_EVAL_VCR":     "replay",
		"SELFMIND_EVAL_OFFLINE": "1",
	})
	defer restore()

	files, err := selfeval.ListCaseFiles(target)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	if len(files) == 0 {
		fmt.Fprintf(a.stderr, "no eval cases found: %s\n", target)
		return 1
	}

	needRepair := 0
	for _, file := range files {
		result, err := selfeval.RunCaseFile(a.ctx, file, selfeval.RunOptions{ConfigPath: a.configPath})
		if err != nil {
			needRepair++
			a.printRepairBrief(file, nil, err, useWorktree)
			continue
		}
		if result.Status == "passed" {
			continue
		}
		needRepair++
		a.printRepairBrief(file, result, nil, useWorktree)
	}

	if needRepair == 0 {
		fmt.Fprintln(a.stdout, "All recorded cases pass; nothing to repair.")
		return 0
	}
	fmt.Fprintf(a.stdout, "\n%d case(s) need repair. Review each brief, apply a fix, then re-run `selfmind selfcheck`.\n", needRepair)
	return 1
}

func (a *App) printRepairBrief(file string, result *selfeval.RunResult, runErr error, useWorktree bool) {
	fmt.Fprintln(a.stdout, strings.Repeat("─", 60))
	fmt.Fprintf(a.stdout, "REPAIR BRIEF: %s\n", file)
	if runErr != nil {
		fmt.Fprintf(a.stdout, "  run error: %v\n", runErr)
	}
	if result != nil {
		fmt.Fprintf(a.stdout, "  case: %s\n", result.CaseID)
		fmt.Fprintf(a.stdout, "  status: %s  (tools=%d errors=%d)\n", result.Status, result.ToolCalls, result.ToolErrors)
		var failed []string
		for _, c := range result.Checks {
			if !c.OK {
				msg := c.Message
				if msg == "" {
					msg = "(no detail)"
				}
				failed = append(failed, fmt.Sprintf("    - %s: %s", c.Name, msg))
			}
		}
		if len(failed) > 0 {
			fmt.Fprintln(a.stdout, "  failed checks:")
			fmt.Fprintln(a.stdout, strings.Join(failed, "\n"))
		}
		if result.OutputPath != "" {
			fmt.Fprintf(a.stdout, "  trace: %s\n", result.OutputPath)
		}
	}
	if useWorktree {
		if path, err := a.provisionRepairWorktree(file); err != nil {
			fmt.Fprintf(a.stdout, "  worktree: unavailable (%v)\n", err)
		} else {
			fmt.Fprintf(a.stdout, "  worktree: %s  (fix here, verify, then `git worktree remove` it)\n", path)
		}
	}
	fmt.Fprintln(a.stdout, "  next: hand this brief to the agent or fix manually, re-run the case, keep the diff only if it goes green.")
}

// provisionRepairWorktree creates an isolated git worktree off HEAD so a repair
// attempt cannot disturb the working tree. It is opt-in and never auto-removed.
func (a *App) provisionRepairWorktree(caseFile string) (string, error) {
	root := repoRoot()
	if root == "" {
		return "", fmt.Errorf("not a git repo")
	}
	if _, err := exec.LookPath("git"); err != nil {
		return "", fmt.Errorf("git not found")
	}
	name := fmt.Sprintf("repair-%s", strings.NewReplacer("/", "-", ".", "-").Replace(filepath.Base(caseFile)))
	path := filepath.Join(root, ".worktrees", name)
	cmd := exec.CommandContext(a.ctx, "git", "worktree", "add", "--detach", path, "HEAD")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return path, nil
}
