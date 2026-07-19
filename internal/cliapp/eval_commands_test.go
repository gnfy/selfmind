package cliapp

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestEvalCleanDiskResidue proves the CLI wiring: `eval clean` scans the config
// home (parent of config.yaml) and the configured data dir for eval-* tenant
// residue, stays a pure report by default, and with --yes removes only the
// verified directories while skipping anything with unrecognized contents.
func TestEvalCleanDiskResidue(t *testing.T) {
	configHome := t.TempDir()
	dataDir := t.TempDir()
	cfgPath := filepath.Join(configHome, "config.yaml")
	cfg := "storage:\n  type: \"sqlite\"\n  data_dir: " + strconv.Quote(dataDir) + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	skillsResidue := filepath.Join(configHome, "eval-chat_basic_001-1782580513697563644")
	if err := os.MkdirAll(filepath.Join(skillsResidue, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	memResidue := filepath.Join(dataDir, "eval-chat_basic_001-1782580513697563644")
	if err := os.MkdirAll(memResidue, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memResidue, "memory.db"), make([]byte, 512), 0o644); err != nil {
		t.Fatal(err)
	}
	// Matching name, foreign contents: must survive --yes and be reported.
	foreign := filepath.Join(dataDir, "eval-foreign_case-1782580513697563000")
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreign, "user-notes.docx"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	newApp := func() (*App, *bytes.Buffer) {
		out := &bytes.Buffer{}
		return &App{ctx: context.Background(), stdout: out, stderr: out, configPath: cfgPath}, out
	}

	// Dry run: reports the residue, deletes nothing.
	app, out := newApp()
	if code := app.evalClean(nil); code != 0 {
		t.Fatalf("dry run exit=%d output=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "Eval residue directories on disk (would delete)") {
		t.Fatalf("dry run should report disk residue, got:\n%s", out.String())
	}
	for _, p := range []string{skillsResidue, memResidue, foreign} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("dry run must not delete %s: %v", p, err)
		}
	}

	// Apply: removes the two verified dirs, keeps and reports the foreign one.
	app, out = newApp()
	if code := app.evalClean([]string{"--yes"}); code != 0 {
		t.Fatalf("apply exit=%d output=%s", code, out.String())
	}
	for _, gone := range []string{skillsResidue, memResidue} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("apply should remove %s (err=%v)", gone, err)
		}
	}
	if _, err := os.Stat(filepath.Join(foreign, "user-notes.docx")); err != nil {
		t.Fatalf("foreign-content dir must survive apply: %v", err)
	}
	if !strings.Contains(out.String(), "SKIPPED "+foreign) {
		t.Fatalf("apply should report the skipped foreign dir, got:\n%s", out.String())
	}
}
