package cliapp

import (
	"strings"
	"testing"
	"time"

	"selfmind/internal/platform/config"
	"selfmind/internal/updatecheck"
)

func updateHintConfig(channel string) *config.Config {
	cfg := &config.Config{}
	cfg.Updates.Enabled = true
	cfg.Updates.Channel = channel
	return cfg
}

// TestUpdateHintFromCache pins the exit-time reminder gating: it fires only
// for a genuinely newer version on the effective channel, never for dev
// builds, disabled updates, or a cache row from another channel.
func TestUpdateHintFromCache(t *testing.T) {
	newer := updatecheck.Result{Current: "0.1.0-beta.8", Latest: "0.1.0-beta.9", Channel: "next", CheckedAt: time.Now()}

	line := updateHintFromCache(updateHintConfig("auto"), "0.1.0-beta.8", newer)
	if !strings.Contains(line, "0.1.0-beta.9") || !strings.Contains(line, "selfmind update") {
		t.Fatalf("hint = %q, want version and command", line)
	}

	// Already running the latest: silent.
	if line := updateHintFromCache(updateHintConfig("auto"), "0.1.0-beta.9", newer); line != "" {
		t.Fatalf("up-to-date must be silent, got %q", line)
	}

	// Channel mismatch: a `latest`-pinned install ignores a `next` cache row.
	if line := updateHintFromCache(updateHintConfig("latest"), "0.1.0-beta.8", newer); line != "" {
		t.Fatalf("channel mismatch must be silent, got %q", line)
	}

	// Dev build: silent.
	if line := updateHintFromCache(updateHintConfig("auto"), "0.1.0-dev", newer); line != "" {
		t.Fatalf("dev build must be silent, got %q", line)
	}

	// Updates disabled: silent.
	cfg := updateHintConfig("auto")
	cfg.Updates.Enabled = false
	if line := updateHintFromCache(cfg, "0.1.0-beta.8", newer); line != "" {
		t.Fatalf("disabled updates must be silent, got %q", line)
	}
}
