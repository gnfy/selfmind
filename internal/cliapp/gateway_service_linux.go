//go:build linux

package cliapp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const gatewaySystemdUnit = "selfmind-gateway.service"

func gatewayServiceInstall(configPath string) (string, error) {
	if !gatewayServiceSupported() {
		return "", fmt.Errorf("systemd user services are unavailable")
	}
	command, err := resolvedGatewayServiceCommand(configPath)
	if err != nil {
		return "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	environment := map[string]string{
		"HOME":        home,
		"PATH":        linuxServicePath(),
		"SELF_CONFIG": commandConfigPath(command),
	}
	for _, key := range []string{"SELFMIND_INSTALL_METHOD", "SELFMIND_NPM_LAUNCHER", "SELFMIND_NODE_PATH"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			environment[key] = value
		}
	}
	for _, entry := range servicePassthroughEnvironment(os.Environ()) {
		if name, value, ok := strings.Cut(entry, "="); ok {
			if _, exists := environment[name]; !exists {
				environment[name] = value
			}
		}
	}
	unit, err := renderSystemdUserUnit(systemdUnitData{
		Description: "SelfMind gateway", ProgramArgs: command, Environment: environment,
	})
	if err != nil {
		return "", err
	}
	path := gatewaySystemdUnitPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := writeSystemdUnitAtomic(path, unit); err != nil {
		return "", err
	}
	if err := runSystemctl("daemon-reload"); err != nil {
		return "", err
	}
	if err := runSystemctl("enable", "--now", gatewaySystemdUnit); err != nil {
		return "", err
	}
	if err := runSystemctl("restart", gatewaySystemdUnit); err != nil {
		return "", err
	}
	return path, nil
}

func gatewayServiceStatus() (bool, string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return false, "", err
	}
	path := gatewaySystemdUnitPath(home)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return false, "SelfMind background service is not installed.", nil
	} else if err != nil {
		return false, "", err
	}
	state := "installed but not running"
	if err := runSystemctl("is-active", "--quiet", gatewaySystemdUnit); err == nil {
		state = "running"
	}
	return true, fmt.Sprintf("SelfMind background service: %s\nUnit: %s", state, path), nil
}

func gatewayServiceUninstall() (bool, string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return false, "", err
	}
	path := gatewaySystemdUnitPath(home)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return false, "SelfMind background service is not installed.", nil
	} else if err != nil {
		return false, "", err
	}
	_ = runSystemctl("disable", "--now", gatewaySystemdUnit)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return true, "", err
	}
	if err := runSystemctl("daemon-reload"); err != nil {
		return true, "", err
	}
	return true, "SelfMind background service removed.", nil
}

func gatewayServiceStartIfInstalled(string) (bool, string, error) {
	installed, _, err := gatewayServiceStatus()
	if err != nil || !installed {
		return installed, "", err
	}
	if err := runSystemctl("start", gatewaySystemdUnit); err != nil {
		return true, "", err
	}
	return true, "SelfMind background service started.", nil
}

func gatewayServiceRestartIfInstalled(string) (bool, string, error) {
	installed, _, err := gatewayServiceStatus()
	if err != nil || !installed {
		return installed, "", err
	}
	if err := runSystemctl("restart", gatewaySystemdUnit); err != nil {
		return true, "", err
	}
	return true, "SelfMind background service restarted.", nil
}

func gatewayServiceStopIfInstalled() (bool, string, error) {
	installed, _, err := gatewayServiceStatus()
	if err != nil || !installed {
		return installed, "", err
	}
	if err := runSystemctl("stop", gatewaySystemdUnit); err != nil {
		return true, "", err
	}
	return true, "SelfMind background service stopped.", nil
}

func gatewayServiceSupported() bool {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "systemctl", "--user", "show-environment").Run() == nil
}

func gatewayServicePreflight() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", "selfmind.service").Run(); err == nil {
		return fmt.Errorf("a system-wide selfmind.service is already active; stop or reconfigure it before enabling the personal background service")
	}
	return nil
}

func gatewayServiceHealthy() bool {
	installed, _, err := gatewayServiceStatus()
	if err != nil || !installed {
		return false
	}
	return runSystemctl("is-active", "--quiet", gatewaySystemdUnit) == nil
}

func gatewayServiceKind() string { return "systemd" }

func gatewayServiceDoctorLine() string {
	installed, _, err := gatewayServiceStatus()
	if err != nil {
		return "systemd=error"
	}
	if !installed {
		return "systemd=not-installed"
	}
	if err := runSystemctl("is-active", "--quiet", gatewaySystemdUnit); err != nil {
		return "systemd=installed-not-running"
	}
	return "systemd=running"
}

func gatewaySystemdUnitPath(home string) string {
	return filepath.Join(home, ".config", "systemd", "user", gatewaySystemdUnit)
}

func runSystemctl(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	full := append([]string{"--user"}, args...)
	output, err := exec.CommandContext(ctx, "systemctl", full...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %w: %s", strings.Join(full, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func linuxServicePath() string {
	paths := []string{strings.TrimSpace(os.Getenv("PATH")), "/usr/local/bin", "/usr/bin", "/bin"}
	seen := map[string]bool{}
	var out []string
	for _, group := range paths {
		for _, path := range strings.Split(group, ":") {
			path = strings.TrimSpace(path)
			if path != "" && !seen[path] {
				seen[path] = true
				out = append(out, path)
			}
		}
	}
	return strings.Join(out, ":")
}

func writeSystemdUnitAtomic(path string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".selfmind-systemd-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}
