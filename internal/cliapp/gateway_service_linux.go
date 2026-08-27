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

	appcore "selfmind/internal/app"
	gatewayrt "selfmind/internal/runtime/gateway"
)

func gatewayServiceInstall(configPath string, previousPID int) (gatewayServiceInstallReceipt, error) {
	if !gatewayServiceSupported() {
		return gatewayServiceInstallReceipt{}, fmt.Errorf("systemd user services are unavailable")
	}
	command, err := resolvedGatewayServiceCommand(configPath)
	if err != nil {
		return gatewayServiceInstallReceipt{}, err
	}
	generation, err := newGatewayServiceGeneration()
	if err != nil {
		return gatewayServiceInstallReceipt{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return gatewayServiceInstallReceipt{}, err
	}
	environment := map[string]string{
		"HOME":                       home,
		"PATH":                       linuxServicePath(),
		"SELF_CONFIG":                commandConfigPath(command),
		selfMindServiceManagerEnv:    "systemd",
		selfMindServiceGenerationEnv: generation,
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
		return gatewayServiceInstallReceipt{}, err
	}
	path := gatewaySystemdUnitPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return gatewayServiceInstallReceipt{}, err
	}
	if err := writeSystemdUnitAtomic(path, unit); err != nil {
		return gatewayServiceInstallReceipt{}, err
	}
	runtimeConfig, err := gatewayrt.LoadConfigForCLI(configPath)
	if err != nil {
		return gatewayServiceInstallReceipt{}, err
	}
	controller := systemdServiceController{
		inspect: inspectSystemdUnit,
		run: func(ctx context.Context, args ...string) error {
			output, err := runSystemctlContext(ctx, args...)
			if err != nil {
				return gatewayServiceCommandError("systemd", strings.Join(args, " "), err, output)
			}
			return err
		},
		proveAbsent: func(parent context.Context) error {
			ctx, cancel := context.WithTimeout(parent, 5*time.Second)
			defer cancel()
			if err := gatewayrt.WaitForRuntimeAbsence(ctx, appcore.ResolveDataDir(runtimeConfig), runtimeConfig.Gateway.Addr, previousPID); err != nil {
				return fmt.Errorf("prove the previous Gateway process, runtime lock, and listener are absent before systemd activation: %w", err)
			}
			return nil
		},
		pause: func(ctx context.Context, delay time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
				return nil
			}
		},
	}
	replaceCtx, replaceCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer replaceCancel()
	if err := controller.replace(replaceCtx, gatewaySystemdUnit); err != nil {
		return gatewayServiceInstallReceipt{}, err
	}
	return gatewayServiceInstallReceipt{Path: path, Manager: "systemd", Generation: generation}, nil
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
	active, err := systemdUnitActive()
	if err != nil {
		return true, "", err
	}
	if active {
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
	active, err := systemdUnitActive()
	return err == nil && active
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
	active, err := systemdUnitActive()
	if err != nil {
		return "systemd=error evidence=" + gatewayServiceEvidencePath()
	}
	if !active {
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
	output, err := runSystemctlContext(ctx, args...)
	if err != nil {
		return gatewayServiceCommandError("systemd", strings.Join(args, " "), err, output)
	}
	return nil
}

func runSystemctlContext(ctx context.Context, args ...string) ([]byte, error) {
	full := append([]string{"--user"}, args...)
	output, err := exec.CommandContext(ctx, "systemctl", full...).CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("personal systemd command %q failed: %w", strings.Join(args, " "), err)
	}
	return output, nil
}

func inspectSystemdUnit(ctx context.Context, unit string) (systemdUnitStatus, error) {
	output, err := runSystemctlContext(ctx, "show", "--property=LoadState", "--property=ActiveState", unit)
	if err != nil {
		lower := strings.ToLower(string(output))
		if strings.Contains(lower, "not-found") || strings.Contains(lower, "could not be found") {
			return systemdUnitStatus{LoadState: "not-found", ActiveState: "inactive"}, nil
		}
		return systemdUnitStatus{}, gatewayServiceCommandError("systemd", "show "+unit, err, output)
	}
	status := systemdUnitStatus{}
	for _, line := range strings.Split(string(output), "\n") {
		name, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch name {
		case "LoadState":
			status.LoadState = value
		case "ActiveState":
			status.ActiveState = value
		}
	}
	if strings.TrimSpace(status.LoadState) == "" || strings.TrimSpace(status.ActiveState) == "" {
		err := fmt.Errorf("systemctl returned incomplete state for %s", unit)
		return systemdUnitStatus{}, gatewayServiceCommandError("systemd", "show "+unit, err, output)
	}
	return status, nil
}

func systemdUnitActive() (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	status, err := inspectSystemdUnit(ctx, gatewaySystemdUnit)
	if err != nil {
		return false, err
	}
	return strings.EqualFold(strings.TrimSpace(status.ActiveState), "active"), nil
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
