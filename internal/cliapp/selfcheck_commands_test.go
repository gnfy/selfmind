package cliapp

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
	})
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
  - input: "/tasks all"
expect:
  status: completed
  max_tool_calls: 0
  contains:
    - "All tasks"
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
