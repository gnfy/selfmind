package cliapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"selfmind/internal/doccheck"
	selfeval "selfmind/internal/eval"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/platform/config"
	"selfmind/internal/platform/scratchhome"
)

type selfcheckOutcome int

const (
	selfcheckPassed selfcheckOutcome = iota
	selfcheckFailed
	selfcheckUnavailable
)

type selfcheckProfile string

const (
	selfcheckLocalFull selfcheckProfile = "local-full"
	selfcheckLocalFast selfcheckProfile = "local-fast"
	selfcheckCI        selfcheckProfile = "ci"
)

func mergeSelfcheckOutcome(current, next selfcheckOutcome) selfcheckOutcome {
	if next > current {
		return next
	}
	return current
}

// selfcheck is the release regression gate: documentation contract, build,
// tests, and provider-offline eval. Provider calls replay from VCR without
// quota, while tool calls still exercise the host. Profiles keep local
// coverage authoritative and make CI ownership explicit.
func (a *App) runSelfcheckCommandIfRequested() (bool, int) {
	if len(a.args) < 2 || a.args[1] != "selfcheck" {
		return false, 0
	}
	args := a.args[2:]
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		a.printSelfcheckHelp()
		return true, 0
	}

	skipGo, skipEval := false, false
	profile := selfcheckLocalFull
	profileSet, fastSet := false, false
	evalDir := "evalcases"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--skip-go":
			skipGo = true
		case "--skip-eval":
			skipEval = true
		case "--fast":
			fastSet = true
			profile = selfcheckLocalFast
		case "--profile":
			i++
			if i >= len(args) {
				fmt.Fprintln(a.stderr, "--profile requires local-full, local-fast, or ci")
				return true, 2
			}
			profileSet = true
			profile = selfcheckProfile(strings.ToLower(strings.TrimSpace(args[i])))
			if profile != selfcheckLocalFull && profile != selfcheckLocalFast && profile != selfcheckCI {
				fmt.Fprintf(a.stderr, "invalid --profile %q (expected local-full, local-fast, or ci)\n", args[i])
				return true, 2
			}
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
	if fastSet && profileSet {
		fmt.Fprintln(a.stderr, "--fast cannot be combined with --profile; use --profile local-fast")
		return true, 2
	}
	outcome := a.selfcheckDocs()
	if !skipGo {
		outcome = mergeSelfcheckOutcome(outcome, a.selfcheckGo())
	}
	if !skipEval {
		outcome = mergeSelfcheckOutcome(outcome, a.selfcheckEval(evalDir, profile))
	}

	fmt.Fprintln(a.stdout)
	switch outcome {
	case selfcheckUnavailable:
		fmt.Fprintln(a.stdout, "selfcheck: UNAVAILABLE")
		return true, 2
	case selfcheckFailed:
		fmt.Fprintln(a.stdout, "selfcheck: FAIL")
		return true, 1
	default:
		fmt.Fprintln(a.stdout, "selfcheck: OK")
		return true, 0
	}
}

func (a *App) printSelfcheckHelp() {
	fmt.Fprintln(a.stdout, "SelfMind selfcheck - release regression gate (docs + build + test + offline eval)")
	fmt.Fprintln(a.stdout)
	fmt.Fprintln(a.stdout, "Usage:")
	fmt.Fprintln(a.stdout, "  selfmind selfcheck [--fast | --profile PROFILE] [--skip-go] [--skip-eval] [--eval-dir DIR]")
	fmt.Fprintln(a.stdout)
	fmt.Fprintln(a.stdout, "  --fast        skip the few cases whose measured replay cost dominates the")
	fmt.Fprintln(a.stdout, "                run — the loop to use after each change. Run with no flags")
	fmt.Fprintln(a.stdout, "                before pushing; mandatory cases are never skipped.")
	fmt.Fprintln(a.stdout, "  --profile     local-full (default), local-fast, or ci. The ci profile runs")
	fmt.Fprintln(a.stdout, "                only cases explicitly owned by CI for the current platform.")
	fmt.Fprintln(a.stdout, "  --skip-go     skip `go build` / `go test` (used after a separate CI build)")
	fmt.Fprintln(a.stdout, "  --skip-eval   skip the offline eval suite")
	fmt.Fprintln(a.stdout)
	fmt.Fprintln(a.stdout, "The documentation contract always runs. Combining --skip-go and --skip-eval")
	fmt.Fprintln(a.stdout, "therefore performs a docs-only check.")
	fmt.Fprintln(a.stdout, "The provider phase runs strictly offline (VCR replay); replayed tool calls")
	fmt.Fprintln(a.stdout, "still execute against the current host toolchain. Every model-backed case in")
	fmt.Fprintln(a.stdout, "the release corpus must have a cassette; missing replay evidence is a failure.")
	fmt.Fprintln(a.stdout, "Deterministic control-plane cases declare `model_required: false` and run")
	fmt.Fprintln(a.stdout, "without a cassette. No live model calls are made.")
	fmt.Fprintln(a.stdout, "Missing host tools fail as environment unavailable (exit 2). The only exception")
	fmt.Fprintln(a.stdout, "is a case explicitly owned by CI for this platform: local output marks it")
	fmt.Fprintln(a.stdout, "CI-DEFERRED, and a release still requires that GitHub Actions job to pass.")
}

