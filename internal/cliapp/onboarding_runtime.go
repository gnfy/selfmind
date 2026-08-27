package cliapp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"selfmind/internal/buildinfo"
	"selfmind/internal/gateway/api"
	"selfmind/internal/modelchange"
	"selfmind/internal/platform/config"
	gatewayrt "selfmind/internal/runtime/gateway"
	"selfmind/internal/tools"
)

type onboardingRuntimeChoice struct {
	WorkspacePath  string
	ApprovalMode   string
	BackgroundMode string
}

type onboardingWorkspace struct {
	ID   string
	Name string
	Path string
}

type managedGatewayRepairDeferred struct {
	ActiveRuns int
}

func (e *managedGatewayRepairDeferred) Error() string {
	return fmt.Sprintf("managed background repair is deferred while %d active run(s) finish", e.ActiveRuns)
}

func (a *App) runOnboardingRuntimeStep(state *onboardingState, options onboardingOptions) int {
	choice, err := a.defaultOnboardingRuntimeChoice(*state)
	if err != nil {
		fmt.Fprintf(a.stderr, "Workspace setup failed: %v\n", err)
		return 1
	}
	if onboardingWorkspaceNeedsExplicitChoice(choice.WorkspacePath) {
		if options.NonInteractive {
			fmt.Fprintln(a.stderr, "A project workspace must be selected explicitly; the filesystem root and home directory are not safe defaults.")
			return 1
		}
		workspace, promptErr := a.promptInput("Project workspace", "")
		if promptErr != nil {
			fmt.Fprintln(a.stderr, promptErr)
			return 1
		}
		choice.WorkspacePath, err = canonicalOnboardingWorkspace(workspace)
		if err != nil || onboardingWorkspaceNeedsExplicitChoice(choice.WorkspacePath) {
			if err == nil {
				err = fmt.Errorf("choose a project directory instead of the filesystem root or home directory")
			}
			fmt.Fprintf(a.stderr, "Workspace setup failed: %v\n", err)
			return 1
		}
	}
	a.printRuntimeChoice(choice)
	if !options.NonInteractive {
		accepted, promptErr := a.promptConfirm("Start SelfMind with these settings?", true)
		if promptErr != nil {
			fmt.Fprintln(a.stderr, promptErr)
			return 1
		}
		if !accepted {
			choice, err = a.customizeOnboardingRuntimeChoice(choice)
			if err != nil {
				fmt.Fprintln(a.stderr, err)
				return 1
			}
			a.printRuntimeChoice(choice)
		}
	}

	fmt.Fprintln(a.stdout, "Setting up SelfMind...")
	receipt, err := a.prepareOnboardingGateway(choice.BackgroundMode)
	degraded := false
	if err != nil {
		var deferred *managedGatewayRepairDeferred
		if errors.As(err, &deferred) {
			degraded = true
			fmt.Fprintln(a.stderr, "SelfMind is available, but background startup still needs repair.")
			fmt.Fprintf(a.stderr, "The managed service will be repaired after %d active run(s) finish.\n", deferred.ActiveRuns)
		} else {
			fmt.Fprintf(a.stderr, "Background setup failed: %v\n", err)
			return 1
		}
	}
	if !options.SkipModel {
		if err := a.verifyOnboardingModelsFromDaemon(state.AuxiliaryDegraded); err != nil {
			fmt.Fprintf(a.stderr, "Daemon model verification failed: %v\n", err)
			return 1
		}
	}
	workspace, err := a.registerOnboardingWorkspace(choice.WorkspacePath)
	if err != nil {
		fmt.Fprintf(a.stderr, "Workspace setup failed: %v\n", err)
		return 1
	}
	if err := a.setOnboardingApprovalMode(choice.ApprovalMode, workspace.ID); err != nil {
		fmt.Fprintf(a.stderr, "Safety mode setup failed: %v\n", err)
		return 1
	}

	state.BackgroundMode = choice.BackgroundMode
	state.BackgroundManager = receipt.Manager
	state.ServiceGeneration = receipt.Generation
	state.GatewayVerifiedAt = time.Now().UTC()
	state.WorkspaceID = workspace.ID
	state.WorkspaceName = workspace.Name
	state.WorkspacePath = workspace.Path
	state.WorkspaceTrusted = true
	state.ApprovalMode = choice.ApprovalMode
	if degraded {
		fmt.Fprintln(a.stdout, "  ! Background startup needs repair")
	} else {
		fmt.Fprintln(a.stdout, "  ✓ Background service healthy")
	}
	fmt.Fprintln(a.stdout, "  ✓ Workspace ready")
	fmt.Fprintln(a.stdout, "  ✓ Safety policy active")
	fmt.Fprintln(a.stdout)
	return 0
}

