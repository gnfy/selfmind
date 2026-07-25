//go:build darwin

package cliapp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const gatewayLaunchdLogMaxBytes = 10 << 20

func gatewayServiceInstall(configPath string) (string, error) {
	command, err := resolvedGatewayServiceCommand(configPath)
	if err != nil {
		return "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	selfMindDir := filepath.Join(home, ".selfmind")
	launchAgentsDir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(selfMindDir, 0700); err != nil {
		return "", fmt.Errorf("create SelfMind home: %w", err)
	}
	if err := os.MkdirAll(launchAgentsDir, 0755); err != nil {
		return "", fmt.Errorf("create LaunchAgents directory: %w", err)
	}
	logPath := filepath.Join(selfMindDir, "gateway-launchd.log")
	if err := rotateLaunchdLog(logPath, gatewayLaunchdLogMaxBytes); err != nil {
		return "", fmt.Errorf("rotate launchd log: %w", err)
	}
	environment := map[string]string{
		"HOME":        home,
		"PATH":        launchdPath(),
		"SELF_CONFIG": commandConfigPath(command),
	}
	for _, key := range []string{"SELFMIND_INSTALL_METHOD", "SELFMIND_NPM_LAUNCHER", "SELFMIND_NODE_PATH"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			environment[key] = value
		}
	}
	plist, err := renderLaunchdPlist(launchdPlistData{
		Label:       gatewayLaunchdLabel,
		ProgramArgs: command,
		Environment: environment,
		StdoutPath:  logPath,
		StderrPath:  logPath,
	})
	if err != nil {
		return "", err
	}
	plistPath := gatewayServicePlistPath(home)
	existing, _ := os.ReadFile(plistPath)
	if err := writeFileAtomic(plistPath, plist, 0644); err != nil {
		return "", fmt.Errorf("write launchd service: %w", err)
	}
	domain := launchdDomain()
	loaded, err := gatewayServiceLoaded()
	if err != nil {
		return "", err
	}
	if loaded && bytes.Equal(existing, plist) {
		return plistPath, nil
	}
	_ = runLaunchctl(context.Background(), "bootout", domain+"/"+gatewayLaunchdLabel)
	if err := runLaunchctl(context.Background(), "bootstrap", domain, plistPath); err != nil {
		return "", err
	}
	if err := runLaunchctl(context.Background(), "kickstart", "-k", domain+"/"+gatewayLaunchdLabel); err != nil {
		return "", err
	}
	return plistPath, nil
}

func gatewayServiceStatus() (bool, string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return false, "", err
	}
	plistPath := gatewayServicePlistPath(home)
	if _, err := os.Stat(plistPath); errors.Is(err, os.ErrNotExist) {
		return false, "SelfMind launchd service is not installed.", nil
	} else if err != nil {
		return false, "", err
	}
	target := launchdDomain() + "/" + gatewayLaunchdLabel
	output, err := exec.Command("launchctl", "print", target).CombinedOutput()
	if err != nil {
		return true, fmt.Sprintf("SelfMind launchd service is installed but not loaded.\nPlist: %s", plistPath), nil
	}
	state := "loaded"
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "state = ") {
			state = strings.TrimSpace(strings.TrimPrefix(line, "state = "))
			break
		}
	}
	return true, fmt.Sprintf("SelfMind launchd service: %s\nPlist: %s", state, plistPath), nil
}

func gatewayServiceUninstall() (bool, string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return false, "", err
	}
	plistPath := gatewayServicePlistPath(home)
	if _, err := os.Stat(plistPath); errors.Is(err, os.ErrNotExist) {
		return false, "SelfMind launchd service is not installed.", nil
	} else if err != nil {
		return false, "", err
	}
	_ = runLaunchctl(context.Background(), "bootout", launchdDomain()+"/"+gatewayLaunchdLabel)
	if err := os.Remove(plistPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return true, "", fmt.Errorf("remove launchd service: %w", err)
	}
	return true, "SelfMind launchd service removed.", nil
}