func (a *App) selfcheckDocs() selfcheckOutcome {
	fmt.Fprintln(a.stdout, "== documentation contract ==")
	root := repoRoot()
	if root == "" {
		fmt.Fprintln(a.stdout, "  UNAVAILABLE go.mod not found from cwd upward")
		return selfcheckUnavailable
	}
	report := doccheck.Check(root, time.Now())
	if !report.OK() {
		for _, issue := range report.Errors {
			fmt.Fprintf(a.stdout, "  FAIL %s\n", issue)
		}
		return selfcheckFailed
	}
	fmt.Fprintf(a.stdout, "  ok   %d documents, %d active plan\n", report.Documents, report.ActivePlans)
	return selfcheckPassed
}

// selfcheckGo runs `go build ./...` then `go test ./...` from the repo root with
// GOWORK=off. It degrades gracefully when the Go toolchain or repo is absent.
func (a *App) selfcheckGo() selfcheckOutcome {
	fmt.Fprintln(a.stdout, "== build & test ==")
	goBin, err := exec.LookPath("go")
	if err != nil {
		fmt.Fprintln(a.stdout, "  UNAVAILABLE go toolchain not found on PATH")
		return selfcheckUnavailable
	}
	root := repoRoot()
	if root == "" {
		fmt.Fprintln(a.stdout, "  UNAVAILABLE go.mod not found from cwd upward")
		return selfcheckUnavailable
	}
	return a.selfcheckGoIn(goBin, root)
}

// selfcheckGoIn builds and tests the module at root. The children run against
// a throwaway HOME so that no test can write the operator's per-user state;
// afterwards any SelfMind state they left there fails the gate.
func (a *App) selfcheckGoIn(goBin, root string) selfcheckOutcome {
	// Go's caches and settings are resolved for the original environment
	// before HOME moves, so nothing is rebuilt or re-downloaded.
	goDirs, err := scratchhome.GoDirs(a.ctx, goBin, os.Environ())
	if err != nil {
		fmt.Fprintf(a.stdout, "  UNAVAILABLE cannot resolve Go cache locations: %v\n", err)
		return selfcheckUnavailable
	}
	claim, err := scratchhome.Fresh(selfcheckGoHomeName(root))
	if err != nil {
		fmt.Fprintf(a.stdout, "  UNAVAILABLE cannot create a throwaway HOME for build and test: %v\n", err)
		return selfcheckUnavailable
	}
	defer func() { _ = claim.Release() }()
	switch {
	case claim.Busy:
		fmt.Fprintln(a.stdout, "  note: another selfcheck is running for this checkout; this run uses a HOME of its own, so Go's test cache will not recognize unchanged packages")
	case claim.Fallback != "":
		fmt.Fprintf(a.stdout, "  note: the stable throwaway HOME is unavailable (%s); this run uses a HOME of its own, so Go's test cache will not recognize unchanged packages\n", claim.Fallback)
	}
	home := claim.Home
	env := selfcheckGoEnv(os.Environ(), home, goDirs)

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
		c.Env = env
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
	if leaked := selfcheckHomeWrites(home); len(leaked) > 0 {
		ok = false
		fmt.Fprintln(a.stdout, "  FAIL tests left SelfMind state in their throwaway HOME (paths relative to it):")
		for i, path := range leaked {
			if i == selfcheckHomeWriteLimit {
				fmt.Fprintf(a.stdout, "    ... and %d more\n", len(leaked)-i)
				break
			}
			fmt.Fprintf(a.stdout, "    %s\n", path)
		}
		fmt.Fprintln(a.stdout, "    With the real HOME these writes reach the operator's own state. Isolate the writing")
		fmt.Fprintln(a.stdout, "    package's test binary: func TestMain(m *testing.M) { testhome.Main(m) } (internal/testutil/testhome).")
	}
	if !ok {
		return selfcheckFailed
	}
	return selfcheckPassed
}

