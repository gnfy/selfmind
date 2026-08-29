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
	environment := map[string]string{
		"PATH": "/usr/bin:/bin", "SELF_CONFIG": "/Users/test/.selfmind/config.yaml",
		selfMindServiceManagerEnv: "launchd", selfMindServiceGenerationEnv: "service_test",
	}
	for _, entry := range servicePassthroughEnvironment([]string{
		"HTTPS_PROXY=http://127.0.0.1:7897",
		"NO_PROXY=localhost,127.0.0.1",
	}) {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			environment[name] = value
		}
	}
	data, err := renderLaunchdPlist(launchdPlistData{
		Label:       gatewayLaunchdLabel,
		ProgramArgs: []string{"/usr/local/bin/node", "/path/with &/selfmind.js", "--config", "/Users/test/.selfmind/config.yaml", "gateway", "run"},
		Environment: environment,
		StdoutPath:  "/Users/test/.selfmind/gateway.log",
		StderrPath:  "/Users/test/.selfmind/gateway.log",
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
		"<key>HTTPS_PROXY</key>",
		"<string>http://127.0.0.1:7897</string>",
		"<key>NO_PROXY</key>",
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
	environment := map[string]string{
		"PATH": "/usr/bin:/bin", "SELF_CONFIG": "/home/test/a%b/config.yaml",
		selfMindServiceManagerEnv: "systemd", selfMindServiceGenerationEnv: "service_test",
	}
	for _, entry := range servicePassthroughEnvironment([]string{
		"HTTPS_PROXY=http://127.0.0.1:7897",
		"NO_PROXY=localhost,127.0.0.1",
	}) {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			environment[name] = value
		}
	}
	data, err := renderSystemdUserUnit(systemdUnitData{
		Description: "SelfMind gateway",
		ProgramArgs: []string{"/opt/Self Mind/selfmind", "--config", "/home/test/a%b/config.yaml", "gateway", "run"},
		Environment: environment,
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
		`Environment="HTTPS_PROXY=http://127.0.0.1:7897"`,
		`Environment="NO_PROXY=localhost,127.0.0.1"`,
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("systemd unit missing %q:\n%s", expected, text)
		}
	}
	if strings.Index(text, `Environment="PATH=`) > strings.Index(text, `Environment="SELF_CONFIG=`) {
		t.Fatalf("environment keys should be stable and sorted:\n%s", text)
	}
}

func TestServicePassthroughEnvironmentIncludesSafeProxyConfiguration(t *testing.T) {
	got := strings.Join(servicePassthroughEnvironment([]string{
		"LANG=en_US.UTF-8",
		"http_proxy=http://lower.example:8080",
		"HTTP_PROXY=http://proxy.example:8080",
		"https_proxy=http://secure.example:8443",
		"all_proxy=socks5://fallback.example:1080",
		"OPENAI_API_KEY=secret",
		"SERVICE_CONFIG=/tmp/config.json",
		"TOKEN_CONFIG=/tmp/looks-safe-but-name-is-secret",
		"NO_PROXY=localhost",
		"no_proxy=localhost",
	}), "\n")
	for _, expected := range []string{
		"LANG=en_US.UTF-8",
		"SERVICE_CONFIG=/tmp/config.json",
		"HTTP_PROXY=http://proxy.example:8080",
		"HTTPS_PROXY=http://secure.example:8443",
		"ALL_PROXY=socks5://fallback.example:1080",
		"NO_PROXY=localhost",
	} {
		if !strings.Contains(got, expected) {
			t.Fatalf("filtered environment missing %q: %q", expected, got)
		}
	}
	for _, forbidden := range []string{
		"http_proxy=", "https_proxy=", "all_proxy=", "no_proxy=", "OPENAI_API_KEY", "TOKEN_CONFIG",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("filtered environment exposed %q: %q", forbidden, got)
		}
	}
}

func TestServicePassthroughEnvironmentUsesAllProxyAsGoTransportFallback(t *testing.T) {
	got := strings.Join(servicePassthroughEnvironment([]string{
		"ALL_PROXY=socks5://127.0.0.1:7897",
	}), "\n")
	for _, expected := range []string{
		"ALL_PROXY=socks5://127.0.0.1:7897",
		"HTTP_PROXY=socks5://127.0.0.1:7897",
		"HTTPS_PROXY=socks5://127.0.0.1:7897",
	} {
		if !strings.Contains(got, expected) {
			t.Fatalf("ALL_PROXY fallback missing %q: %q", expected, got)
		}
	}
}

func TestServicePassthroughEnvironmentRejectsUnsafeProxyValuesAndLookalikeNames(t *testing.T) {
	got := strings.Join(servicePassthroughEnvironment([]string{
		"HTTPS_PROXY=http://user:password@proxy.example:8080",
		"http_proxy=ftp://proxy.example:2121",
		"PROXY_PASSWORD=secret",
		"MY_PROXY=http://proxy.example:8080",
		"NO_PROXY=http://user:password@bypass.example",
		"no_proxy=localhost",
	}), "\n")
	for _, forbidden := range []string{"user", "password", "ftp://", "PROXY_PASSWORD", "MY_PROXY"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("unsafe proxy environment exposed %q: %q", forbidden, got)
		}
	}
	if !strings.Contains(got, "NO_PROXY=localhost") {
		t.Fatalf("safe NO_PROXY was removed: %q", got)
	}
}
