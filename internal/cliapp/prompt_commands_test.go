package cliapp

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/buildinfo"
)

func newPromptTestApp(t *testing.T, configPath string, args ...string) (*App, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	return &App{
		ctx: context.Background(), args: append([]string{"selfmind", "prompt"}, args...),
		stdin: strings.NewReader(""), stdout: out, stderr: errOut, configPath: configPath,
	}, out, errOut
}

func TestGatewayRestartRejectsInvalidPromptBeforeShutdown(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, ".selfmind", "config.yaml")
	app, _, errOut := newPromptTestApp(t, configPath)
	promptRoot := filepath.Join(filepath.Dir(configPath), "prompts")
	if err := os.MkdirAll(promptRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(promptRoot, "agent.md"), []byte("## Unknown\n\nbroken\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if code := app.gatewayRestart(nil); code != 1 {
		t.Fatalf("restart code = %d, stderr=%s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "running gateway was not restarted") || !strings.Contains(errOut.String(), "unknown section") {
		t.Fatalf("restart error = %q", errOut.String())
	}
}

func TestPromptEditValidateDiffAndReset(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".selfmind", "config.yaml")

	app, out, errOut := newPromptTestApp(t, configPath, "edit", "agent")
	if handled, code := app.runPromptCommandIfRequested(); !handled || code != 0 {
		t.Fatalf("edit handled=%v code=%d stderr=%s", handled, code, errOut.String())
	}
	promptPath := filepath.Join(filepath.Dir(configPath), "prompts", "agent.md")
	if _, err := os.Stat(promptPath); err != nil {
		t.Fatalf("template not created: %v\n%s", err, out.String())
	}
	custom := "## Persona\n\nUse concise Chinese.\n\n## Progress Updates\n\noff\n"
	if err := os.WriteFile(promptPath, []byte(custom), 0o600); err != nil {
		t.Fatal(err)
	}

	app, out, errOut = newPromptTestApp(t, configPath, "validate")
	if _, code := app.runPromptCommandIfRequested(); code != 0 {
		t.Fatalf("validate code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "workspace is valid") {
		t.Fatalf("validate output: %s", out.String())
	}

	app, out, errOut = newPromptTestApp(t, configPath, "diff", "main")
	if _, code := app.runPromptCommandIfRequested(); code != 0 {
		t.Fatalf("diff code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Use concise Chinese") || !strings.Contains(out.String(), "+ off") {
		t.Fatalf("diff output: %s", out.String())
	}

	app, out, errOut = newPromptTestApp(t, configPath, "reset", "agent")
	if _, code := app.runPromptCommandIfRequested(); code != 0 {
		t.Fatalf("reset code=%d stderr=%s", code, errOut.String())
	}
	if _, err := os.Stat(promptPath); !os.IsNotExist(err) {
		t.Fatalf("prompt still active after reset: %v", err)
	}
	if !strings.Contains(out.String(), ".bak-") {
		t.Fatalf("reset did not report recoverable backup: %s", out.String())
	}
}

func TestPromptValidationRejectsLockedSectionOff(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".selfmind", "config.yaml")
	promptPath := filepath.Join(filepath.Dir(configPath), "prompts", "agent.md")
	if err := os.MkdirAll(filepath.Dir(promptPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(promptPath, []byte("## Working Style\n\noff\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	app, _, errOut := newPromptTestApp(t, configPath, "validate")
	if _, code := app.runPromptCommandIfRequested(); code != 1 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "cannot be disabled") {
		t.Fatalf("stderr=%s", errOut.String())
	}

	app, out, errOut := newPromptTestApp(t, configPath, "edit", "agent")
	if _, code := app.runPromptCommandIfRequested(); code != 0 {
		t.Fatalf("edit cannot open invalid prompt: code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), promptPath) {
		t.Fatalf("edit output=%s", out.String())
	}

	app, _, errOut = newPromptTestApp(t, configPath, "reset", "agent")
	if _, code := app.runPromptCommandIfRequested(); code != 0 {
		t.Fatalf("reset cannot recover invalid prompt: code=%d stderr=%s", code, errOut.String())
	}
}

func TestPromptEditMigratesLegacyFileWithRecoverableBackup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".selfmind", "config.yaml")
	promptPath := filepath.Join(filepath.Dir(configPath), "prompts", "agent.md")
	if err := os.MkdirAll(filepath.Dir(promptPath), 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := "## Persona\n\nConcise.\n\n## Examples\n\n- Preserve identifiers.\n"
	if err := os.WriteFile(promptPath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	app, out, errOut := newPromptTestApp(t, configPath, "edit", "agent")
	if _, code := app.runPromptCommandIfRequested(); code != 0 {
		t.Fatalf("edit code=%d stderr=%s", code, errOut.String())
	}
	data, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "<!-- selfmind:section Persona -->") || !strings.Contains(string(data), "## Examples") {
		t.Fatalf("migrated file:\n%s", data)
	}
	if !strings.Contains(out.String(), "Migrated legacy prompt markers; backup:") {
		t.Fatalf("edit output=%s", out.String())
	}
	backups, err := filepath.Glob(promptPath + ".legacy-*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("backups=%v err=%v", backups, err)
	}
	backup, err := os.ReadFile(backups[0])
	if err != nil || string(backup) != legacy {
		t.Fatalf("backup=%q err=%v", backup, err)
	}
}

func TestPromptShowExplainsReplaceAndAppendPolicies(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".selfmind", "config.yaml")

	app, out, errOut := newPromptTestApp(t, configPath, "show", "memory_extract")
	if _, code := app.runPromptCommandIfRequested(); code != 0 {
		t.Fatalf("memory show code=%d stderr=%s", code, errOut.String())
	}
	for _, want := range []string{
		"default = built-in only",
		"policy: append-only; locked base preserved; off not allowed",
		"injection: memory_extract.post_run",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("memory show missing %q:\n%s", want, out.String())
		}
	}

	app, out, errOut = newPromptTestApp(t, configPath, "show", "agent")
	if _, code := app.runPromptCommandIfRequested(); code != 0 {
		t.Fatalf("agent show code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "policy: replaceable; off allowed") ||
		!strings.Contains(out.String(), "policy: append-only; locked base preserved") {
		t.Fatalf("agent show did not distinguish policies:\n%s", out.String())
	}
}

// TestPromptShowOnInvalidWorkspaceFailsAndDoesNotClaimDefaults pins A-9. An
// invalid workspace previously rendered every section as "default" and exited
// 0, so a script or agent could not tell "no customizations" from "the file is
// malformed and the daemon will refuse to start".
func TestPromptShowOnInvalidWorkspaceFailsAndDoesNotClaimDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".selfmind", "config.yaml")

	app, _, errOut := newPromptTestApp(t, configPath, "edit", "agent")
	if handled, code := app.runPromptCommandIfRequested(); !handled || code != 0 {
		t.Fatalf("edit handled=%v code=%d stderr=%s", handled, code, errOut.String())
	}
	promptPath := filepath.Join(filepath.Dir(configPath), "prompts", "agent.md")

	// A customized Persona plus a heading the catalog does not know: the file is
	// unloadable, so no section state can be resolved.
	broken := "## Persona\n\nUse concise Chinese.\n\n## Notes\n\nstray heading\n"
	if err := os.WriteFile(promptPath, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}

	app, out, errOut := newPromptTestApp(t, configPath, "show", "agent")
	handled, code := app.runPromptCommandIfRequested()
	if !handled {
		t.Fatal("show was not handled")
	}
	if code == 0 {
		t.Fatalf("an invalid workspace must not exit 0\nstdout=%s\nstderr=%s", out.String(), errOut.String())
	}
	stdout := out.String()
	if !strings.Contains(stdout, "Validation: INVALID") {
		t.Errorf("missing validation line:\n%s", stdout)
	}
	if strings.Contains(stdout, "state: default") {
		t.Errorf("unresolvable sections must not be reported as default:\n%s", stdout)
	}
	if !strings.Contains(stdout, "state: unresolved") {
		t.Errorf("expected unresolved section state:\n%s", stdout)
	}
	if !strings.Contains(stdout, "last-known-good snapshot") || !strings.Contains(stdout, "built-in defaults") {
		t.Errorf("the operator must be told the daemon will ignore the active file and use a safe fallback:\n%s", stdout)
	}
}

// TestPromptShowOnValidWorkspaceStillSucceeds guards the exit-code change from
// turning an ordinary inspection into a failure.
func TestPromptShowOnValidWorkspaceStillSucceeds(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".selfmind", "config.yaml")

	app, out, errOut := newPromptTestApp(t, configPath, "show", "agent")
	handled, code := app.runPromptCommandIfRequested()
	if !handled || code != 0 {
		t.Fatalf("show handled=%v code=%d stderr=%s", handled, code, errOut.String())
	}
	if strings.Contains(out.String(), "state: unresolved") {
		t.Errorf("a valid workspace must resolve section state:\n%s", out.String())
	}
}

func TestPromptListShowsInvalidDiskWorkspace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".selfmind", "config.yaml")
	promptPath := filepath.Join(filepath.Dir(configPath), "prompts", "agent.md")
	if err := os.MkdirAll(filepath.Dir(promptPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(promptPath, []byte("## Unknown\n\nbroken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	app, out, _ := newPromptTestApp(t, configPath, "list")
	if _, code := app.runPromptCommandIfRequested(); code != 1 {
		t.Fatalf("list code=%d output=%s", code, out.String())
	}
	for _, want := range []string{"Prompt workspace:", "Disk: INVALID", "unknown section", "unresolved"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("invalid prompt list missing %q:\n%s", want, out.String())
		}
	}
}

func TestPromptTestVerifiesCustomSectionComposition(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".selfmind", "config.yaml")
	promptPath := filepath.Join(filepath.Dir(configPath), "prompts", "agent.md")
	if err := os.MkdirAll(filepath.Dir(promptPath), 0o700); err != nil {
		t.Fatal(err)
	}
	content := "## Persona\n\nUse concise Chinese.\n\n## Working Style\n\nPrefer reversible edits.\n\n## Progress Updates\n\noff\n"
	if err := os.WriteFile(promptPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	app, out, errOut := newPromptTestApp(t, configPath, "test", "agent")
	if _, code := app.runPromptCommandIfRequested(); code != 0 {
		t.Fatalf("test code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "custom_sections=3 section_composition=verified") {
		t.Fatalf("prompt test did not verify active composition:\n%s", out.String())
	}
}

func TestPromptBuildRestartNoticeSupportsOldAndNewDaemonEvents(t *testing.T) {
	if got := promptBuildRestartNotice(buildinfo.Fingerprint(), ""); got != "" {
		t.Fatalf("matching fingerprint notice=%q", got)
	}
	if got := promptBuildRestartNotice("different-build", ""); !strings.Contains(got, "restart required") {
		t.Fatalf("different fingerprint notice=%q", got)
	}
	if got := promptBuildRestartNotice("", "different-version"); !strings.Contains(got, "daemon version differs") {
		t.Fatalf("legacy event/version notice=%q", got)
	}
}
