package cliapp

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestVersionCommandExitsWithoutStartingTUI(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"selfmind", "--version"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "SelfMind "+Version {
		t.Fatalf("version output = %q", got)
	}
}

func TestUnknownCommandDoesNotStartTUI(t *testing.T) {
	t.Setenv("SELF_USE_GATEWAY", "")
	t.Setenv("SELF_USE_DAEMON", "")
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"selfmind", "frobnicate"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), `unknown command "frobnicate"`) {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
