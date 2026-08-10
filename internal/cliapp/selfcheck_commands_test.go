package cliapp

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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
	if app.selfcheckEval(dir, false) {
		t.Fatalf("selfcheckEval should fail when a require_cassette case has no cassette; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "require_cassette") {
		t.Fatalf("failure output should explain the missing required cassette; got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "required_case") {
		t.Fatalf("failure output should name the failing case; got:\n%s", out.String())
	}
}

func TestSelfcheckEvalSkipsOptionalMissingCassette(t *testing.T) {
	dir := t.TempDir()
	writeSelfcheckCase(t, dir, "optional_case", false)
	t.Setenv("SELFMIND_EVAL_VCR_DIR", filepath.Join(t.TempDir(), "vcr"))
	t.Setenv("SELFMIND_EVAL_MIN_CASES", "")

	app, out := newSelfcheckTestApp()
	if !app.selfcheckEval(dir, false) {
		t.Fatalf("selfcheckEval should pass when an optional case merely lacks a cassette; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "1 skipped") {
		t.Fatalf("summary should count the skipped case; got:\n%s", out.String())
	}
}

func TestSelfcheckEvalMinCasesFailsWhenBelowThreshold(t *testing.T) {
	dir := t.TempDir()
	writeSelfcheckCase(t, dir, "optional_case", false)
	t.Setenv("SELFMIND_EVAL_VCR_DIR", filepath.Join(t.TempDir(), "vcr"))
	t.Setenv("SELFMIND_EVAL_MIN_CASES", "2")

	app, out := newSelfcheckTestApp()
	if app.selfcheckEval(dir, false) {
		t.Fatalf("selfcheckEval should fail when fewer cases replay than SELFMIND_EVAL_MIN_CASES; output:\n%s", out.String())
	}
	// The failure message must print actual vs required so CI logs are actionable.
	if !strings.Contains(out.String(), "replayed 0 case(s)") || !strings.Contains(out.String(), "SELFMIND_EVAL_MIN_CASES=2") {
		t.Fatalf("failure output should print actual vs required counts; got:\n%s", out.String())
	}
}

func TestSelfcheckEvalMinCasesUnsetKeepsSkipBehavior(t *testing.T) {
	dir := t.TempDir()
	writeSelfcheckCase(t, dir, "optional_case", false)
	t.Setenv("SELFMIND_EVAL_VCR_DIR", filepath.Join(t.TempDir(), "vcr"))
	t.Setenv("SELFMIND_EVAL_MIN_CASES", "")

	app, out := newSelfcheckTestApp()
	if !app.selfcheckEval(dir, false) {
		t.Fatalf("selfcheckEval with SELFMIND_EVAL_MIN_CASES unset must keep the skip-only default; output:\n%s", out.String())
	}
}

func TestSelfcheckEvalMinCasesRejectsInvalidValue(t *testing.T) {
	dir := t.TempDir()
	writeSelfcheckCase(t, dir, "optional_case", false)
	t.Setenv("SELFMIND_EVAL_VCR_DIR", filepath.Join(t.TempDir(), "vcr"))
	t.Setenv("SELFMIND_EVAL_MIN_CASES", "three")

	app, out := newSelfcheckTestApp()
	if app.selfcheckEval(dir, false) {
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
	ok := app.selfcheckEval(dir, true)
	text := out.String()

	if !strings.Contains(text, "1 slow case(s) skipped by --fast") {
		t.Fatalf("the skipped count must be reported; got:\n%s", text)
	}
	// The mandatory case still ran, found no cassette, and failed the gate.
	if ok || !strings.Contains(text, "require_cassette") {
		t.Fatalf("a mandatory case must not be dropped by --fast; ok=%v output:\n%s", ok, text)
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
