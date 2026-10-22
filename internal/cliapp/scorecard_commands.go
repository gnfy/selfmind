package cliapp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	selfeval "selfmind/internal/eval"
)

// evalScorecard runs the existing harness and preserves per-sample evidence.
func (a *App) evalScorecard(args []string) int {
	target := "evalcases/dayinlife"
	out := ""
	opts := selfeval.RunOptions{ConfigPath: a.configPath}
	live := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--model":
			i++
			if i >= len(args) {
				fmt.Fprintln(a.stderr, "--model requires a value")
				return 2
			}
			opts.Model = args[i]
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
	if live && strings.EqualFold(os.Getenv("SELFMIND_EVAL_VCR"), "replay") {
		fmt.Fprintln(a.stderr, "--live conflicts with SELFMIND_EVAL_VCR=replay; unset replay mode first")
		return 2
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

	results := selfeval.ResultSet{Version: 1}
	passed := 0
	for _, file := range files {
		c, loadErr := selfeval.LoadCase(file)
		if loadErr != nil {
			results.Add(selfeval.RunResult{CaseID: filepath.Base(file), Status: "error", Error: loadErr.Error()})
			continue
		}
		if live {
			fmt.Fprintf(a.stdout, "running %s ...\n", c.ID)
		}
		r, err := selfeval.RunCaseFile(a.ctx, file, opts)
		if err != nil && r == nil {
			r = &selfeval.RunResult{CaseID: c.ID, Status: "error", Error: err.Error()}
		}
		results.Add(*r)
		if r.Status == "passed" {
			passed++
		}
	}

	provider := opts.Provider
	if provider == "" {
		provider = "(default)"
	}
	report := results.Markdown()
	fmt.Fprintln(a.stdout, report)
	if out == "" {
		out = filepath.Join("evalruns", fmt.Sprintf("scorecard-%s.md", sanitizeProvider(provider)))
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	if err := os.WriteFile(out, []byte(report), 0o644); err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	jsonPath := out + ".json"
	if err := os.WriteFile(jsonPath, data, 0o600); err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	if err := os.Chmod(jsonPath, 0o600); err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	fmt.Fprintf(a.stdout, "scorecard written to %s; samples: %s\n", out, jsonPath)

	if passed < len(files) {
		return 1
	}
	return 0
}

func sanitizeProvider(p string) string {
	return strings.NewReplacer("(", "", ")", "", "/", "-", " ", "-").Replace(p)
}
