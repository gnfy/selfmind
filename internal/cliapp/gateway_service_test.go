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
