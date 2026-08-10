package cliapp

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	selfeval "selfmind/internal/eval"
	"selfmind/internal/kernel/llm"
)

// selfcheck is the Phase 0 regression gate ("catch" net): build + test + offline
// eval, aggregated into one pass/fail. It must never make live model calls — the
// eval phase runs in strict offline VCR replay, so it cannot burn provider quota
// and stays deterministic. Cases without a recorded cassette are reported and
// skipped so the gate grows as cassettes are recorded — EXCEPT cases marked
// `require_cassette: true`, whose missing cassette is a failure, and the
// SELFMIND_EVAL_MIN_CASES floor, which fails the gate when fewer cases were
// actually replayed than required (keeps CI honest about verifying ~0 cases).
func (a *App) runSelfcheckCommandIfRequested() (bool, int) {
	if len(a.args) < 2 || a.args[1] != "selfcheck" {
		return false, 0
	}
	args := a.args[2:]
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		a.printSelfcheckHelp()
		return true, 0
	}

	skipGo, skipEval, fast := false, false, false
	evalDir := "evalcases"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--skip-go":
			skipGo = true
		case "--skip-eval":
			skipEval = true
		case "--fast":
			fast = true
		case "--eval-dir":
			i++
			if i >= len(args) {
				fmt.Fprintln(a.stderr, "--eval-dir requires a path")
				return true, 2
			}
			evalDir = args[i]
		default:
			fmt.Fprintf(a.stderr, "unknown flag: %s\n", args[i])
			return true, 2
		}
	}

	failed := false
	if !skipGo {
		if !a.selfcheckGo() {
			failed = true
		}
	}
	if !skipEval {
		if !a.selfcheckEval(evalDir, fast) {
			failed = true
		}
	}

	fmt.Fprintln(a.stdout)
	if failed {
		fmt.Fprintln(a.stdout, "selfcheck: FAIL")
		return true, 1
	}
	fmt.Fprintln(a.stdout, "selfcheck: OK")
	return true, 0
}

func (a *App) printSelfcheckHelp() {
	fmt.Fprintln(a.stdout, "SelfMind selfcheck — local regression gate (build + test + offline eval)")
	fmt.Fprintln(a.stdout)
	fmt.Fprintln(a.stdout, "Usage:")
	fmt.Fprintln(a.stdout, "  selfmind selfcheck [--fast] [--skip-go] [--skip-eval] [--eval-dir DIR]")
	fmt.Fprintln(a.stdout)
	fmt.Fprintln(a.stdout, "  --fast        skip the few cases whose measured replay cost dominates the")
	fmt.Fprintln(a.stdout, "                run — the loop to use after each change. Run with no flags")
	fmt.Fprintln(a.stdout, "                before pushing; mandatory cases are never skipped.")
	fmt.Fprintln(a.stdout, "  --skip-go     skip `go build` / `go test` (eval gate only; used by CI)")
	fmt.Fprintln(a.stdout, "  --skip-eval   skip the offline eval suite (build/test only)")
	fmt.Fprintln(a.stdout)
	fmt.Fprintln(a.stdout, "The eval phase runs strictly offline (VCR replay). Cases without a recorded")
	fmt.Fprintln(a.stdout, "cassette are skipped — unless the case sets `require_cassette: true`, which")
	fmt.Fprintln(a.stdout, "makes a missing cassette a failure. Set SELFMIND_EVAL_MIN_CASES=N to fail")
	fmt.Fprintln(a.stdout, "when fewer than N cases were actually replayed. No live model calls are made.")
}

