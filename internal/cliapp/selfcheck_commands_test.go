package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// writeSelfcheckCase drops a minimal eval case YAML under dir and returns its path.
func writeSelfcheckCase(t *testing.T, dir, id string, requireCassette bool) string {
	t.Helper()
	content := "id: " + id + "\n" +
		"title: \"selfcheck test case\"\n" +
		"suite: selfcheck-test\n" +
		"channel: cli\n"
	if requireCassette {
		content += "require_cassette: true\n"
	}
	content += "turns:\n  - input: \"hello\"\n"
	path := filepath.Join(dir, id+".yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write case: %v", err)
	}
	return path
}

func newSelfcheckTestApp() (*App, *bytes.Buffer) {
	out := &bytes.Buffer{}
	return &App{ctx: context.Background(), stdout: out, stderr: out}, out
}

func TestSelfcheckGoEnvRemovesRecorderWrappers(t *testing.T) {
	env := selfcheckGoEnv([]string{
		"PATH=/usr/bin",
		"SELFMIND_EVAL_VCR=replay",
		"SELFMIND_EVAL_OFFLINE=1",
		"SELFMIND_EVAL_VCR_DIR=/tmp/vcr",
		"SELFMIND_FLIGHT_RECORDER=1",
		"SELFMIND_FLIGHT_DIR=/tmp/flight",
		"SELFMIND_FLIGHT_KEEP=50",
		"GOWORK=/tmp/go.work",
		"SELFMIND_EVAL_MIN_CASES=12",
	}, "/scratch/home", nil)
	joined := strings.Join(env, "\n")
	for _, forbidden := range []string{
		"SELFMIND_EVAL_VCR=",
		"SELFMIND_EVAL_OFFLINE=",
		"SELFMIND_EVAL_VCR_DIR=",
		"SELFMIND_FLIGHT_RECORDER=",
		"SELFMIND_FLIGHT_DIR=",
		"SELFMIND_FLIGHT_KEEP=",
		"GOWORK=/tmp/go.work",
	} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("selfcheckGoEnv retained %q in:\n%s", forbidden, joined)
		}
	}
	for _, required := range []string{"PATH=/usr/bin", "SELFMIND_EVAL_MIN_CASES=12", "GOWORK=off"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("selfcheckGoEnv dropped %q from:\n%s", required, joined)
		}
	}
}

// envValues maps an environment list to its effective values: the last
// assignment of a key wins, as it does for exec.Cmd.
func envValues(t *testing.T, env []string) map[string]string {
	t.Helper()
	values := map[string]string{}
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("malformed environment entry %q", entry)
		}
		values[key] = value
	}
	return values
}

// Build and test children get a throwaway HOME for every per-user location,
// including the config file, while Go keeps the caches `go env` found for the
// original environment and GOFLAGS passes through as before.
func TestSelfcheckGoEnvMovesPerUserStateAndKeepsGoCaches(t *testing.T) {
	for _, home := range []string{"/scratch/home", "/var/tmp/selfmind-selfcheck-501-0123456789ab"} {
		env := selfcheckGoEnv([]string{
			"PATH=/usr/bin",
			"HOME=/Users/dev",
			"XDG_CACHE_HOME=/Users/dev/.cache",
			"XDG_CONFIG_HOME=/Users/dev/.config",
			"SELF_CONFIG=/Users/dev/.selfmind/config.yaml",
			"GOCACHE=/stale/cache",
			"GOFLAGS=-mod=mod",
		}, home, []string{
			"GOCACHE=/Users/dev/Library/Caches/go-build",
			"GOMODCACHE=/Users/dev/go/pkg/mod",
			"GOPATH=/Users/dev/go",
			"GOENV=/Users/dev/Library/Application Support/go/env",
		})
		joined := strings.Join(env, "\n")
		for _, key := range []string{"HOME", "XDG_CACHE_HOME", "SELF_CONFIG", "GOCACHE"} {
			if count := strings.Count("\n"+joined, "\n"+key+"="); count != 1 {
				t.Fatalf("%s appears %d times in:\n%s", key, count, joined)
			}
		}
		values := envValues(t, env)
		want := map[string]string{
			"HOME":            home,
			"XDG_CONFIG_HOME": filepath.Join(home, ".config"),
			"XDG_CACHE_HOME":  filepath.Join(home, ".cache"),
			"XDG_DATA_HOME":   filepath.Join(home, ".local", "share"),
			"XDG_STATE_HOME":  filepath.Join(home, ".local", "state"),
			"SELF_CONFIG":     "",
			"GOCACHE":         "/Users/dev/Library/Caches/go-build",
			"GOMODCACHE":      "/Users/dev/go/pkg/mod",
			"GOPATH":          "/Users/dev/go",
			"GOENV":           "/Users/dev/Library/Application Support/go/env",
			"GOFLAGS":         "-mod=mod",
			"GOWORK":          "off",
			"PATH":            "/usr/bin",
		}
		for key, value := range want {
			if got, ok := values[key]; !ok || got != value {
				t.Fatalf("%s = %q (set=%v); want %q in:\n%s", key, got, ok, value, joined)
			}
		}
	}
}

