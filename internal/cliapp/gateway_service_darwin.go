//go:build darwin

package cliapp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	appcore "selfmind/internal/app"
	gatewayrt "selfmind/internal/runtime/gateway"
)

const gatewayLaunchdLogMaxBytes = 10 << 20

type launchdJobStatus struct {
	Loaded bool
	State  string
}

func (s launchdJobStatus) Healthy() bool {
	return s.Loaded && strings.EqualFold(strings.TrimSpace(s.State), "running")
}

type launchdServiceController struct {
	inspect     func(context.Context, string) (launchdJobStatus, error)
	run         func(context.Context, ...string) error
	proveAbsent func(context.Context) error
	pause       func(context.Context, time.Duration) error
}

func newLaunchdServiceController() launchdServiceController {
	return launchdServiceController{
		inspect: inspectLaunchdJob,
		run:     runLaunchctl,
		pause: func(ctx context.Context, delay time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
				return nil
			}
		},
	}
}

func (c launchdServiceController) replace(ctx context.Context, domain, plistPath string) error {
	target := domain + "/" + gatewayLaunchdLabel
	status, err := c.inspect(ctx, target)
	if err != nil {
		return err
	}
	if status.Loaded {
		if err := c.run(ctx, "bootout", target); err != nil {
			if latest, inspectErr := c.inspect(ctx, target); inspectErr != nil || latest.Loaded {
				return fmt.Errorf("stop existing personal launchd service: %s", launchdErrorDetail(err))
			}
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, err = c.inspect(ctx, target)
		if err != nil {
			return err
		}
		if !status.Loaded {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("existing personal launchd service did not finish stopping; administrator access is not required, retry after current SelfMind work finishes")
		}
		if err := c.pause(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
	bootstrap := []string{"bootstrap", domain, plistPath}
	if c.proveAbsent != nil {
		if err := c.proveAbsent(ctx); err != nil {
			return err
		}
	}
	if err := c.run(ctx, bootstrap...); err != nil {
		status, inspectErr := c.inspect(ctx, target)
		if inspectErr != nil {
			return inspectErr
		}
		if status.Loaded {
			return fmt.Errorf("register personal launchd service: %s", launchdErrorDetail(err))
		}
		if c.proveAbsent != nil {
			if absentErr := c.proveAbsent(ctx); absentErr != nil {
				return absentErr
			}
		}
		if retryErr := c.run(ctx, bootstrap...); retryErr != nil {
			return fmt.Errorf("register personal launchd service after one safe retry: %s; administrator access is not required", launchdErrorDetail(retryErr))
		}
	}
	if err := c.run(ctx, "kickstart", "-k", target); err != nil {
		return fmt.Errorf("start personal launchd service: %s", launchdErrorDetail(err))
	}
	return nil
}

func launchdErrorDetail(err error) string {
	if err == nil {
		return "unknown launchd error"
	}
	lower := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lower, "details saved to"):
		return strings.Split(err.Error(), "\n")[0]
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(lower, "timed out"):
		return "the personal launchd operation timed out"
	case strings.Contains(lower, "input/output"):
		return "launchd reported an inconsistent personal service state"
	case strings.Contains(lower, "operation not permitted") || strings.Contains(lower, "permission"):
		return "launchd denied the personal service operation"
	default:
		return "the personal launchd operation failed"
	}
}

func gatewayServiceInstall(configPath string, previousPID int) (gatewayServiceInstallReceipt, error) {
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
		return gatewayServiceInstallReceipt{}, fmt.Errorf("resolve user home: %w", err)
	}
	selfMindDir := filepath.Join(home, ".selfmind")
	launchAgentsDir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(selfMindDir, 0700); err != nil {
		return gatewayServiceInstallReceipt{}, fmt.Errorf("create SelfMind home: %w", err)
	}
	if err := os.MkdirAll(launchAgentsDir, 0755); err != nil {
		return gatewayServiceInstallReceipt{}, fmt.Errorf("create LaunchAgents directory: %w", err)
	}
	logPath := filepath.Join(selfMindDir, "gateway-launchd.log")
	if err := rotateLaunchdLog(logPath, gatewayLaunchdLogMaxBytes); err != nil {
		return gatewayServiceInstallReceipt{}, fmt.Errorf("rotate launchd log: %w", err)
	}
	environment := map[string]string{
		"HOME":                       home,
		"PATH":                       launchdPath(),
		"SELF_CONFIG":                commandConfigPath(command),
		selfMindServiceManagerEnv:    "launchd",
		selfMindServiceGenerationEnv: generation,
	}
	for _, key := range []string{"SELFMIND_INSTALL_METHOD", "SELFMIND_NPM_LAUNCHER", "SELFMIND_NODE_PATH"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			environment[key] = value
		}
	}
	// A launchd agent does NOT inherit the installing shell's environment, so a
	// plist carrying only HOME/PATH left the daemon without the operator's proxy
	// settings or tool configuration locations — tools then failed on macOS for
	// reasons that looked nothing like a missing variable. Carry the generic,
	// non-credential ones across.
	//
	// The plist is world-readable (0644), so a credential-shaped name or a value
	// that embeds credentials is never written.
	for _, entry := range servicePassthroughEnvironment(os.Environ()) {
		if name, value, ok := strings.Cut(entry, "="); ok {
			if _, exists := environment[name]; !exists {
				environment[name] = value
			}
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
		return gatewayServiceInstallReceipt{}, err
	}
	plistPath := gatewayServicePlistPath(home)
	if err := writeFileAtomic(plistPath, plist, 0644); err != nil {
		return gatewayServiceInstallReceipt{}, fmt.Errorf("write launchd service: %w", err)
	}
	domain := launchdDomain()
	runtimeConfig, err := gatewayrt.LoadConfigForCLI(configPath)
	if err != nil {
		return gatewayServiceInstallReceipt{}, err
	}
	controller := newLaunchdServiceController()
	controller.proveAbsent = func(parent context.Context) error {
		ctx, cancel := context.WithTimeout(parent, 5*time.Second)
		defer cancel()
		if err := gatewayrt.WaitForRuntimeAbsence(ctx, appcore.ResolveDataDir(runtimeConfig), runtimeConfig.Gateway.Addr, previousPID); err != nil {
			return fmt.Errorf("prove the previous Gateway process, runtime lock, and listener are absent before launchd bootstrap: %w", err)
		}
		return nil
	}
	replaceCtx, replaceCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer replaceCancel()
	if err := controller.replace(replaceCtx, domain, plistPath); err != nil {
		return gatewayServiceInstallReceipt{}, err
	}
	return gatewayServiceInstallReceipt{Path: plistPath, Manager: "launchd", Generation: generation}, nil
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

func gatewayServicePreflight() error { return nil }

func gatewayServiceHealthy() bool {
	status, err := inspectLaunchdJob(context.Background(), launchdDomain()+"/"+gatewayLaunchdLabel)
	return err == nil && status.Healthy()
}

func gatewayServiceKind() string {
	return "launchd"
}

func gatewayServiceLoaded() (bool, error) {
	status, err := inspectLaunchdJob(context.Background(), launchdDomain()+"/"+gatewayLaunchdLabel)
	return status.Loaded, err
}

func inspectLaunchdJob(ctx context.Context, target string) (launchdJobStatus, error) {
	output, err := exec.CommandContext(ctx, "launchctl", "print", target).CombinedOutput()
	status, parseErr := parseLaunchdJobStatus(output, err)
	if parseErr != nil {
		return status, gatewayServiceCommandError("launchd", "print "+target, err, output)
	}
	return status, nil
}

func parseLaunchdJobStatus(output []byte, commandErr error) (launchdJobStatus, error) {
	text := strings.TrimSpace(string(output))
	lower := strings.ToLower(text)
	if commandErr != nil {
		if strings.Contains(lower, "could not find service") || strings.Contains(lower, "service not found") {
			return launchdJobStatus{}, nil
		}
		detail := strings.TrimSpace(strings.Join([]string{commandErr.Error(), text}, ": "))
		return launchdJobStatus{}, fmt.Errorf("inspect personal launchd service: %s", launchdErrorDetail(errors.New(detail)))
	}
	status := launchdJobStatus{Loaded: true, State: "loaded"}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "state = ") {
			status.State = strings.TrimSpace(strings.TrimPrefix(line, "state = "))
			break
		}
	}
	return status, nil
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

func runLaunchctl(ctx context.Context, args ...string) error {
	output, err := exec.CommandContext(ctx, "launchctl", args...).CombinedOutput()
	if err != nil {
		return gatewayServiceCommandError("launchd", strings.Join(args, " "), err, output)
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
