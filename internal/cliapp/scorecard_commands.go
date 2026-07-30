package cliapp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	selfeval "selfmind/internal/eval"
)

// evalScorecard (W8.4) runs a scenario suite and produces a "can it replace
// codex" scorecard: per-scenario pass/fail plus metrics, and an aggregate. With
// --provider it runs as that provider (e.g. codex-cli for the parity column or
// kimi-coding for the cheap bulk column). Output is markdown, written to --out
// and printed, so it can be shared as evidence.
func (a *App) evalScorecard(args []string) int {
	target := "evalcases/dayinlife"
	out := ""
	opts := selfeval.RunOptions{ConfigPath: a.configPath}
	live := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--provider":
			i++
			if i >= len(args) {
				fmt.Fprintln(a.stderr, "--provider requires a value")
				return 2
			}
			opts.Provider = args[i]
		case "--out":
			i++
			if i >= len(args) {
				fmt.Fprintln(a.stderr, "--out requires a path")
				return 2
			}
			out = args[i]
		case "--live":
			live = true
			opts.ProgressWriter = a.stdout
		default:
			if strings.HasPrefix(args[i], "-") {
				fmt.Fprintf(a.stderr, "unknown flag: %s\n", args[i])
				return 2
			}
			target = args[i]
		}
	}
	if !live {
		// Default to offline replay so a scorecard run cannot burn quota by
		// accident; pass --live (with a --provider) to record/run for real.
		restore := setEnvScoped(map[string]string{"SELFMIND_EVAL_VCR": "replay", "SELFMIND_EVAL_OFFLINE": "1"})
		defer restore()
	}

	files, err := selfeval.ListCaseFiles(target)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	if len(files) == 0 {
		fmt.Fprintf(a.stderr, "no scenarios found: %s\n", target)
		return 1
	}

	type row struct {
		id, title, status string
		tools, errs       int
		dur               time.Duration
		failedChecks      []string
	}
	var rows []row
	passed := 0
	for _, file := range files {
		c, _ := selfeval.LoadCase(file)
		id, title := filepath.Base(file), ""
		if c != nil {
			id, title = c.ID, c.Title
		}
		if live {
			fmt.Fprintf(a.stdout, "running %s ...\n", id)
		}
		r, err := selfeval.RunCaseFile(a.ctx, file, opts)
		rw := row{id: id, title: title}
		if err != nil {
			rw.status = "error"
			rw.failedChecks = []string{err.Error()}
		} else {
			rw.status = r.Status
			rw.tools, rw.errs, rw.dur = r.ToolCalls, r.ToolErrors, r.Duration
			for _, ck := range r.Checks {
				if !ck.OK {
					rw.failedChecks = append(rw.failedChecks, ck.Name)
				}
			}
			if r.Status == "passed" {
				passed++
			}
		}
		rows = append(rows, rw)
	}

	provider := opts.Provider
	if provider == "" {
		provider = "(default)"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "# SelfMind day-in-the-life scorecard\n\n")
	fmt.Fprintf(&sb, "- suite: `%s`\n- provider: `%s`\n- mode: %s\n- result: **%d/%d scenarios passed**\n\n",
		target, provider, liveLabel(live), passed, len(rows))
	sb.WriteString("| Scenario | Status | Tools | Errors | Duration | Failed checks |\n")
	sb.WriteString("|---|---|---|---|---|---|\n")
	for _, r := range rows {
		mark := "✅"
		if r.status != "passed" {
			mark = "❌"
		}
		fc := strings.Join(r.failedChecks, ", ")
		if fc == "" {
			fc = "—"
		}
		fmt.Fprintf(&sb, "| %s %s | %s | %d | %d | %s | %s |\n",
			mark, firstNonEmptyStr(r.title, r.id), r.status, r.tools, r.errs, r.dur.Round(time.Millisecond), fc)
	}
	report := sb.String()
	fmt.Fprintln(a.stdout, report)

	if out == "" {
		out = filepath.Join("evalruns", fmt.Sprintf("scorecard-%s.md", sanitizeProvider(provider)))
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err == nil {
		if err := os.WriteFile(out, []byte(report), 0o644); err == nil {
			fmt.Fprintf(a.stdout, "scorecard written to %s\n", out)
		}
	}
	if passed < len(rows) {
		return 1
	}
	return 0
}

func liveLabel(live bool) string {
	if live {
		return "live"
	}
	return "offline replay"
}

func firstNonEmptyStr(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func sanitizeProvider(p string) string {
	return strings.NewReplacer("(", "", ")", "", "/", "-", " ", "-").Replace(p)
}