// selfcheckGoHomeName names the build-and-test HOME after the checkout and
// the user. The path stays the same from run to run because Go's test cache
// records HOME whenever a test reads it; a new path each run would re-run
// every such package.
func selfcheckGoHomeName(root string) string {
	sum := sha256.Sum256([]byte(root))
	return fmt.Sprintf("selfmind-selfcheck-%d-%s", os.Getuid(), hex.EncodeToString(sum[:6]))
}

// selfcheckHomeWriteLimit bounds the paths a guard failure prints.
const selfcheckHomeWriteLimit = 20

// selfcheckHomeWrites lists, relative to home, the SelfMind state that build
// and test children left there: each ".selfmind*" tree and SelfMind's
// per-user cache directory (macOS Library/Caches, or the XDG cache that
// selfcheckGoEnv places below HOME), with the entries directly inside them.
func selfcheckHomeWrites(home string) []string {
	var roots []string
	if entries, err := os.ReadDir(home); err == nil {
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".selfmind") {
				roots = append(roots, entry.Name())
			}
		}
	}
	for _, cache := range []string{filepath.Join("Library", "Caches", "selfmind"), filepath.Join(".cache", "selfmind")} {
		if _, err := os.Lstat(filepath.Join(home, cache)); err == nil {
			roots = append(roots, cache)
		}
	}
	sort.Strings(roots)
	var paths []string
	for _, root := range roots {
		paths = append(paths, filepath.ToSlash(root))
		entries, _ := os.ReadDir(filepath.Join(home, root))
		for _, entry := range entries {
			paths = append(paths, filepath.ToSlash(filepath.Join(root, entry.Name())))
		}
	}
	return paths
}

// selfcheckGoEnv builds the environment of selfcheck's build and test
// children. It drops the eval/flight wrappers an interactive shell may enable
// (the eval phase enables replay explicitly in selfcheckEval), forces
// GOWORK=off, and roots every per-user location — HOME, the XDG directories,
// the SELF_CONFIG file — in the throwaway home, while goDirs keeps Go's own
// caches and settings where `go env` found them for the original
// environment. GOFLAGS passes through unchanged.
func selfcheckGoEnv(environ []string, home string, goDirs []string) []string {
	assignments := append(scratchhome.Env(home), goDirs...)
	assignments = append(assignments, "GOWORK=off")
	replaced := map[string]struct{}{
		"SELFMIND_EVAL_VCR":        {},
		"SELFMIND_EVAL_OFFLINE":    {},
		"SELFMIND_EVAL_VCR_DIR":    {},
		"SELFMIND_FLIGHT_RECORDER": {},
		"SELFMIND_FLIGHT_DIR":      {},
		"SELFMIND_FLIGHT_KEEP":     {},
	}
	for _, assignment := range assignments {
		key, _, _ := strings.Cut(assignment, "=")
		replaced[key] = struct{}{}
	}
	out := make([]string, 0, len(environ)+len(assignments))
	for _, entry := range environ {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, skip := replaced[strings.ToUpper(strings.TrimSpace(key))]; skip {
			continue
		}
		out = append(out, entry)
	}
	return append(out, assignments...)
}

type selfcheckEvalCase struct {
	file    string
	caseDef *selfeval.Case
}