func gatewayServiceStartIfInstalled(configPath string) (bool, string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return false, "", err
	}
	plistPath := gatewayServicePlistPath(home)
	if _, err := os.Stat(plistPath); errors.Is(err, os.ErrNotExist) {
		return false, "", nil
	} else if err != nil {
		return false, "", err
	}
	loaded, err := gatewayServiceLoaded()
	if err != nil {
		return true, "", err
	}
	if loaded {
		if err := rotateInstalledLaunchdLog(home); err != nil {
			return true, "", err
		}
		if err := runLaunchctl(context.Background(), "kickstart", launchdDomain()+"/"+gatewayLaunchdLabel); err != nil {
			return true, "", err
		}
		return true, "SelfMind launchd service started.", nil
	}
	if err := rotateInstalledLaunchdLog(home); err != nil {
		return true, "", err
	}
	domain := launchdDomain()
	if err := runLaunchctl(context.Background(), "bootstrap", domain, plistPath); err != nil {
		return true, "", err
	}
	if err := runLaunchctl(context.Background(), "kickstart", "-k", domain+"/"+gatewayLaunchdLabel); err != nil {
		return true, "", err
	}
	return true, "SelfMind launchd service started.\nPlist: " + plistPath, nil
}

func gatewayServiceRestartIfInstalled(configPath string) (bool, string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return false, "", err
	}
	plistPath := gatewayServicePlistPath(home)
	if _, err := os.Stat(plistPath); errors.Is(err, os.ErrNotExist) {
		return false, "", nil
	} else if err != nil {
		return false, "", err
	}
	if err := rotateInstalledLaunchdLog(home); err != nil {
		return true, "", err
	}
	domain := launchdDomain()
	loaded, err := gatewayServiceLoaded()
	if err != nil {
		return true, "", err
	}
	if !loaded {
		if err := runLaunchctl(context.Background(), "bootstrap", domain, plistPath); err != nil {
			return true, "", err
		}
	}
	if err := runLaunchctl(context.Background(), "kickstart", "-k", domain+"/"+gatewayLaunchdLabel); err != nil {
		return true, "", err
	}
	return true, "SelfMind launchd service restarted.", nil
}

func gatewayServiceStopIfInstalled() (bool, string, error) {
	installed, _, err := gatewayServiceStatus()
	if err != nil || !installed {
		return installed, "", err
	}
	if err := runLaunchctl(context.Background(), "bootout", launchdDomain()+"/"+gatewayLaunchdLabel); err != nil {
		return true, "", err
	}
	return true, "SelfMind launchd service stopped.", nil
}

func gatewayServiceSupported() bool {
	return true
}

func gatewayServiceLoaded() (bool, error) {
	output, err := exec.Command("launchctl", "print", launchdDomain()+"/"+gatewayLaunchdLabel).CombinedOutput()
	if err == nil {
		return true, nil
	}
	if len(strings.TrimSpace(string(output))) == 0 {
		return false, nil
	}
	return false, nil
}

func gatewayServiceDoctorLine() string {
	installed, _, err := gatewayServiceStatus()
	if err != nil {
		return "launchd=error"
	}
	if !installed {
		return "launchd=not-installed"
	}
	loaded, err := gatewayServiceLoaded()
	if err != nil {
		return "launchd=error"
	}
	if loaded {
		return "launchd=loaded"
	}
	return "launchd=installed-not-loaded"
}

func gatewayServicePlistPath(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", gatewayLaunchdLabel+".plist")
}

func launchdDomain() string {
	return "gui/" + strconv.Itoa(os.Getuid())
}

func launchdPath() string {
	paths := make([]string, 0, 12)
	seen := make(map[string]struct{})
	appendPath := func(value string) {
		for _, part := range strings.Split(value, ":") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if _, ok := seen[part]; ok {
				continue
			}
			seen[part] = struct{}{}
			paths = append(paths, part)
		}
	}
	appendPath(os.Getenv("PATH"))
	appendPath("/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin")
	return strings.Join(paths, ":")
}

func commandConfigPath(command []string) string {
	for i := 0; i+1 < len(command); i++ {
		if command[i] == "--config" {
			return command[i+1]
		}
	}
	return ""
}

func runLaunchctl(ctx context.Context, args ...string) error {
	output, err := exec.CommandContext(ctx, "launchctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".selfmind-launchd-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(mode); err != nil {
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

func rotateInstalledLaunchdLog(home string) error {
	return rotateLaunchdLog(filepath.Join(home, ".selfmind", "gateway-launchd.log"), gatewayLaunchdLogMaxBytes)
}

func rotateLaunchdLog(path string, maxBytes int64) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Size() <= maxBytes {
		return nil
	}
	rotated := path + ".1"
	if err := os.Remove(rotated); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(path, rotated)
}