// writeHomeFixture writes a standalone module whose only test runs body, and
// returns its root. selfcheck's Go phase runs against it as against the
// repository.
func writeHomeFixture(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"go.mod":     "module homefixture\n\ngo 1.21\n",
		"fixture.go": "package homefixture\n",
		"fixture_test.go": `package homefixture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var (
	_ = json.Marshal
	_ = filepath.Join
	_ = fmt.Sprint
	_ = time.Now
)

func write(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFixture(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = home, cache
` + body + `
}
`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func requireGoTool(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and tests a fixture module with the Go tool")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool unavailable")
	}
	return goBin
}

// The children see a throwaway HOME, not the caller's, and no SELF_CONFIG
// pointing back at the operator's file; Go keeps the caches `go env` reports
// for the caller's environment. A non-SelfMind file under that HOME is none
// of the guard's business.
func TestSelfcheckGoRunsTestsAgainstAThrowawayHome(t *testing.T) {
	goBin := requireGoTool(t)
	report := filepath.Join(t.TempDir(), "child-env.json")
	gopath := filepath.Join(t.TempDir(), "gopath-of-the-caller")
	t.Setenv("HOMEFIXTURE_REPORT", report)
	t.Setenv("SELF_CONFIG", filepath.Join(os.Getenv("HOME"), ".selfmind", "config.yaml"))
	t.Setenv("GOPATH", gopath)
	t.Setenv("GOMODCACHE", "")
	root := writeHomeFixture(t, `
	write(t, filepath.Join(home, ".config", "othertool", "state"))
	seen := map[string]string{}
	for _, key := range []string{"HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "SELF_CONFIG", "GOCACHE", "GOMODCACHE", "GOPATH", "GOENV", "GOWORK"} {
		seen[key] = os.Getenv(key)
	}
	data, _ := json.Marshal(seen)
	if err := os.WriteFile(os.Getenv("HOMEFIXTURE_REPORT"), data, 0o600); err != nil {
		t.Fatal(err)
	}`)

	app, out := newSelfcheckTestApp()
	if got := app.selfcheckGoIn(goBin, root); got != selfcheckPassed {
		t.Fatalf("fixture that writes no SelfMind state must pass; got=%v output:\n%s", got, out.String())
	}
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("fixture test did not run: %v\n%s", err, out.String())
	}
	var seen map[string]string
	if err := json.Unmarshal(data, &seen); err != nil {
		t.Fatal(err)
	}
	childHome := seen["HOME"]
	if childHome == "" || childHome == os.Getenv("HOME") || !strings.HasPrefix(filepath.Base(childHome), "selfmind-selfcheck-") {
		t.Fatalf("child HOME = %q; want a throwaway selfcheck home, not %q", childHome, os.Getenv("HOME"))
	}
	if seen["XDG_CACHE_HOME"] != filepath.Join(childHome, ".cache") || seen["XDG_CONFIG_HOME"] != filepath.Join(childHome, ".config") {
		t.Fatalf("child XDG directories escape its HOME: %v", seen)
	}
	if seen["SELF_CONFIG"] != "" {
		t.Fatalf("child SELF_CONFIG = %q; the operator's config must not reach tests", seen["SELF_CONFIG"])
	}
	if seen["GOPATH"] != gopath || seen["GOMODCACHE"] != filepath.Join(gopath, "pkg", "mod") {
		t.Fatalf("child GOPATH/GOMODCACHE = %q/%q; want the caller's %q", seen["GOPATH"], seen["GOMODCACHE"], gopath)
	}
	for _, key := range []string{"GOCACHE", "GOENV"} {
		if seen[key] == "" || strings.HasPrefix(seen[key], childHome) {
			t.Fatalf("child %s = %q; want the caller's location, outside %s", key, seen[key], childHome)
		}
	}
	if seen["GOWORK"] != "off" {
		t.Fatalf("child GOWORK = %q; want off", seen["GOWORK"])
	}
	if _, err := os.Lstat(childHome); !os.IsNotExist(err) {
		t.Fatalf("throwaway HOME %s survived the run (err=%v)", childHome, err)
	}
}