type selfcheckSuiteStats struct {
	valid        int
	recorded     int
	providerless int
	selected     int
	runnable     int
	deferred     int
	missing      []string
}

// selfcheckEval replays recorded provider responses under root. Provider calls
// are strictly offline; tool calls still run against the host and are therefore
// preflighted before any case starts.
func (a *App) selfcheckEval(root string, profile selfcheckProfile) selfcheckOutcome {
	resolvedRoot, err := resolveSelfcheckEvalRoot(root)
	if err != nil {
		fmt.Fprintf(a.stdout, "== provider-offline eval (%s) ==\n  UNAVAILABLE %v\n", root, err)
		return selfcheckUnavailable
	}
	fmt.Fprintf(a.stdout, "== provider-offline eval (%s; profile=%s; platform=%s) ==\n", resolvedRoot, profile, runtime.GOOS)
	fmt.Fprintln(a.stdout, "  prompt snapshot: embedded defaults; overrides=none (forced)")
	configPath, configNote := a.selfcheckEvalConfig()
	fmt.Fprintf(a.stdout, "  config: %s\n", configNote)

	files, err := selfeval.ListCaseFiles(resolvedRoot)
	if err != nil {
		fmt.Fprintf(a.stdout, "  UNAVAILABLE cannot list eval cases: %v\n", err)
		return selfcheckUnavailable
	}
	if len(files) == 0 {
		fmt.Fprintln(a.stdout, "  UNAVAILABLE eval directory contains no YAML cases")
		return selfcheckUnavailable
	}
	vcrDir := strings.TrimSpace(os.Getenv("SELFMIND_EVAL_VCR_DIR"))
	if vcrDir == "" {
		repo := repoRoot()
		if repo == "" {
			fmt.Fprintln(a.stdout, "  UNAVAILABLE cannot resolve the default .vcr directory without a repository root")
			return selfcheckUnavailable
		}
		vcrDir = filepath.Join(repo, ".vcr")
	} else if !filepath.IsAbs(vcrDir) {
		vcrDir = filepath.Clean(vcrDir)
	}

	failed := 0
	profileSkipped := 0
	loadedCount, recordedSessions := 0, 0
	suites := map[string]*selfcheckSuiteStats{}
	selected := make([]selfcheckEvalCase, 0, len(files))
	for _, file := range files {
		c, loadErr := selfeval.LoadCase(file)
		if loadErr != nil {
			failed++
			fmt.Fprintf(a.stdout, "  FAIL %s (invalid: %v)\n", file, loadErr)
			continue
		}
		loadedCount++
		suite := selfcheckCaseSuite(c, file)
		stats := suites[suite]
		if stats == nil {
			stats = &selfcheckSuiteStats{}
			suites[suite] = stats
		}
		stats.valid++
		if !c.RequiresModel() {
			stats.providerless++
		}
		if llm.HasCassetteSession(vcrDir, c.ID) {
			recordedSessions++
			stats.recorded++
		}
		if profile == selfcheckCI && !c.RequiredOnCI(runtime.GOOS) {
			profileSkipped++
			continue
		}
		stats.selected++
		selected = append(selected, selfcheckEvalCase{file: file, caseDef: c})
	}
	if len(selected) == 0 {
		fmt.Fprintf(a.stdout, "  UNAVAILABLE profile %s selected no cases for %s (%d excluded)\n", profile, runtime.GOOS, profileSkipped)
		return selfcheckUnavailable
	}
	recordFiles, _ := filepath.Glob(filepath.Join(vcrDir, "*", "*.json"))
	fmt.Fprintf(a.stdout, "  corpus: %d valid cases, %d recorded sessions, %d cassette records, %d selected\n", loadedCount, recordedSessions, len(recordFiles), len(selected))
	maxCaseSeconds := 0
	if v := strings.TrimSpace(os.Getenv("SELFMIND_EVAL_MAX_CASE_SECONDS")); v != "" {
		n, parseErr := strconv.Atoi(v)
		if parseErr != nil || n < 0 {
			fmt.Fprintf(a.stdout, "  UNAVAILABLE invalid SELFMIND_EVAL_MAX_CASE_SECONDS=%q (expected a non-negative integer)\n", v)
			return selfcheckUnavailable
		}
		maxCaseSeconds = n
	}

	tooLong, slowSkipped := 0, 0
	runnable := make([]selfcheckEvalCase, 0, len(selected))
	for _, item := range selected {
		c := item.caseDef
		stats := suites[selfcheckCaseSuite(c, item.file)]
		if c.RequiresModel() && !llm.HasCassetteSession(vcrDir, c.ID) {
			failed++
			stats.missing = append(stats.missing, c.ID)
			fmt.Fprintf(a.stdout, "  FAIL %s (model-backed release case has no cassette; run `SELFMIND_EVAL_VCR=record selfmind eval run %s --live` and commit .vcr/%s/)\n", c.ID, item.file, c.ID)
			continue
		}
		if profile != selfcheckCI && maxCaseSeconds > 0 && c.Expect.MaxDurationSeconds > maxCaseSeconds && !c.RequireCassette {
			tooLong++
			continue
		}
		if profile == selfcheckLocalFast && c.Slow && !c.RequireCassette {
			slowSkipped++
			continue
		}
		runnable = append(runnable, item)
	}

	// Replay runs the product in this process, and replayed tool calls run on
	// the host: both get a throwaway HOME so that neither first-run state nor a
	// tool's own files reach the operator's per-user directories. Go's caches
	// and settings are resolved for the real environment first, as for the
	// build and test children.
	home, err := scratchhome.New("selfmind-selfcheck-eval-")
	if err != nil {
		fmt.Fprintf(a.stdout, "  UNAVAILABLE cannot create a throwaway HOME for replay: %v\n", err)
		return selfcheckUnavailable
	}
	defer func() { _ = scratchhome.Remove(home) }()
	var goDirs []string
	if goBin, lookErr := exec.LookPath("go"); lookErr == nil {
		if goDirs, err = scratchhome.GoDirs(a.ctx, goBin, os.Environ()); err != nil {
			fmt.Fprintf(a.stdout, "  UNAVAILABLE cannot resolve Go cache locations: %v\n", err)
			return selfcheckUnavailable
		}
	}
	scoped := map[string]string{
		"PATH":                  selfcheckEvalPath(os.Getenv("PATH")),
		"SELFMIND_EVAL_VCR":     "replay",
		"SELFMIND_EVAL_OFFLINE": "1",
		"SELFMIND_EVAL_VCR_DIR": vcrDir,
	}
	for _, assignment := range append(scratchhome.Env(home), goDirs...) {
		key, value, _ := strings.Cut(assignment, "=")
		scoped[key] = value
	}
	restore := setEnvScoped(scoped)
	defer restore()

	if runtime.GOOS == "linux" && isWSL() {
		fmt.Fprintln(a.stdout, "  environment: WSL detected; Windows PATH entries are excluded from replayed tool calls")
	}
	var deferred int
	var unavailable bool
	runnable, deferred, unavailable = a.selfcheckToolRequirements(runnable, profile, suites)
	for _, item := range runnable {
		suites[selfcheckCaseSuite(item.caseDef, item.file)].runnable++
	}
	printSelfcheckSuiteStats(a.stdout, suites)
	if unavailable {
		return selfcheckUnavailable
	}

	passed, executed, missingRequirements := 0, 0, 0
	for _, item := range runnable {
		c := item.caseDef
		if profile == selfcheckCI {
			fmt.Fprintf(a.stdout, "  run  %s (ci:%s)\n", c.ID, c.CI.Reason)
		}
		result, runErr := selfeval.RunCaseFile(a.ctx, item.file, selfeval.RunOptions{ConfigPath: configPath})
		if errors.Is(runErr, selfeval.ErrRequirementUnavailable) {
			// The environment could not provide what the case needs, as with a
			// missing tool: nothing about the product was verified or refuted.
			if profile != selfcheckCI && c.RequiredOnCI(runtime.GOOS) {
				deferred++
				suites[selfcheckCaseSuite(c, item.file)].deferred++
				fmt.Fprintf(a.stdout, "  CI-DEFERRED %s (%v; owner=ci:%s)\n", c.ID, runErr, c.CI.Reason)
				continue
			}
			missingRequirements++
			fmt.Fprintf(a.stdout, "  UNAVAILABLE %s (%v)\n", c.ID, runErr)
			continue
		}
		executed++
		if runErr != nil {
			failed++
			fmt.Fprintf(a.stdout, "  FAIL %s (%v)\n", c.ID, runErr)
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
	summary := fmt.Sprintf("  summary: %d passed, %d failed, %d executed, %d delegated to CI, %d excluded by profile", passed, failed, executed, deferred, profileSkipped)
	if missingRequirements > 0 {
		summary += fmt.Sprintf(", %d unavailable", missingRequirements)
	}
	fmt.Fprintln(a.stdout, summary)
	if deferred > 0 {
		fmt.Fprintln(a.stdout, "  note: CI-DEFERRED cases are not locally verified; release requires the matching GitHub Actions jobs to pass")
	}
	if slowSkipped > 0 {
		fmt.Fprintf(a.stdout, "  note: %d slow case(s) skipped by --fast/local-fast; run `selfmind selfcheck` before pushing\n", slowSkipped)
	}
	if tooLong > 0 {
		fmt.Fprintf(a.stdout, "  note: %d case(s) skipped by SELFMIND_EVAL_MAX_CASE_SECONDS=%d\n", tooLong, maxCaseSeconds)
	}
	if raw := strings.TrimSpace(os.Getenv("SELFMIND_EVAL_MIN_CASES")); raw != "" {
		min, parseErr := strconv.Atoi(raw)
		if parseErr != nil || min < 0 {
			fmt.Fprintf(a.stdout, "  UNAVAILABLE invalid SELFMIND_EVAL_MIN_CASES=%q (expected a non-negative integer)\n", raw)
			return selfcheckUnavailable
		}
		if executed < min {
			fmt.Fprintf(a.stdout, "  FAIL eval gate executed %d case(s) but SELFMIND_EVAL_MIN_CASES=%d requires at least %d\n", executed, min, min)
			return selfcheckFailed
		}
	}
	if missingRequirements > 0 {
		return selfcheckUnavailable
	}
	if executed == 0 && failed == 0 {
		fmt.Fprintln(a.stdout, "  UNAVAILABLE selected profile executed no cases")
		return selfcheckUnavailable
	}
	if failed > 0 {
		return selfcheckFailed
	}
	return selfcheckPassed
}

// selfcheckEvalConfig picks the configuration replay runs with, resolved
// against the real environment before replay moves HOME: the explicit or
// default config file when it exists. When it does not, replay runs on the
// built-in defaults that a first run writes inside the throwaway HOME, so
// selfcheck never creates the person's config.
func (a *App) selfcheckEvalConfig() (path, note string) {
	resolved, _ := config.ResolveConfigPath(a.configPath)
	if _, err := os.Stat(resolved); errors.Is(err, fs.ErrNotExist) {
		return "", "built-in defaults (no file at " + resolved + ")"
	}
	return resolved, resolved
}

func selfcheckCaseSuite(c *selfeval.Case, file string) string {
	if c != nil && strings.TrimSpace(c.Suite) != "" {
		return strings.TrimSpace(c.Suite)
	}
	if dir := filepath.Base(filepath.Dir(file)); dir != "." && dir != "" {
		return dir
	}
	return "default"
}

func printSelfcheckSuiteStats(w io.Writer, suites map[string]*selfcheckSuiteStats) {
	if len(suites) == 0 {
		return
	}
	names := make([]string, 0, len(suites))
	for name := range suites {
		names = append(names, name)
	}
	sort.Strings(names)
	fmt.Fprintln(w, "  suites:")
	for _, name := range names {
		stats := suites[name]
		fmt.Fprintf(w, "    %-20s valid=%d recorded=%d providerless=%d selected=%d runnable=%d deferred=%d missing=%d\n",
			name, stats.valid, stats.recorded, stats.providerless, stats.selected, stats.runnable, stats.deferred, len(stats.missing))
		if len(stats.missing) > 0 {
			sort.Strings(stats.missing)
			fmt.Fprintf(w, "      missing: %s\n", strings.Join(stats.missing, ", "))
		}
	}
}

func resolveSelfcheckEvalRoot(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		root = "evalcases"
	}
	if !filepath.IsAbs(root) {
		repo := repoRoot()
		if repo == "" {
			return "", fmt.Errorf("cannot resolve relative eval path %q: go.mod not found from cwd upward", root)
		}
		root = filepath.Join(repo, root)
	}
	root = filepath.Clean(root)
	if _, err := os.Stat(root); err != nil {
		return "", fmt.Errorf("eval path %s is unavailable: %w", root, err)
	}
	return root, nil
}

func selfcheckEvalPath(pathValue string) string {
	if runtime.GOOS != "linux" {
		return pathValue
	}
	parts := filepath.SplitList(pathValue)
	filtered := parts[:0]
	for _, part := range parts {
		clean := strings.ToLower(filepath.ToSlash(strings.TrimSpace(part)))
		if clean == "/mnt/c" || strings.HasPrefix(clean, "/mnt/c/") {
			continue
		}
		filtered = append(filtered, part)
	}
	return strings.Join(filtered, string(os.PathListSeparator))
}

func isWSL() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	data, err := os.ReadFile("/proc/sys/kernel/osrelease")
	return err == nil && strings.Contains(strings.ToLower(string(data)), "microsoft")
}

