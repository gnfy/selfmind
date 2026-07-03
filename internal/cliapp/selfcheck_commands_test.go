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

func TestSelfcheckEvalFailsOnMissingRequiredCassette(t *testing.T) {
	dir := t.TempDir()
	writeSelfcheckCase(t, dir, "required_case", true)
	t.Setenv("SELFMIND_EVAL_VCR_DIR", filepath.Join(t.TempDir(), "vcr"))
	t.Setenv("SELFMIND_EVAL_MIN_CASES", "")

	app, out := newSelfcheckTestApp()
	if app.selfcheckEval(dir) {
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
	if !app.selfcheckEval(dir) {
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
	if app.selfcheckEval(dir) {
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
	if !app.selfcheckEval(dir) {
		t.Fatalf("selfcheckEval with SELFMIND_EVAL_MIN_CASES unset must keep the skip-only default; output:\n%s", out.String())
	}
}

func TestSelfcheckEvalMinCasesRejectsInvalidValue(t *testing.T) {
	dir := t.TempDir()
	writeSelfcheckCase(t, dir, "optional_case", false)
	t.Setenv("SELFMIND_EVAL_VCR_DIR", filepath.Join(t.TempDir(), "vcr"))
	t.Setenv("SELFMIND_EVAL_MIN_CASES", "three")

	app, out := newSelfcheckTestApp()
	if app.selfcheckEval(dir) {
		t.Fatalf("selfcheckEval should fail on an unparseable SELFMIND_EVAL_MIN_CASES; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "invalid SELFMIND_EVAL_MIN_CASES") {
		t.Fatalf("failure output should flag the invalid threshold; got:\n%s", out.String())
	}
}
