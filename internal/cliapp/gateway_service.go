package cliapp

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"selfmind/internal/platform/config"
)

const gatewayLaunchdLabel = "com.selfmind.gateway"

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

var servicePassthroughEnvironmentKeyMarkers = []string{"PROXY", "CONFIG", "_HOME", "KUBECONFIG"}

var servicePassthroughEnvironmentExactKeys = map[string]bool{
	"SHELL": true, "LANG": true, "LC_ALL": true, "LC_CTYPE": true, "NO_PROXY": true, "no_proxy": true,
}

// servicePassthroughEnvironment picks only non-credential locations, locale,
// and proxy settings for an operating-system service definition. Service files
// may be readable by other local processes, so names or values that resemble
// credentials never cross this boundary.
func servicePassthroughEnvironment(parent []string) []string {
	out := make([]string, 0, 8)
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
	return out
}