func (a *App) defaultOnboardingRuntimeChoice(state onboardingState) (onboardingRuntimeChoice, error) {
	workspace := strings.TrimSpace(state.WorkspacePath)
	if workspace == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return onboardingRuntimeChoice{}, err
		}
		workspace = cwd
	}
	canonical, err := canonicalOnboardingWorkspace(workspace)
	if err != nil {
		return onboardingRuntimeChoice{}, err
	}
	mode := strings.TrimSpace(state.ApprovalMode)
	if !isValidApprovalMode(mode) {
		mode = string(tools.DefaultApprovalMode)
	}
	background := strings.TrimSpace(state.BackgroundMode)
	if background != "managed" && background != "on-demand" {
		if gatewayServiceSupported() {
			background = "managed"
		} else {
			background = "on-demand"
		}
	}
	return onboardingRuntimeChoice{WorkspacePath: canonical, ApprovalMode: mode, BackgroundMode: background}, nil
}

func (a *App) printRuntimeChoice(choice onboardingRuntimeChoice) {
	fmt.Fprintln(a.stdout, "Run setup")
	fmt.Fprintln(a.stdout)
	fmt.Fprintf(a.stdout, "  Workspace:  %s\n", choice.WorkspacePath)
	fmt.Fprintln(a.stdout, "              Repository instructions trusted; tool approvals still apply")
	fmt.Fprintf(a.stdout, "  Safety:     %s\n", choice.ApprovalMode)
	fmt.Fprintf(a.stdout, "  Protection: %s\n", onboardingProtectionSummary())
	background := "On demand"
	if choice.BackgroundMode == "managed" {
		background = "Enabled"
	}
	fmt.Fprintf(a.stdout, "  Background: %s\n", background)
	fmt.Fprintln(a.stdout)
}

func (a *App) customizeOnboardingRuntimeChoice(current onboardingRuntimeChoice) (onboardingRuntimeChoice, error) {
	workspace, err := a.promptInput("Workspace", current.WorkspacePath)
	if err != nil {
		return current, err
	}
	current.WorkspacePath, err = canonicalOnboardingWorkspace(workspace)
	if err != nil {
		return current, err
	}
	modeIndex, err := a.promptChoice("Safety mode:", []string{
		"Cautious (on-request)",
		"Smart (recommended)",
		"Automatic edits (auto-edit)",
	})
	if err != nil {
		return current, err
	}
	current.ApprovalMode = []string{"on-request", "smart", "auto-edit"}[modeIndex]
	if gatewayServiceSupported() {
		managed, confirmErr := a.promptConfirm("Enable background startup?", true)
		if confirmErr != nil {
			return current, confirmErr
		}
		if managed {
			current.BackgroundMode = "managed"
		} else {
			current.BackgroundMode = "on-demand"
		}
	}
	return current, nil
}

func onboardingProtectionSummary() string {
	if runtime.GOOS == "linux" && tools.ExecSandboxAvailable() {
		return "isolated execution with approval controls"
	}
	return "approval-controlled host execution"
}

