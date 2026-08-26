package cliapp

import (
	"strings"
	"testing"
)

func TestRenderLaunchdPlist(t *testing.T) {
	data, err := renderLaunchdPlist(launchdPlistData{
		Label:       gatewayLaunchdLabel,
		ProgramArgs: []string{"/usr/local/bin/node", "/path/with &/selfmind.js", "--config", "/Users/test/.selfmind/config.yaml", "gateway", "run"},
		Environment: map[string]string{"PATH": "/usr/bin:/bin", "SELF_CONFIG": "/Users/test/.selfmind/config.yaml"},
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
		Environment: map[string]string{"PATH": "/usr/bin:/bin", "SELF_CONFIG": "/home/test/a%b/config.yaml"},
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