// waitForEntries waits until dir holds at least n entries.
func waitForEntries(t *testing.T, dir string, n int) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if entries, err := os.ReadDir(dir); err == nil && len(entries) >= n {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// Two selfchecks of one checkout at the same time both finish on their own
// merits: the second does not clear the HOME the first is running in, and it
// says why its own run cannot reuse Go's test cache.
func TestSelfcheckGoConcurrentRunsOfOneCheckoutKeepTheirHomes(t *testing.T) {
	goBin := requireGoTool(t)
	started := t.TempDir()
	seen := t.TempDir()
	release := filepath.Join(t.TempDir(), "release")
	t.Setenv("HOMEFIXTURE_STARTED", started)
	t.Setenv("HOMEFIXTURE_SEEN", seen)
	t.Setenv("HOMEFIXTURE_RELEASE", release)
	root := writeHomeFixture(t, `
	alive := filepath.Join(home, ".config", "fixture", fmt.Sprint(os.Getpid()))
	write(t, alive)
	write(t, filepath.Join(os.Getenv("HOMEFIXTURE_STARTED"), fmt.Sprint(os.Getpid())))
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(os.Getenv("HOMEFIXTURE_RELEASE")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never released")
		}
	}
	if _, err := os.Stat(alive); err != nil {
		t.Fatalf("HOME %s was cleared under this run: %v", home, err)
	}
	write(t, filepath.Join(os.Getenv("HOMEFIXTURE_SEEN"), filepath.Base(home)))`)

	type result struct {
		outcome selfcheckOutcome
		output  string
	}
	results := make(chan result, 2)
	run := func() {
		app, out := newSelfcheckTestApp()
		outcome := app.selfcheckGoIn(goBin, root)
		results <- result{outcome, out.String()}
	}
	go run()
	firstStarted := waitForEntries(t, started, 1)
	go run()
	bothStarted := firstStarted && waitForEntries(t, started, 2)
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var outputs []string
	failed := !bothStarted
	for i := 0; i < 2; i++ {
		got := <-results
		outputs = append(outputs, got.output)
		failed = failed || got.outcome != selfcheckPassed
	}
	joined := strings.Join(outputs, "\n----\n")
	if failed {
		t.Fatalf("both concurrent runs must pass (both started: %v):\n%s", bothStarted, joined)
	}
	if count := strings.Count(joined, "another selfcheck is running for this checkout"); count != 1 {
		t.Fatalf("exactly one run must say it shares the checkout, got %d:\n%s", count, joined)
	}
	if homes, err := os.ReadDir(seen); err != nil || len(homes) != 2 {
		t.Fatalf("the two runs must use two homes: %v %v", homes, err)
	}
}

// A test that leaves SelfMind state under HOME fails the gate, and the
// failure names each path, whether it is the data tree, another .selfmind*
// tree, or the per-user cache.
func TestSelfcheckGoFailsWhenTestsLeaveSelfMindStateInHome(t *testing.T) {
	goBin := requireGoTool(t)
	root := writeHomeFixture(t, `
	write(t, filepath.Join(home, ".selfmind", "fixture", "state.json"))
	write(t, filepath.Join(home, ".selfmind-fixture-runtime", "lease"))
	write(t, filepath.Join(cache, "selfmind", "fixture", "run.db"))`)

	app, out := newSelfcheckTestApp()
	got := app.selfcheckGoIn(goBin, root)
	text := out.String()
	if got != selfcheckFailed {
		t.Fatalf("tests that write SelfMind state under HOME must fail selfcheck; got=%v output:\n%s", got, text)
	}
	cacheRoot := ".cache/selfmind"
	if runtime.GOOS == "darwin" {
		cacheRoot = "Library/Caches/selfmind"
	}
	for _, path := range []string{".selfmind/fixture", ".selfmind-fixture-runtime/lease", cacheRoot + "/fixture", "testhome.Main"} {
		if !strings.Contains(text, path) {
			t.Fatalf("failure output must name %q:\n%s", path, text)
		}
	}
	if strings.Contains(text, "FAIL go test ./...") {
		t.Fatalf("the fixture's own test passed; only the guard may fail:\n%s", text)
	}
}

func TestSelfcheckEvalFailsOnMissingRequiredCassette(t *testing.T) {
	dir := t.TempDir()
	writeSelfcheckCase(t, dir, "required_case", true)
	t.Setenv("SELFMIND_EVAL_VCR_DIR", filepath.Join(t.TempDir(), "vcr"))
	t.Setenv("SELFMIND_EVAL_MIN_CASES", "")

	app, out := newSelfcheckTestApp()
	if got := app.selfcheckEval(dir, selfcheckLocalFull); got != selfcheckFailed {
		t.Fatalf("selfcheckEval should fail when a require_cassette case has no cassette; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "model-backed release case has no cassette") {
		t.Fatalf("failure output should explain the missing required cassette; got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "required_case") {
		t.Fatalf("failure output should name the failing case; got:\n%s", out.String())
	}
}

func TestSelfcheckEvalFailsOnAnyMissingModelCassette(t *testing.T) {
	dir := t.TempDir()
	writeSelfcheckCase(t, dir, "optional_case", false)
	t.Setenv("SELFMIND_EVAL_VCR_DIR", filepath.Join(t.TempDir(), "vcr"))
	t.Setenv("SELFMIND_EVAL_MIN_CASES", "")

	app, out := newSelfcheckTestApp()
	if got := app.selfcheckEval(dir, selfcheckLocalFull); got != selfcheckFailed {
		t.Fatalf("selfcheckEval should fail when any model-backed release case lacks a cassette; got=%v output:\n%s", got, out.String())
	}
	if !strings.Contains(out.String(), "missing=1") || !strings.Contains(out.String(), "missing: optional_case") {
		t.Fatalf("suite report should expose the missing release evidence; got:\n%s", out.String())
	}
}

func TestSelfcheckEvalMinCasesFailsWhenBelowThreshold(t *testing.T) {
	dir := t.TempDir()
	writeSelfcheckCase(t, dir, "optional_case", false)
	t.Setenv("SELFMIND_EVAL_VCR_DIR", filepath.Join(t.TempDir(), "vcr"))
	t.Setenv("SELFMIND_EVAL_MIN_CASES", "2")

	app, out := newSelfcheckTestApp()
	if got := app.selfcheckEval(dir, selfcheckLocalFull); got != selfcheckFailed {
		t.Fatalf("selfcheckEval should fail when fewer cases replay than SELFMIND_EVAL_MIN_CASES; output:\n%s", out.String())
	}
	// The failure message must print actual vs required so CI logs are actionable.
	if !strings.Contains(out.String(), "executed 0 case(s)") || !strings.Contains(out.String(), "SELFMIND_EVAL_MIN_CASES=2") {
		t.Fatalf("failure output should print actual vs required counts; got:\n%s", out.String())
	}
}

func TestSelfcheckEvalMinCasesUnsetStillFailsMissingEvidence(t *testing.T) {
	dir := t.TempDir()
	writeSelfcheckCase(t, dir, "optional_case", false)
	t.Setenv("SELFMIND_EVAL_VCR_DIR", filepath.Join(t.TempDir(), "vcr"))
	t.Setenv("SELFMIND_EVAL_MIN_CASES", "")

	app, out := newSelfcheckTestApp()
	if got := app.selfcheckEval(dir, selfcheckLocalFull); got != selfcheckFailed {
		t.Fatalf("selfcheckEval must fail when release evidence is missing; got=%v output:\n%s", got, out.String())
	}
}

func TestSelfcheckEvalRunsProviderlessCaseWithoutCassette(t *testing.T) {
	dir := t.TempDir()
	content := `id: providerless_control
title: providerless control flow
suite: selfcheck-test
workspace: isolated
channel: cli
model_required: false
turns:
  - input: "/new providerless control check"
  - input: "/resume"
expect:
  status: completed
  max_tool_calls: 0
  contains:
    - "providerless control check"
`
	if err := os.WriteFile(filepath.Join(dir, "providerless.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SELFMIND_EVAL_VCR_DIR", filepath.Join(t.TempDir(), "vcr"))
	t.Setenv("SELFMIND_EVAL_MIN_CASES", "")

	app, out := newSelfcheckTestApp()
	if got := app.selfcheckEval(dir, selfcheckLocalFull); got != selfcheckPassed {
		t.Fatalf("providerless case should run without a cassette; got=%v output:\n%s", got, out.String())
	}
	if !strings.Contains(out.String(), "providerless=1") || !strings.Contains(out.String(), "1 passed") {
		t.Fatalf("providerless execution should be visible in coverage output:\n%s", out.String())
	}
}

func TestSelfcheckEvalMinCasesRejectsInvalidValue(t *testing.T) {
	dir := t.TempDir()
	writeSelfcheckCase(t, dir, "optional_case", false)
	t.Setenv("SELFMIND_EVAL_VCR_DIR", filepath.Join(t.TempDir(), "vcr"))
	t.Setenv("SELFMIND_EVAL_MIN_CASES", "three")

	app, out := newSelfcheckTestApp()
	if got := app.selfcheckEval(dir, selfcheckLocalFull); got != selfcheckUnavailable {
		t.Fatalf("selfcheckEval should fail on an unparseable SELFMIND_EVAL_MIN_CASES; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "invalid SELFMIND_EVAL_MIN_CASES") {
		t.Fatalf("failure output should flag the invalid threshold; got:\n%s", out.String())
	}
}

// --fast exists so the loop after each change stays in the tens of seconds:
// four cases carry roughly 90% of the replay cost. It must report what it
// dropped — a quiet cap reads as full coverage — and it must never drop a
// mandatory case, because cost is not a reason to lose the gate.
func TestSelfcheckFastSkipsSlowCasesButReportsAndKeepsMandatory(t *testing.T) {
	dir := t.TempDir()
	writeSelfcheckSlowCase(t, dir, "slow_optional", false)
	writeSelfcheckSlowCase(t, dir, "slow_mandatory", true)
	vcr := filepath.Join(t.TempDir(), "vcr")
	// The optional case needs a cassette: without one it is reported as
	// "no cassette", which is the more actionable state and is checked first.
	// --fast never replays it, so the content does not matter.
	if err := os.MkdirAll(filepath.Join(vcr, "slow_optional"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vcr, "slow_optional", "0000.json"), []byte(`{"method":"stream"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SELFMIND_EVAL_VCR_DIR", vcr)
	t.Setenv("SELFMIND_EVAL_MIN_CASES", "")

	app, out := newSelfcheckTestApp()
	outcome := app.selfcheckEval(dir, selfcheckLocalFast)
	text := out.String()

	if !strings.Contains(text, "1 slow case(s) skipped by --fast") {
		t.Fatalf("the skipped count must be reported; got:\n%s", text)
	}
	// The mandatory case still ran, found no cassette, and failed the gate.
	if outcome != selfcheckFailed || !strings.Contains(text, "model-backed release case has no cassette") {
		t.Fatalf("a mandatory case must not be dropped by --fast; outcome=%v output:\n%s", outcome, text)
	}
}

func TestSelfcheckEvalMissingDirectoryIsUnavailable(t *testing.T) {
	t.Setenv("SELFMIND_EVAL_MIN_CASES", "")
	app, out := newSelfcheckTestApp()
	got := app.selfcheckEval(filepath.Join(t.TempDir(), "missing"), selfcheckLocalFull)
	if got != selfcheckUnavailable || !strings.Contains(out.String(), "UNAVAILABLE") {
		t.Fatalf("missing eval directory must be unavailable; got=%v output:\n%s", got, out.String())
	}
}

func TestSelfcheckCISelectsOnlyCurrentPlatformCases(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("CI eval profiles currently target Linux and macOS")
	}
	dir := t.TempDir()
	content := "id: ci_current\ntitle: ci\nsuite: selfcheck-test\nchannel: cli\nci:\n  required: true\n  reason: clean_checkout\n  platforms: [" + runtime.GOOS + "]\nturns:\n  - input: hello\n"
	if err := os.WriteFile(filepath.Join(dir, "ci.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSelfcheckCase(t, dir, "local_only", false)
	t.Setenv("SELFMIND_EVAL_VCR_DIR", filepath.Join(t.TempDir(), "vcr"))
	t.Setenv("SELFMIND_EVAL_MIN_CASES", "")

	app, out := newSelfcheckTestApp()
	got := app.selfcheckEval(dir, selfcheckCI)
	if got != selfcheckFailed {
		t.Fatalf("selected CI case without cassette must fail; got=%v output:\n%s", got, out.String())
	}
	if !strings.Contains(out.String(), "1 excluded by profile") || !strings.Contains(out.String(), "ci_current") {
		t.Fatalf("CI profile must exclude local-only cases and name selected failures; output:\n%s", out.String())
	}
}

func TestSelfcheckLocalDefersMissingToolOnlyWhenCIOwnsTheCase(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("CI eval profiles currently target Linux and macOS")
	}
	dir := t.TempDir()
	content := `id: ci_tool_only
title: CI owns the unavailable tool
suite: selfcheck-test
workspace: isolated
channel: cli
model_required: false
ci:
  required: true
  reason: cross_platform
  platforms: [` + runtime.GOOS + `]
requires:
  commands: [selfmind-command-that-does-not-exist]
turns:
  - input: "/new deferred case"
`
	if err := os.WriteFile(filepath.Join(dir, "ci-tool.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	control := `id: local_control
title: local control
suite: selfcheck-test
workspace: isolated
channel: cli
model_required: false
turns:
  - input: "/new local control"
expect:
  status: completed
`
	if err := os.WriteFile(filepath.Join(dir, "local.yaml"), []byte(control), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SELFMIND_EVAL_VCR_DIR", filepath.Join(t.TempDir(), "vcr"))
	t.Setenv("SELFMIND_EVAL_MIN_CASES", "")

	app, out := newSelfcheckTestApp()
	if got := app.selfcheckEval(dir, selfcheckLocalFull); got != selfcheckPassed {
		t.Fatalf("local gate should delegate an explicitly CI-owned missing tool; got=%v output:\n%s", got, out.String())
	}
	text := out.String()
	if !strings.Contains(text, "CI-DEFERRED ci_tool_only") || !strings.Contains(text, "1 delegated to CI") {
		t.Fatalf("delegated CI ownership must be explicit in output:\n%s", text)
	}
}

func TestSelfcheckLocalMissingToolWithoutCIOwnerIsUnavailable(t *testing.T) {
	dir := t.TempDir()
	content := `id: local_missing_tool
title: local missing tool
suite: selfcheck-test
workspace: isolated
channel: cli
model_required: false
requires:
  commands: [selfmind-command-that-does-not-exist]
turns:
  - input: "/new local unavailable"
`
	if err := os.WriteFile(filepath.Join(dir, "local-missing.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SELFMIND_EVAL_VCR_DIR", filepath.Join(t.TempDir(), "vcr"))
	t.Setenv("SELFMIND_EVAL_MIN_CASES", "")

	app, out := newSelfcheckTestApp()
	if got := app.selfcheckEval(dir, selfcheckLocalFull); got != selfcheckUnavailable {
		t.Fatalf("unowned local tool requirement must fail closed; got=%v output:\n%s", got, out.String())
	}
	if !strings.Contains(out.String(), "UNAVAILABLE local_missing_tool") {
		t.Fatalf("unavailable output should identify the case:\n%s", out.String())
	}
}

// A case whose MCP stub cannot be built offline, here because the module
// cache is empty and downloads are off, is an unavailable requirement with its
// reason rather than a regression.
func TestSelfcheckUnbuildableMCPStubIsUnavailable(t *testing.T) {
	if testing.Short() {
		t.Skip("boots the eval harness and runs the Go tool")
	}
	dir := t.TempDir()
	content := `id: unbuildable_stub
title: unbuildable stub
suite: selfcheck-test
workspace: isolated
model_required: false
requires:
  commands: [go]
runtime:
  mcp_stubs:
    - name: stub
turns:
  - input: "/status"
`
	if err := os.WriteFile(filepath.Join(dir, "unbuildable.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GOMODCACHE", t.TempDir())
	t.Setenv("SELFMIND_EVAL_VCR_DIR", filepath.Join(t.TempDir(), "vcr"))
	t.Setenv("SELFMIND_EVAL_MIN_CASES", "")

	app, out := newSelfcheckTestApp()
	if got := app.selfcheckEval(dir, selfcheckLocalFull); got != selfcheckUnavailable {
		t.Fatalf("an unbuildable stub must be unavailable, not a failure; got=%v output:\n%s", got, out.String())
	}
	text := out.String()
	if !strings.Contains(text, "UNAVAILABLE unbuildable_stub") || !strings.Contains(text, "GOPROXY=off") {
		t.Fatalf("unavailable output should name the case and the reason:\n%s", text)
	}
	if strings.Contains(text, "FAIL unbuildable_stub") {
		t.Fatalf("an unavailable requirement was reported as a failure:\n%s", text)
	}
}

func TestSelfcheckEvalPathDropsWindowsPATHFromLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux-specific PATH policy")
	}
	got := selfcheckEvalPath("/mnt/c/Program Files/nodejs:/usr/local/bin:/usr/bin")
	if strings.Contains(strings.ToLower(got), "/mnt/c/") || !strings.Contains(got, "/usr/bin") {
		t.Fatalf("sanitized PATH = %q", got)
	}
}

func TestSelfcheckGoMissingToolchainIsUnavailable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	app, out := newSelfcheckTestApp()
	if got := app.selfcheckGo(); got != selfcheckUnavailable || !strings.Contains(out.String(), "UNAVAILABLE") {
		t.Fatalf("missing Go must be unavailable; got=%v output:\n%s", got, out.String())
	}
}

func TestSelfcheckAllowsDocsOnly(t *testing.T) {
	app, out := newSelfcheckTestApp()
	app.args = []string{"selfmind", "selfcheck", "--skip-go", "--skip-eval"}
	handled, code := app.runSelfcheckCommandIfRequested()
	if !handled || code != 0 || !strings.Contains(out.String(), "documentation contract") {
		t.Fatalf("docs-only selfcheck should pass; handled=%v code=%d output:\n%s", handled, code, out.String())
	}
}

func writeSelfcheckSlowCase(t *testing.T, dir, id string, requireCassette bool) string {
	t.Helper()
	content := "id: " + id + "\ntitle: \"slow\"\nsuite: selfcheck-test\nchannel: cli\nslow: true\n"
	if requireCassette {
		content += "require_cassette: true\n"
	}
	content += "turns:\n  - input: \"hello\"\n"
	path := filepath.Join(dir, id+".yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write case: %v", err)
	}
	return path
}