func incompatibleLinuxExecutable(path string) bool {
	if runtime.GOOS != "linux" {
		return false
	}
	lower := strings.ToLower(filepath.ToSlash(strings.TrimSpace(path)))
	return strings.HasSuffix(lower, ".exe") || strings.HasSuffix(lower, ".cmd") || strings.HasSuffix(lower, ".bat") ||
		lower == "/mnt/c" || strings.HasPrefix(lower, "/mnt/c/")
}

func (a *App) selfcheckToolRequirements(
	cases []selfcheckEvalCase,
	profile selfcheckProfile,
	suites map[string]*selfcheckSuiteStats,
) (runnable []selfcheckEvalCase, deferred int, unavailable bool) {
	commands := make(map[string][]string)
	for _, item := range cases {
		for _, command := range item.caseDef.Requires.Commands {
			commands[command] = append(commands[command], item.caseDef.ID)
		}
	}
	if len(commands) == 0 {
		return cases, 0, false
	}
	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	sort.Strings(names)
	missingCommands := make(map[string]bool)
	for _, name := range names {
		path, err := exec.LookPath(name)
		if err != nil || incompatibleLinuxExecutable(path) {
			missingCommands[name] = true
			continue
		}
		version := selfcheckCommandVersion(a.ctx, path)
		if version != "" {
			fmt.Fprintf(a.stdout, "  tool %s: %s (%s)\n", name, path, version)
		} else {
			fmt.Fprintf(a.stdout, "  tool %s: %s\n", name, path)
		}
	}
	if len(missingCommands) == 0 {
		return cases, 0, false
	}

	runnable = make([]selfcheckEvalCase, 0, len(cases))
	for _, item := range cases {
		missing := make([]string, 0, len(item.caseDef.Requires.Commands))
		for _, command := range item.caseDef.Requires.Commands {
			if missingCommands[command] {
				missing = append(missing, command)
			}
		}
		if len(missing) == 0 {
			runnable = append(runnable, item)
			continue
		}
		sort.Strings(missing)
		if profile != selfcheckCI && item.caseDef.RequiredOnCI(runtime.GOOS) {
			deferred++
			suites[selfcheckCaseSuite(item.caseDef, item.file)].deferred++
			fmt.Fprintf(a.stdout, "  CI-DEFERRED %s (missing native %s tool(s): %s; owner=ci:%s)\n",
				item.caseDef.ID, runtime.GOOS, strings.Join(missing, ", "), item.caseDef.CI.Reason)
			continue
		}
		unavailable = true
		fmt.Fprintf(a.stdout, "  UNAVAILABLE %s requires native %s tool(s): %s\n",
			item.caseDef.ID, runtime.GOOS, strings.Join(missing, ", "))
	}
	return runnable, deferred, unavailable
}

func selfcheckCommandVersion(parent context.Context, path string) string {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(out))
	if before, _, ok := strings.Cut(line, "\n"); ok {
		line = strings.TrimSpace(before)
	}
	if len(line) > 120 {
		line = line[:120]
	}
	return line
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
