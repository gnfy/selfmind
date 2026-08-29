package cliapp

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"selfmind/internal/platform/config"
)

const gatewayLaunchdLabel = "com.selfmind.gateway"

const (
	selfMindServiceManagerEnv    = "SELFMIND_SERVICE_MANAGER"
	selfMindServiceGenerationEnv = "SELFMIND_SERVICE_GENERATION"
)

type gatewayServiceInstallReceipt struct {
	Path       string
	Manager    string
	Generation string
}

func newGatewayServiceGeneration() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("create service generation: %w", err)
	}
	return "service_" + hex.EncodeToString(raw), nil
}

func gatewayServiceEvidencePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".selfmind", "service-manager-last-error.log")
}

// recordGatewayServiceFailure keeps exact platform output on a private
// diagnostic surface while user-facing errors remain short and portable.
func recordGatewayServiceFailure(manager, command string, commandErr error, output []byte) string {
	path := gatewayServiceEvidencePath()
	if path == "" {
		return ""
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return ""
	}
	var body bytes.Buffer
	fmt.Fprintf(&body, "time: %s\nmanager: %s\ncommand: %s\nerror: %v\noutput:\n", time.Now().UTC().Format(time.RFC3339Nano), manager, command, commandErr)
	body.Write(output)
	if len(output) == 0 || output[len(output)-1] != '\n' {
		body.WriteByte('\n')
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".service-manager-error-*.tmp")
	if err != nil {
		return ""
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return ""
	}
	if _, err := temp.Write(body.Bytes()); err != nil {
		_ = temp.Close()
		return ""
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return ""
	}
	if err := temp.Close(); err != nil {
		return ""
	}
	if err := os.Rename(tempPath, path); err != nil {
		return ""
	}
	return path
}

func gatewayServiceCommandError(manager, command string, commandErr error, output []byte) error {
	path := recordGatewayServiceFailure(manager, command, commandErr, output)
	if path != "" {
		return fmt.Errorf("personal %s command %q failed; details saved to %s", manager, command, path)
	}
	return fmt.Errorf("personal %s command %q failed", manager, command)
}

type launchdPlistData struct {
	Label       string
	ProgramArgs []string
	Environment map[string]string
	StdoutPath  string
	StderrPath  string
}

type systemdUnitData struct {
	Description string
	ProgramArgs []string
	Environment map[string]string
}

func environmentValue(parent []string, name string) string {
	for i := len(parent) - 1; i >= 0; i-- {
		key, value, ok := strings.Cut(parent[i], "=")
		if ok && key == name {
			return value
		}
	}
	return ""
}

func resolvedGatewayServiceCommand(configPath string) ([]string, error) {
	resolvedConfig, _ := config.ResolveConfigPath(configPath)
	launcher := strings.TrimSpace(os.Getenv("SELFMIND_NPM_LAUNCHER"))
	node := strings.TrimSpace(os.Getenv("SELFMIND_NODE_PATH"))
	if launcher != "" && node != "" {
		if _, err := os.Stat(launcher); err == nil {
			if _, err := os.Stat(node); err == nil {
				return []string{node, launcher, "--config", resolvedConfig, "gateway", "run"}, nil
			}
		}
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve SelfMind executable: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return nil, fmt.Errorf("resolve absolute SelfMind executable: %w", err)
	}
	return []string{executable, "--config", resolvedConfig, "gateway", "run"}, nil
}

func commandConfigPath(command []string) string {
	for i := 0; i+1 < len(command); i++ {
		if command[i] == "--config" {
			return command[i+1]
		}
	}
	return ""
}

func renderLaunchdPlist(data launchdPlistData) ([]byte, error) {
	if strings.TrimSpace(data.Label) == "" || len(data.ProgramArgs) == 0 {
		return nil, fmt.Errorf("launchd label and program arguments are required")
	}
	var out bytes.Buffer
	out.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	out.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	out.WriteString(`<plist version="1.0">` + "\n<dict>\n")
	writePlistKeyString(&out, "Label", data.Label)
	out.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, arg := range data.ProgramArgs {
		writePlistString(&out, "    ", arg)
	}
	out.WriteString("  </array>\n")
	out.WriteString("  <key>RunAtLoad</key>\n  <true/>\n")
	out.WriteString("  <key>KeepAlive</key>\n  <dict>\n")
	out.WriteString("    <key>SuccessfulExit</key>\n    <false/>\n")
	out.WriteString("  </dict>\n")
	out.WriteString("  <key>ThrottleInterval</key>\n  <integer>10</integer>\n")
	out.WriteString("  <key>ProcessType</key>\n  <string>Background</string>\n")
	if len(data.Environment) > 0 {
		out.WriteString("  <key>EnvironmentVariables</key>\n  <dict>\n")
		keys := sortedStringKeys(data.Environment)
		for _, key := range keys {
			writePlistKeyString(&out, key, data.Environment[key])
		}
		out.WriteString("  </dict>\n")
	}
	writePlistKeyString(&out, "StandardOutPath", data.StdoutPath)
	writePlistKeyString(&out, "StandardErrorPath", data.StderrPath)
	out.WriteString("</dict>\n</plist>\n")
	return out.Bytes(), nil
}

func writePlistKeyString(out *bytes.Buffer, key, value string) {
	out.WriteString("  <key>")
	_ = xml.EscapeText(out, []byte(key))
	out.WriteString("</key>\n")
	writePlistString(out, "  ", value)
}

func writePlistString(out *bytes.Buffer, indent, value string) {
	out.WriteString(indent)
	out.WriteString("<string>")
	_ = xml.EscapeText(out, []byte(value))
	out.WriteString("</string>\n")
}

func sortedStringKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

func renderSystemdUserUnit(data systemdUnitData) ([]byte, error) {
	if len(data.ProgramArgs) == 0 {
		return nil, fmt.Errorf("systemd program arguments are required")
	}
	description := strings.TrimSpace(data.Description)
	if description == "" {
		description = "SelfMind gateway"
	}
	var out bytes.Buffer
	fmt.Fprintf(&out, "[Unit]\nDescription=%s\nAfter=network-online.target\nWants=network-online.target\n\n", systemdValue(description))
	out.WriteString("[Service]\nType=simple\nExecStart=")
	for i, arg := range data.ProgramArgs {
		if i > 0 {
			out.WriteByte(' ')
		}
		out.WriteString(systemdQuote(arg))
	}
	out.WriteByte('\n')
	for _, key := range sortedStringKeys(data.Environment) {
		out.WriteString("Environment=")
		out.WriteString(systemdQuote(key + "=" + data.Environment[key]))
		out.WriteByte('\n')
	}
	out.WriteString("Restart=on-failure\nRestartSec=5s\nTimeoutStopSec=45s\nKillSignal=SIGINT\n\n[Install]\nWantedBy=default.target\n")
	return out.Bytes(), nil
}

func systemdQuote(value string) string {
	value = strings.ReplaceAll(value, "%", "%%")
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\"", "\\\"")
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "\r", " ")
	return "\"" + value + "\""
}

func systemdValue(value string) string {
	value = strings.ReplaceAll(value, "%", "%%")
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "\r", " ")
	return value
}

var servicePassthroughEnvironmentKeyMarkers = []string{"CONFIG", "_HOME", "KUBECONFIG"}

var servicePassthroughEnvironmentExactKeys = map[string]bool{
	"SHELL": true, "LANG": true, "LC_ALL": true, "LC_CTYPE": true,
}

// servicePassthroughEnvironment picks only non-credential configuration
// locations, locale, and exact standard proxy variables for an operating-system
// service definition. A managed Gateway does not inherit the installing shell's
// environment, so safe proxy values must cross this seam for the standard Go
// HTTP transport to preserve the same routing as an interactive client.
// Credential-bearing proxy URLs and proxy-shaped lookalike names never cross.
func servicePassthroughEnvironment(parent []string) []string {
	out := make([]string, 0, 12)
	for _, entry := range parent {
		name, value, ok := strings.Cut(entry, "=")
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if !ok || name == "" || value == "" {
			continue
		}
		upper := strings.ToUpper(name)
		if isCredentialShapedEnvName(upper) || valueEmbedsCredentials(value) {
			continue
		}
		if servicePassthroughEnvironmentExactKeys[name] || servicePassthroughEnvironmentExactKeys[upper] {
			out = append(out, name+"="+value)
			continue
		}
		for _, marker := range servicePassthroughEnvironmentKeyMarkers {
			if strings.Contains(upper, marker) {
				out = append(out, name+"="+value)
				break
			}
		}
	}
	out = append(out, serviceProxyEnvironment(parent)...)
	return out
}

// serviceProxyEnvironment resolves the standard process proxy variables into
// one canonical service environment. Go's http.ProxyFromEnvironment does not
// read ALL_PROXY, so a safe ALL_PROXY value supplies missing HTTP_PROXY and
// HTTPS_PROXY values while remaining available to compatible child processes.
func serviceProxyEnvironment(parent []string) []string {
	values := make(map[string]string, 8)
	for _, entry := range parent {
		name, value, ok := strings.Cut(entry, "=")
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if !ok || name == "" || value == "" {
			continue
		}
		switch name {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
			"http_proxy", "https_proxy", "all_proxy", "no_proxy":
			values[name] = value
		}
	}

	proxyValue := func(upper, lower string) string {
		for _, key := range []string{upper, lower} {
			if value := values[key]; validServiceProxyURL(value) {
				return value
			}
		}
		return ""
	}
	allProxy := proxyValue("ALL_PROXY", "all_proxy")
	httpProxy := proxyValue("HTTP_PROXY", "http_proxy")
	if httpProxy == "" {
		httpProxy = allProxy
	}
	httpsProxy := proxyValue("HTTPS_PROXY", "https_proxy")
	if httpsProxy == "" {
		httpsProxy = allProxy
	}
	noProxy := firstSafeNoProxyEnvironmentValue(values, "NO_PROXY", "no_proxy")

	out := make([]string, 0, 4)
	for _, item := range []struct {
		name  string
		value string
	}{
		{name: "HTTP_PROXY", value: httpProxy},
		{name: "HTTPS_PROXY", value: httpsProxy},
		{name: "ALL_PROXY", value: allProxy},
		{name: "NO_PROXY", value: noProxy},
	} {
		if item.value != "" {
			out = append(out, item.name+"="+item.value)
		}
	}
	return out
}

func firstSafeNoProxyEnvironmentValue(values map[string]string, keys ...string) string {
	for _, key := range keys {
		value := strings.TrimSpace(values[key])
		if value != "" && !strings.ContainsAny(value, "\x00\r\n@") {
			return value
		}
	}
	return ""
}

func validServiceProxyURL(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "\x00\r\n") || valueEmbedsCredentials(value) {
		return false
	}
	candidate := value
	if !strings.Contains(candidate, "://") {
		candidate = "http://" + candidate
	}
	parsed, err := url.Parse(candidate)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil {
		return false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "socks5", "socks5h":
		return true
	default:
		return false
	}
}
