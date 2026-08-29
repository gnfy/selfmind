package cliapp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/buildinfo"
	"selfmind/internal/gateway/api"
	"selfmind/internal/platform/config"
	gatewayrt "selfmind/internal/runtime/gateway"
)

// reconcileManagedGateway is the sole shutdown-to-ownership transition used by
// setup and the explicit service command. It never force-stops active work.
func (a *App) reconcileManagedGateway() (gatewayServiceInstallReceipt, error) {
	return a.reconcileManagedGatewayWithEnvironment(nil)
}

// persistManagedGatewayReceipt commits the verified service generation after
// any managed replacement path. Without this final ownership write,
// env refresh can start the right daemon yet make subsequent status checks
// report Runtime Degraded against the superseded generation.
func (a *App) persistManagedGatewayReceipt(receipt gatewayServiceInstallReceipt) error {
	if strings.TrimSpace(receipt.Manager) == "" || strings.TrimSpace(receipt.Generation) == "" {
		return fmt.Errorf("managed Gateway receipt is incomplete")
	}
	cfg, err := config.LoadConfig(config.Options{Path: a.configPath})
	if err != nil {
		return err
	}
	statePath := onboardingStatePath(cfg, a.configPath)
	state, err := loadOnboardingState(statePath)
	if err != nil {
		return err
	}
	state.BackgroundMode = "managed"
	state.BackgroundManager = receipt.Manager
	state.ServiceGeneration = receipt.Generation
	state.GatewayVerifiedAt = time.Now().UTC()
	return saveOnboardingState(statePath, state)
}

// reconcileManagedGatewayWithEnvironment performs the same safe managed
// replacement while rendering the service from an explicitly sampled login
// environment. Nil keeps the ordinary install/update behavior. The sample is
// filtered by gatewayServiceInstall before any value reaches a service file.
func (a *App) reconcileManagedGatewayWithEnvironment(parent []string) (gatewayServiceInstallReceipt, error) {
	if a.managedGatewayReconcile != nil {
		return a.managedGatewayReconcile()
	}
	if err := gatewayServicePreflight(); err != nil {
		return gatewayServiceInstallReceipt{}, err
	}
	previousPID := gatewayrt.RunningPID(a.gatewayDataDir())
	timeout := gatewayrt.ResolveDrainTimeout() + 10*time.Second
	ctx, cancel := contextWithTimeout(a.ctx, timeout)
	err := gatewayrt.RequestShutdown(ctx, gatewayrt.StopOptions{
		URL: a.gatewayURL(), DataDir: a.gatewayDataDir(), Timeout: timeout,
		Reason: api.ShutdownReasonServiceReconcile, WaitForSafeBoundary: true,
	})
	cancel()
	if err != nil {
		if status, statusErr := a.readOnboardingGatewayStatus(); statusErr == nil &&
			compatibleOnboardingGateway(status, buildinfo.Version, a.configPath) &&
			(errors.Is(err, gatewayrt.ErrShutdownDeferred) || errors.Is(err, context.DeadlineExceeded)) {
			activeRuns := status.ActiveRunCount
			if activeRuns < 1 {
				activeRuns = 1
			}
			return gatewayServiceInstallReceipt{}, &managedGatewayRepairDeferred{ActiveRuns: activeRuns}
		}
		return gatewayServiceInstallReceipt{}, fmt.Errorf("wait for the current Gateway to finish safely: %w", err)
	}
	releaseCtx, releaseCancel := contextWithTimeout(a.ctx, 10*time.Second)
	err = gatewayrt.WaitForOwnerRelease(releaseCtx, a.gatewayDataDir())
	releaseCancel()
	if err != nil {
		return gatewayServiceInstallReceipt{}, fmt.Errorf("wait for the current Gateway runtime lock: %w", err)
	}
	receipt, err := gatewayServiceInstall(a.configPath, previousPID, parent)
	if err != nil {
		return gatewayServiceInstallReceipt{}, err
	}
	ctx, cancel = contextWithTimeout(a.ctx, 15*time.Second)
	defer cancel()
	if _, err := gatewayrt.WaitForRunning(ctx, gatewayrt.EnsureOptions{ConfigPath: a.configPath, Timeout: 12 * time.Second}); err != nil {
		return gatewayServiceInstallReceipt{}, err
	}
	if err := a.verifyOnboardingGateway(receipt); err != nil {
		return gatewayServiceInstallReceipt{}, err
	}
	return receipt, nil
}