// selfcheckGo runs `go build ./...` then `go test ./...` from the repo root with
// GOWORK=off. It degrades gracefully when the Go toolchain or repo is absent.
func (a *App) selfcheckGo() bool {
	fmt.Fprintln(a.stdout, "== build & test ==")
	goBin, err := exec.LookPath("go")
	if err != nil {
		fmt.Fprintln(a.stdout, "  go toolchain not found on PATH; skipping build/test")
		return true
	}
	root := repoRoot()
	if root == "" {
		fmt.Fprintln(a.stdout, "  go.mod not found from cwd upward; skipping build/test")
		return true
	}
	ok := true
	for _, step := range []struct {
		label string
		cmd   []string
	}{
		{"go build ./...", []string{"build", "./..."}},
		{"go test ./...", []string{"test", "./..."}},
	} {
		start := time.Now()
		c := exec.CommandContext(a.ctx, goBin, step.cmd...)
		c.Dir = root
		c.Env = selfcheckGoEnv(os.Environ())
		out, runErr := c.CombinedOutput()
		if runErr != nil {
			ok = false
			fmt.Fprintf(a.stdout, "  FAIL %s (%s)\n", step.label, time.Since(start).Round(time.Millisecond))
			fmt.Fprint(a.stdout, indent(strings.TrimSpace(string(out)), "    "))
			fmt.Fprintln(a.stdout)
			continue
		}
		fmt.Fprintf(a.stdout, "  ok   %s (%s)\n", step.label, time.Since(start).Round(time.Millisecond))
	}
	return ok
}

// selfcheckGoEnv keeps ordinary build and unit-test processes independent from
// the eval/flight wrappers that may be enabled in the caller's interactive
// shell. The eval phase enables replay explicitly in selfcheckEval.
func selfcheckGoEnv(environ []string) []string {
	blocked := map[string]struct{}{
		"SELFMIND_EVAL_VCR":        {},
		"SELFMIND_EVAL_OFFLINE":    {},
		"SELFMIND_EVAL_VCR_DIR":    {},
		"SELFMIND_FLIGHT_RECORDER": {},
		"SELFMIND_FLIGHT_DIR":      {},
		"SELFMIND_FLIGHT_KEEP":     {},
		"GOWORK":                   {},
	}
	out := make([]string, 0, len(environ)+1)
	for _, entry := range environ {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, skip := blocked[strings.ToUpper(strings.TrimSpace(key))]; skip {
			continue
		}
		out = append(out, entry)
	}
	return append(out, "GOWORK=off")
}

