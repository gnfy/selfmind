package cliapp

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServiceManagerFailureSeparatesUserMessageFromExactPrivateEvidence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	raw := []byte("Bootstrap failed: 5: Input/output error\nTry re-running as root\n")
	err := gatewayServiceCommandError("launchd", "bootstrap gui/501 test.plist", errors.New("exit status 5"), raw)
	if strings.Contains(err.Error(), "Bootstrap failed") || strings.Contains(err.Error(), "root") {
		t.Fatalf("user-facing error exposed raw platform output: %v", err)
	}
	path := filepath.Join(home, ".selfmind", "service-manager-last-error.log")
	evidence, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(evidence), string(raw)) || !strings.Contains(string(evidence), "exit status 5") {
		t.Fatalf("private evidence did not preserve exact failure:\n%s", evidence)
	}
	info, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("evidence mode = %o, want 600", info.Mode().Perm())
	}
}

func TestRenderLaunchdPlist(t *testing.T) {
	data, err := renderLaunchdPlist(launchdPlistData{
		Label:       gatewayLaunchdLabel,
		ProgramArgs: []string{"/usr/local/bin/node", "/path/with &/selfmind.js", "--config", "/Users/test/.selfmind/config.yaml", "gateway", "run"},
		Environment: map[string]string{
			"PATH": "/usr/bin:/bin", "SELF_CONFIG": "/Users/test/.selfmind/config.yaml",
			selfMindServiceManagerEnv: "launchd", selfMindServiceGenerationEnv: "service_test",
		},
		StdoutPath: "/Users/test/.selfmind/gateway.log",
		StderrPath: "/Users/test/.selfmind/gateway.log",
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, expected := range []string{
		"<string>com.selfmind.gateway</string>",
		"<string>/path/with &amp;/selfmind.js</string>",
		"<key>KeepAlive</key>",
		"<key>SuccessfulExit</key>",
		"<key>ThrottleInterval</key>",
		"<key>RunAtLoad</key>",
		"<key>SELF_CONFIG</key>",
		"<key>SELFMIND_SERVICE_MANAGER</key>",
		"<string>service_test</string>",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("plist missing %q:\n%s", expected, text)
		}
	}
	if strings.Index(text, "<key>PATH</key>") > strings.Index(text, "<key>SELF_CONFIG</key>") {
		t.Fatalf("environment keys should be stable and sorted:\n%s", text)
	}
}

func TestRenderSystemdUserUnit(t *testing.T) {
	data, err := renderSystemdUserUnit(systemdUnitData{
		Description: "SelfMind gateway",
		ProgramArgs: []string{"/opt/Self Mind/selfmind", "--config", "/home/test/a%b/config.yaml", "gateway", "run"},
		Environment: map[string]string{
			"PATH": "/usr/bin:/bin", "SELF_CONFIG": "/home/test/a%b/config.yaml",
			selfMindServiceManagerEnv: "systemd", selfMindServiceGenerationEnv: "service_test",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, expected := range []string{
		`ExecStart="/opt/Self Mind/selfmind" "--config" "/home/test/a%%b/config.yaml" "gateway" "run"`,
		`Environment="SELF_CONFIG=/home/test/a%%b/config.yaml"`,
		"Restart=on-failure",
		"WantedBy=default.target",
		`Environment="SELFMIND_SERVICE_MANAGER=systemd"`,
		`Environment="SELFMIND_SERVICE_GENERATION=service_test"`,
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("systemd unit missing %q:\n%s", expected, text)
		}
	}
	if strings.Index(text, `Environment="PATH=`) > strings.Index(text, `Environment="SELF_CONFIG=`) {
		t.Fatalf("environment keys should be stable and sorted:\n%s", text)
	}
}

func TestServicePassthroughEnvironmentRejectsCredentials(t *testing.T) {
	got := strings.Join(servicePassthroughEnvironment([]string{
		"LANG=en_US.UTF-8",
		"HTTPS_PROXY=http://proxy.example:8080",
		"OPENAI_API_KEY=secret",
		"SERVICE_CONFIG=/tmp/config.json",
		"TOKEN_CONFIG=/tmp/looks-safe-but-name-is-secret",
		"NO_PROXY=localhost",
	}), "\n")
	for _, expected := range []string{"LANG=en_US.UTF-8", "HTTPS_PROXY=http://proxy.example:8080", "SERVICE_CONFIG=/tmp/config.json", "NO_PROXY=localhost"} {
		if !strings.Contains(got, expected) {
			t.Fatalf("filtered environment missing %q: %q", expected, got)
		}
	}
	for _, forbidden := range []string{"OPENAI_API_KEY", "TOKEN_CONFIG"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("filtered environment exposed %q: %q", forbidden, got)
		}
	}
}
