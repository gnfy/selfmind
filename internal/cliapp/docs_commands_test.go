package cliapp

import (
	"strings"
	"testing"
)

func TestDocsCheckCommand(t *testing.T) {
	app, out := newSelfcheckTestApp()
	app.args = []string{"selfmind", "docs", "check"}
	handled, code := app.runDocsCommandIfRequested()
	if !handled || code != 0 || !strings.Contains(out.String(), "docs check: OK") {
		t.Fatalf("docs check failed; handled=%v code=%d output:\n%s", handled, code, out.String())
	}
}

func TestDocsCommandRejectsUnknownSubcommand(t *testing.T) {
	app, out := newSelfcheckTestApp()
	app.args = []string{"selfmind", "docs", "accept-everything"}
	handled, code := app.runDocsCommandIfRequested()
	if !handled || code != 2 || !strings.Contains(out.String(), "unknown docs command") {
		t.Fatalf("unknown docs command should fail; handled=%v code=%d output:\n%s", handled, code, out.String())
	}
}
