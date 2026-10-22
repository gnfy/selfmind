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

func TestServiceHomeGuard(t *testing.T) {
	account, other, named := t.TempDir(), t.TempDir(), t.TempDir()
	link := filepath.Join(t.TempDir(), "home-link")
	if err := os.Symlink(account, link); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "no-such-home")
	accountIs := func(path string) func() (string, error) { return func() (string, error) { return path, nil } }
	lookupFails := func() (string, error) { return "", errors.New("account database unavailable") }
	for _, tc := range []struct {
		name, home string
		account    func() (string, error)
		allowed    string
		refused    []string
	}{
		{"the account's own home", account, accountIs(account), "", nil},
		{"a link to the account's home", link, accountIs(account), "", nil},
		{"another HOME", other, accountIs(account), "", []string{"refusing to restart the background service", other, account}},
		{"a HOME that does not exist", missing, accountIs(account), "", []string{missing, account}},
		{"another HOME the operator named", other, accountIs(account), other, nil},
		{"another HOME than the one the operator named", other, accountIs(account), named, []string{other}},
		{"a blank operator setting", other, accountIs(account), "  ", []string{other}},
		{"an account that cannot be looked up", other, lookupFails, "", []string{"cannot confirm", other, "account database unavailable"}},
		{"an account that cannot be looked up, HOME named by the operator", other, lookupFails, other, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkServiceHome("restart the background service", tc.home, tc.account, tc.allowed)
			if len(tc.refused) == 0 {
				if err != nil {
					t.Fatalf("refused a HOME that may change the service: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("a HOME that is not the account's own was allowed to change the service")
			}
			for _, want := range tc.refused {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal %q does not say %q", err, want)
				}
			}
		})
	}
}

// fakeServiceManager puts stand-ins for launchctl and systemctl first on PATH.
// Reading state succeeds, as for a running service, and a user session exists;
// every other verb is recorded in the returned list and fails. A guard that regressed can therefore neither
// reach the real service manager nor change anything.
func fakeServiceManager(t *testing.T) func() []string {
	t.Helper()
	bin := t.TempDir()
	record := filepath.Join(bin, "verbs.log")
	script := "#!/bin/sh\n" +
		"verb=\"\"\n" +
		"for arg in \"$@\"; do case \"$arg\" in --*) ;; *) verb=\"$arg\"; break ;; esac; done\n" +
		"case \"$verb\" in\n" +
		"  print) echo \"state = running\"; exit 0 ;;\n" +
		"  show) echo \"LoadState=loaded\"; echo \"ActiveState=active\"; exit 0 ;;\n" +
		"  show-environment) exit 0 ;;\n" +
		"esac\n" +
		"echo \"$verb\" >> '" + record + "'\n" +
		"exit 1\n"
	for _, name := range []string{"launchctl", "systemctl"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []string {
		data, _ := os.ReadFile(record)
		return strings.Fields(string(data))
	}
}

// serviceOperations are every call that changes the personal service, as the
// commands make them.
func serviceOperations() map[string]func() error {
	return map[string]func() error{
		"install":   func() error { _, err := gatewayServiceInstall("", 0, nil); return err },
		"uninstall": func() error { _, _, err := gatewayServiceUninstall(); return err },
		"start":     func() error { _, _, err := gatewayServiceStartIfInstalled(""); return err },
		"restart":   func() error { _, _, err := gatewayServiceRestartIfInstalled(""); return err },
		"stop":      func() error { _, _, err := gatewayServiceStopIfInstalled(); return err },
		"preflight": func() error { return gatewayServicePreflight() },
	}
}

// An isolated HOME that holds a service file is the incident: its install, stop
// or restart acted on the account's real service. Each operation must refuse
// before it reaches the service manager, and leave the service file alone.
func assertForeignHomeCannotChangeService(t *testing.T, home, serviceFile string, verbs func() []string) {
	t.Helper()
	for name, run := range serviceOperations() {
		t.Run(name, func(t *testing.T) {
			err := run()
			if err == nil || !strings.Contains(err.Error(), "refusing to") || !strings.Contains(err.Error(), home) {
				t.Fatalf("a HOME that is not the account's own was not refused: %v", err)
			}
		})
	}
	if got := verbs(); len(got) != 0 {
		t.Fatalf("the service manager was changed from a foreign HOME: %v", got)
	}
	if _, err := os.Stat(serviceFile); err != nil {
		t.Fatalf("the service file was removed: %v", err)
	}
}

// A HOME with no service never touches the service manager, so an isolated HOME
// keeps running its own on-demand daemon with these commands.
func assertHomeWithoutServiceIsUnaffected(t *testing.T, verbs func() []string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv(serviceHomeEnv, "")
	for name, run := range map[string]func() error{
		"uninstall": serviceOperations()["uninstall"], "start": serviceOperations()["start"],
		"restart": serviceOperations()["restart"], "stop": serviceOperations()["stop"],
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(); err != nil {
				t.Fatalf("a HOME with no service was refused: %v", err)
			}
		})
	}
	if got := verbs(); len(got) != 0 {
		t.Fatalf("the service manager was touched without a service: %v", got)
	}
}
