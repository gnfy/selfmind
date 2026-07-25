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
