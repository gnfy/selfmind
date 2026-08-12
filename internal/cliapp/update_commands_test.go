package cliapp

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"selfmind/internal/updatecheck"
)

// Regression for the stale-notice bug: the cache row written BEFORE an
// upgrade (current=old, latest=new) must stop announcing once the running
// binary already IS the latest version.
func TestShouldAnnounceUpdateComparesRunningVersionNotCachedCurrent(t *testing.T) {
	cached := updatecheck.Result{
		Current:   "0.1.0-beta.4",
		Latest:    "0.1.0-beta.5",
		Channel:   "next",
		CheckedAt: time.Now().UTC(),
	}
	if shouldAnnounceUpdate(cached, "v0.1.0-beta.4", "next") != true {
		t.Fatalf("expected announce for an older running binary")
	}
	if shouldAnnounceUpdate(cached, "v0.1.0-beta.5", "next") {
		t.Fatalf("stale cache must not announce after the upgrade already happened")
	}
	if shouldAnnounceUpdate(cached, "v0.1.0-beta.6", "next") {
		t.Fatalf("a running binary newer than the cached latest must not announce")
	}
}

// After a channel switch the cached row watches the wrong dist-tag: a next
// row must never announce to a user whose effective channel is now latest.
func TestShouldAnnounceUpdateRequiresMatchingChannel(t *testing.T) {
	cached := updatecheck.Result{
		Current:   "0.1.0-beta.4",
		Latest:    "0.1.0-beta.5",
		Channel:   "next",
		CheckedAt: time.Now().UTC(),
	}
	if shouldAnnounceUpdate(cached, "v0.1.0-beta.4", "latest") {
		t.Fatalf("a cache row from another channel must not announce")
	}
}

func TestShouldAnnounceUpdateIgnoresEmptyCache(t *testing.T) {
	if shouldAnnounceUpdate(updatecheck.Result{}, "v0.1.0-beta.5", "next") {
		t.Fatalf("an empty cache row must never announce")
	}
}

func TestChannelPinHint(t *testing.T) {
	if hint := channelPinHint("", "next", "next"); hint != "" {
		t.Fatalf("no flag given: no hint, got %q", hint)
	}
	if hint := channelPinHint("latest", "", "latest"); hint != "" {
		t.Fatalf("auto config has no pin to disagree with, got %q", hint)
	}
	if hint := channelPinHint("next", "next", "next"); hint != "" {
		t.Fatalf("flag agreeing with the pin needs no hint, got %q", hint)
	}
	hint := channelPinHint("latest", "next", "latest")
	if !strings.Contains(hint, "updates.channel: latest") || !strings.Contains(hint, "auto") {
		t.Fatalf("mismatch hint should mention the durable edit and auto, got %q", hint)
	}
}

func TestUpdateInstallArgsPerPackageManager(t *testing.T) {
	cases := []struct {
		method  string
		channel string
		want    string
	}{
		{"", "next", "npm install -g @selfmind/cli@next"},
		{"npm", "latest", "npm install -g @selfmind/cli@latest"},
		{"pnpm", "next", "pnpm add -g @selfmind/cli@next"},
		{"yarn", "beta", "yarn global add @selfmind/cli@next"},
		{"bun", "", "bun add -g @selfmind/cli@latest"},
		{"unknown-tool", "latest", "npm install -g @selfmind/cli@latest"},
	}
	for _, tc := range cases {
		got := strings.Join(updateInstallArgs(tc.channel, tc.method), " ")
		if got != tc.want {
			t.Errorf("updateInstallArgs(%q, %q) = %q, want %q", tc.channel, tc.method, got, tc.want)
		}
	}
}

func TestUpdateAvailableHintUsesSelfMindUpdate(t *testing.T) {
	got := updatecheck.AvailableNotice("0.1.0-beta.14")
	if got != "Update available: SelfMind 0.1.0-beta.14. Run `selfmind update`." {
		t.Fatalf("update hint = %q", got)
	}
	if strings.Contains(got, "npm install") {
		t.Fatalf("user-facing hint must hide package-manager details: %q", got)
	}
}

func TestChooseUpdateDisposition(t *testing.T) {
	cases := []struct {
		name               string
		current, available string
		force              bool
		want               updateDisposition
	}{
		{"older upgrades", "0.1.0-beta.12", "0.1.0-beta.13", false, updateInstall},
		{"equal refreshes package", "0.1.0-beta.13", "0.1.0-beta.13", false, updateRefresh},
		{"newer local build is preserved", "0.1.0-beta.14", "0.1.0-beta.13", false, updateSkipNewer},
		{"force permits replacement", "0.1.0-beta.14", "0.1.0-beta.13", true, updateInstall},
		{"npm-managed development replacement restores release", "0.1.0-dev", "0.1.0-beta.13", false, updateInstall},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := chooseUpdateDisposition(tc.current, tc.available, tc.force); got != tc.want {
				t.Fatalf("chooseUpdateDisposition(%q, %q, %v) = %v, want %v", tc.current, tc.available, tc.force, got, tc.want)
			}
		})
	}
}

func TestUpdateVersionPatternExtractsSemverFromBanner(t *testing.T) {
	cases := []struct {
		banner string
		want   string
	}{
		{"SelfMind v0.1.0-beta.5 (abc1234, built 2026-07-27)", "v0.1.0-beta.5"},
		{"SelfMind 1.2.3", "1.2.3"},
		{"SelfMind v0.1.0-dev", "v0.1.0-dev"},
		{"no version here", ""},
	}
	for _, tc := range cases {
		if got := updateVersionPattern.FindString(tc.banner); got != tc.want {
			t.Errorf("extract from %q = %q, want %q", tc.banner, got, tc.want)
		}
	}
}

// A development build must refuse to self-overwrite unless --force is given,
// and must not touch the network before that guard fires.
func TestUpdateApplyRefusesDevelopmentBuildWithoutForce(t *testing.T) {
	t.Setenv("SELFMIND_NPM_LAUNCHER", "")
	t.Setenv("SELFMIND_NPM_PACKAGE", "")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := &App{
		ctx:    context.Background(),
		args:   []string{"selfmind", "update"},
		stdout: stdout,
		stderr: stderr,
	}
	handled, code := app.runUpdateCommandIfRequested()
	if !handled {
		t.Fatalf("update command not handled")
	}
	if code == 0 {
		t.Fatalf("dev build without --force must fail, got exit 0")
	}
	if !strings.Contains(stderr.String(), "--force") {
		t.Fatalf("error should point at --force, got: %s", stderr.String())
	}
}

func TestRunningFromNPMInstall(t *testing.T) {
	t.Setenv("SELFMIND_NPM_LAUNCHER", "/prefix/lib/node_modules/@selfmind/cli/bin/selfmind.js")
	if !runningFromNPMInstall() {
		t.Fatal("npm launcher marker must identify an npm-managed development replacement")
	}
	t.Setenv("SELFMIND_NPM_LAUNCHER", "")
	t.Setenv("SELFMIND_NPM_PACKAGE", "@selfmind/cli")
	if !runningFromNPMInstall() {
		t.Fatal("npm package marker must identify an npm-managed development replacement")
	}
}