// selfcheckEval replays recorded eval cases under root with strict offline VCR.
func (a *App) selfcheckEval(root string, fast bool) bool {
	fmt.Fprintf(a.stdout, "== offline eval (%s) ==\n", root)
	// Force strict offline replay so a cassette miss errors instead of calling
	// out. Preserve any caller-set values and restore them after.
	restore := setEnvScoped(map[string]string{
		"SELFMIND_EVAL_VCR":     "replay",
		"SELFMIND_EVAL_OFFLINE": "1",
	})
	defer restore()

	vcrDir := strings.TrimSpace(os.Getenv("SELFMIND_EVAL_VCR_DIR"))

	files, err := selfeval.ListCaseFiles(root)
	if err != nil {
		fmt.Fprintf(a.stdout, "  no eval cases: %v\n", err)
		return true
	}
	// A duration ceiling lets CI run the fast cases and leave the long ones to
	// the local loop. Long cases are the ones whose value is mostly model
	// behaviour, and their replay cost is real: skipping them keeps the shared
	// gate quick without giving up the race and environment coverage that only
	// a different machine provides. The count is reported, never silent.
	maxCaseSeconds := 0
	if v := strings.TrimSpace(os.Getenv("SELFMIND_EVAL_MAX_CASE_SECONDS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxCaseSeconds = n
		}
	}
	passed, failed, skipped, replayed, tooLong, slowSkipped := 0, 0, 0, 0, 0, 0
	for _, file := range files {
		c, err := selfeval.LoadCase(file)
		if err != nil {
			failed++
			fmt.Fprintf(a.stdout, "  FAIL %s (invalid: %v)\n", file, err)
			continue
		}
		if !llm.HasCassetteSession(vcrDir, c.ID) {
			// Mandatory cases must not silently drop out of the gate: a missing
			// cassette is a failure, not a skip.
			if c.RequireCassette {
				failed++
				fmt.Fprintf(a.stdout, "  FAIL %s (require_cassette: true but no cassette recorded; run `SELFMIND_EVAL_VCR=record selfmind eval run %s` and commit .vcr/%s/)\n", c.ID, file, c.ID)
				continue
			}
			skipped++
			continue
		}
		// Ordering matters: the cassette check runs first so "no cassette" stays
		// an accurate count, and the duration ceiling only ever reports cases
		// that COULD have run here. A mandatory case is never dropped by a cost
		// policy.
		if maxCaseSeconds > 0 && c.Expect.MaxDurationSeconds > maxCaseSeconds && !c.RequireCassette {
			tooLong++
			continue
		}
		// --fast drops the handful of cases whose measured replay cost dominates
		// the run, so the loop you run after every change stays in the tens of
		// seconds. A mandatory case is never dropped, and the count is reported.
		if fast && c.Slow && !c.RequireCassette {
			slowSkipped++
			continue
		}
		replayed++
		result, err := selfeval.RunCaseFile(a.ctx, file, selfeval.RunOptions{ConfigPath: a.configPath})
		if err != nil {
			failed++
			fmt.Fprintf(a.stdout, "  FAIL %s (%v)\n", c.ID, err)
			continue
		}
		if result.Status == "passed" {
			passed++
			fmt.Fprintf(a.stdout, "  ok   %s\n", c.ID)
		} else {
			failed++
			fmt.Fprintf(a.stdout, "  FAIL %s (%s)\n", c.ID, result.Status)
		}
	}
	fmt.Fprintf(a.stdout, "  summary: %d passed, %d failed, %d skipped (no cassette), %d replayed\n", passed, failed, skipped, replayed)
	if slowSkipped > 0 {
		fmt.Fprintf(a.stdout, "  note: %d slow case(s) skipped by --fast; run `selfmind selfcheck` with no flags before pushing\n", slowSkipped)
	}
	if tooLong > 0 {
		fmt.Fprintf(a.stdout, "  note: %d case(s) skipped by SELFMIND_EVAL_MAX_CASE_SECONDS=%d; run `selfmind selfcheck` locally to cover them\n", tooLong, maxCaseSeconds)
	}
	if passed == 0 && failed == 0 {
		fmt.Fprintln(a.stdout, "  note: no recorded cassettes yet; record with `selfmind eval run --live` then commit cassettes locally")
	}
	// SELFMIND_EVAL_MIN_CASES is the CI floor: it fails the gate when the number
	// of actually-replayed (non-skipped) cases falls below the threshold, so the
	// gate cannot quietly degrade to verifying ~0 cases (e.g. cassettes deleted
	// or case IDs renamed away from their cassettes). Unset keeps the local
	// grow-as-you-record default.
	if raw := strings.TrimSpace(os.Getenv("SELFMIND_EVAL_MIN_CASES")); raw != "" {
		min, err := strconv.Atoi(raw)
		if err != nil || min < 0 {
			fmt.Fprintf(a.stdout, "  FAIL invalid SELFMIND_EVAL_MIN_CASES=%q (expected a non-negative integer)\n", raw)
			return false
		}
		if replayed < min {
			fmt.Fprintf(a.stdout, "  FAIL eval gate replayed %d case(s) but SELFMIND_EVAL_MIN_CASES=%d requires at least %d; record missing cassettes with `SELFMIND_EVAL_VCR=record selfmind eval run <case-or-dir>` and commit .vcr/<case-id>/\n", replayed, min, min)
			return false
		}
	}
	return failed == 0
}

// repoRoot walks up from the working directory to find a go.mod.
func repoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// setEnvScoped sets env vars and returns a function that restores prior values.
func setEnvScoped(kv map[string]string) func() {
	prev := make(map[string]*string, len(kv))
	for k := range kv {
		if v, ok := os.LookupEnv(k); ok {
			vv := v
			prev[k] = &vv
		} else {
			prev[k] = nil
		}
	}
	for k, v := range kv {
		_ = os.Setenv(k, v)
	}
	return func() {
		for k, v := range prev {
			if v == nil {
				_ = os.Unsetenv(k)
			} else {
				_ = os.Setenv(k, *v)
			}
		}
	}
}

func indent(s, pad string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = pad + ln
	}
	return strings.Join(lines, "\n")
}