func (a *App) prepareOnboardingGateway(mode string) (gatewayServiceInstallReceipt, error) {
	if mode == "managed" {
		if !gatewayServiceSupported() {
			return gatewayServiceInstallReceipt{}, fmt.Errorf("the operating-system background service is unavailable; choose on-demand mode")
		}
		return a.reconcileManagedGateway()
	} else {
		if gatewayServiceSupported() {
			if installed, _, err := gatewayServiceStatus(); err != nil {
				return gatewayServiceInstallReceipt{}, err
			} else if installed {
				timeout := gatewayrt.ResolveDrainTimeout() + 10*time.Second
				ctx, cancel := contextWithTimeout(a.ctx, timeout)
				err = gatewayrt.RequestShutdown(ctx, gatewayrt.StopOptions{
					URL: a.gatewayURL(), DataDir: a.gatewayDataDir(), Timeout: timeout,
					Reason: api.ShutdownReasonServiceReconcile, WaitForSafeBoundary: true,
				})
				cancel()
				if err != nil {
					return gatewayServiceInstallReceipt{}, err
				}
				releaseCtx, releaseCancel := contextWithTimeout(a.ctx, 10*time.Second)
				err = gatewayrt.WaitForOwnerRelease(releaseCtx, a.gatewayDataDir())
				releaseCancel()
				if err != nil {
					return gatewayServiceInstallReceipt{}, err
				}
				if _, _, err := gatewayServiceUninstall(); err != nil {
					return gatewayServiceInstallReceipt{}, err
				}
			}
		}
	}
	ctx, cancel := contextWithTimeout(a.ctx, 15*time.Second)
	defer cancel()
	if _, err := gatewayrt.EnsureRunning(ctx, gatewayrt.EnsureOptions{ConfigPath: a.configPath, Timeout: 12 * time.Second}); err != nil {
		return gatewayServiceInstallReceipt{}, err
	}
	receipt := gatewayServiceInstallReceipt{Manager: "on-demand"}
	if err := a.verifyOnboardingGateway(receipt); err != nil {
		return gatewayServiceInstallReceipt{}, err
	}
	return receipt, nil
}

func (a *App) verifyOnboardingGateway(receipt gatewayServiceInstallReceipt) error {
	status, err := a.readOnboardingGatewayStatus()
	if err != nil {
		return err
	}
	if err := validateRestartedDaemonHealth(status, buildinfo.Version); err != nil {
		return err
	}
	if receipt.Generation != "" {
		state := onboardingState{BackgroundMode: "managed", BackgroundManager: receipt.Manager, ServiceGeneration: receipt.Generation}
		if !managedGatewayOwned(state, gatewayServiceHealthy(), status.Runtime, buildinfo.Version, a.configPath) {
			return fmt.Errorf("the Gateway is reachable but is not owned by the installed %s service generation", receipt.Manager)
		}
	}
	return nil
}

func (a *App) readOnboardingGatewayStatus() (api.GatewayStatusResponse, error) {
	if a.onboardingGatewayStatus != nil {
		return a.onboardingGatewayStatus(a.ctx)
	}
	ctx, cancel := contextWithTimeout(a.ctx, 3*time.Second)
	defer cancel()
	data, statusCode, err := gatewayrt.RequestStatus(ctx, a.gatewayURL())
	if err != nil {
		return api.GatewayStatusResponse{}, err
	}
	if statusCode >= http.StatusBadRequest {
		return api.GatewayStatusResponse{}, fmt.Errorf("gateway status returned HTTP %d", statusCode)
	}
	var status api.GatewayStatusResponse
	if err := json.Unmarshal(data, &status); err != nil {
		return api.GatewayStatusResponse{}, err
	}
	return status, nil
}

func compatibleOnboardingGateway(status api.GatewayStatusResponse, expectedVersion, expectedConfigPath string) bool {
	if validateRestartedDaemonHealth(status, expectedVersion) != nil ||
		strings.TrimSpace(status.Runtime.ConfigPath) == "" ||
		strings.TrimSpace(status.Runtime.ModelRouteFingerprint) == "" {
		return false
	}
	wantConfig, _ := config.ResolveConfigPath(expectedConfigPath)
	gotConfig, _ := config.ResolveConfigPath(status.Runtime.ConfigPath)
	if !samePath(gotConfig, wantConfig) {
		return false
	}
	cfg, err := config.LoadConfig(config.Options{Path: expectedConfigPath})
	if err != nil {
		return false
	}
	return status.Runtime.ModelRouteFingerprint == modelchange.SnapshotFromConfig(cfg).Fingerprint()
}

func (a *App) verifyOnboardingModelsFromDaemon(auxiliaryDegraded bool) error {
	roles := []string{"primary"}
	if !auxiliaryDegraded {
		roles = append(roles, "auxiliary")
	}
	for _, role := range roles {
		body, _ := json.Marshal(api.ModelProbeRequest{Role: role})
		req, err := http.NewRequestWithContext(a.ctx, http.MethodPost, a.gatewayURL()+"/v1/gateway/model/probe", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		a.attachGatewayAuth(req)
		a.attachLocalControlAuth(req)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		data, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			return readErr
		}
		if resp.StatusCode >= http.StatusBadRequest {
			return fmt.Errorf("%s", gatewayErrorLine(resp.Status, data))
		}
		var result api.ModelProbeResponse
		if err := json.Unmarshal(data, &result); err != nil {
			return err
		}
		if !result.OK {
			return fmt.Errorf("%s model: %s", role, firstNonEmpty(result.Error, "probe failed"))
		}
	}
	return nil
}

