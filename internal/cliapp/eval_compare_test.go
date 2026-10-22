package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	selfeval "selfmind/internal/eval"
)

func TestEvalCompareUsesAllSamplesAndDoesNotBecomeGate(t *testing.T) {
	dir := t.TempDir()
	p := &selfeval.RunProvenance{Mode: "live", Provider: "route", Model: "model", CaseHash: "fixture", PolicyHash: "policy"}
	paths := []string{filepath.Join(dir, "before.json"), filepath.Join(dir, "after.json")}
	for i, path := range paths {
		s := selfeval.ResultSet{Version: 1, Samples: []selfeval.RunResult{{CaseID: "x", Status: "failed", Duration: time.Second * time.Duration(i+1), Provenance: p}}}
		data, _ := json.Marshal(s)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out := &bytes.Buffer{}
	a := &App{ctx: context.Background(), args: append([]string{"selfmind", "eval", "compare"}, paths...), stdout: out, stderr: out}
	handled, code := a.runEvalCommandIfRequested()
	if !handled || code != 0 || !strings.Contains(out.String(), "+100.0%") || !strings.Contains(out.String(), "0/1") {
		t.Fatalf("code=%d output=%s", code, out)
	}
	if a.evalCompare(paths[:1]) != 2 {
		t.Fatal("missing argument accepted")
	}
}

func TestScorecardLiveCannotSilentlyReplay(t *testing.T) {
	t.Setenv("SELFMIND_EVAL_VCR", "replay")
	out := &bytes.Buffer{}
	a := &App{ctx: context.Background(), stdout: out, stderr: out}
	if code := a.evalScorecard([]string{"--live"}); code != 2 || !strings.Contains(out.String(), "conflicts") {
		t.Fatalf("code=%d %s", code, out)
	}
}

func TestScorecardRetainsInvalidCaseAndContinues(t *testing.T) {
	dir := t.TempDir()
	out := &bytes.Buffer{}
	cases := filepath.Join(dir, "cases")
	if err := os.Mkdir(cases, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cases, "a.yaml"), []byte("id: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	valid := "id: controls\nmodel_required: false\nworkspace: isolated\nchannel: cli\nturns:\n  - input: /status\n"
	if err := os.WriteFile(filepath.Join(cases, "z.yaml"), []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("storage:\n  type: sqlite\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)
	a := &App{ctx: context.Background(), stdout: out, stderr: out, configPath: cfg}
	report := filepath.Join(dir, "scorecard.md")
	if code := a.evalScorecard([]string{cases, "--out", report}); code != 1 {
		t.Fatalf("code=%d output=%s", code, out)
	}
	set, err := selfeval.LoadResultSet(report + ".json")
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Samples) != 2 || set.Samples[0].Status != "error" || set.Samples[1].Status != "passed" {
		t.Fatalf("samples discarded: %+v", set.Samples)
	}
}