func (a *App) registerOnboardingWorkspace(path string) (onboardingWorkspace, error) {
	name := filepath.Base(path)
	req := api.WorkspaceRegisterRequest{
		TenantID: os.Getenv("SELF_TENANT_ID"), Platform: "cli",
		PlatformUserID: platformUserID(), DisplayName: platformUserID(),
		Name: name, LocalPath: path, AllowedRoots: []string{path},
	}
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(a.ctx, http.MethodPost, a.gatewayURL()+"/v1/workspaces/register", bytes.NewReader(body))
	if err != nil {
		return onboardingWorkspace{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	a.attachGatewayAuth(httpReq)
	a.attachLocalControlAuth(httpReq)
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return onboardingWorkspace{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		data, _ := io.ReadAll(resp.Body)
		return onboardingWorkspace{}, fmt.Errorf("%s", gatewayErrorLine(resp.Status, data))
	}
	var payload struct {
		Workspace struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			LocalPath string `json:"local_path"`
		} `json:"workspace"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return onboardingWorkspace{}, err
	}
	if strings.TrimSpace(payload.Workspace.ID) == "" {
		return onboardingWorkspace{}, fmt.Errorf("gateway returned no workspace id")
	}
	return onboardingWorkspace{ID: payload.Workspace.ID, Name: payload.Workspace.Name, Path: payload.Workspace.LocalPath}, nil
}

func (a *App) setOnboardingApprovalMode(mode, workspaceID string) error {
	req := api.MessageRequest{
		TenantID: os.Getenv("SELF_TENANT_ID"), Platform: "cli",
		PlatformUserID: platformUserID(), DisplayName: platformUserID(), Channel: "cli",
		Content: "/mode " + mode, WorkspaceID: workspaceID,
	}
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(a.ctx, http.MethodPost, a.gatewayURL()+"/v1/message", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	a.attachGatewayAuth(httpReq)
	a.attachLocalControlAuth(httpReq)
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s", gatewayErrorLine(resp.Status, data))
	}
	return nil
}

func (a *App) expectedBackgroundStateReady(state onboardingState) bool {
	if state.BackgroundMode != "managed" {
		return state.BackgroundMode == "on-demand"
	}
	if !gatewayServiceSupported() || state.BackgroundManager != gatewayServiceKind() {
		return false
	}
	serviceHealthy := gatewayServiceHealthy()
	if a.managedServiceHealthy != nil {
		serviceHealthy = a.managedServiceHealthy()
	}
	if !serviceHealthy {
		return false
	}
	status, err := a.readOnboardingGatewayStatus()
	if err != nil {
		return false
	}
	return managedGatewayOwned(state, true, status.Runtime, buildinfo.Version, a.configPath)
}

func managedGatewayOwned(state onboardingState, serviceHealthy bool, runtimeInfo api.GatewayRuntimeInfo, expectedVersion, expectedConfigPath string) bool {
	if !serviceHealthy || state.BackgroundMode != "managed" || strings.TrimSpace(state.BackgroundManager) == "" || strings.TrimSpace(state.ServiceGeneration) == "" {
		return false
	}
	if strings.TrimSpace(runtimeInfo.ConfigPath) == "" || strings.TrimSpace(runtimeInfo.Version) == "" || strings.TrimSpace(expectedVersion) == "" {
		return false
	}
	resolvedConfig, _ := config.ResolveConfigPath(expectedConfigPath)
	runtimeConfig, _ := config.ResolveConfigPath(runtimeInfo.ConfigPath)
	return strings.EqualFold(strings.TrimSpace(runtimeInfo.ServiceManager), strings.TrimSpace(state.BackgroundManager)) &&
		strings.TrimSpace(runtimeInfo.ServiceGeneration) == strings.TrimSpace(state.ServiceGeneration) &&
		strings.TrimSpace(runtimeInfo.Version) == strings.TrimSpace(expectedVersion) &&
		samePath(runtimeConfig, resolvedConfig)
}
