package cliapp

import (
	"selfmind/internal/buildinfo"
	"selfmind/internal/gateway/api"
	"selfmind/internal/platform/config"
)

type managedBackgroundStatus string

const (
	managedBackgroundAbsent      managedBackgroundStatus = "absent"
	managedBackgroundStarting    managedBackgroundStatus = "starting"
	managedBackgroundHealthy     managedBackgroundStatus = "managed healthy"
	managedBackgroundUnhealthy   managedBackgroundStatus = "managed unhealthy"
	managedBackgroundConflicting managedBackgroundStatus = "conflicting/unowned"
	managedBackgroundDegraded    managedBackgroundStatus = "Runtime Degraded"
)

func classifyManagedBackground(
	installed bool,
	jobRunning bool,
	receipt onboardingState,
	status *api.GatewayStatusResponse,
	expectedVersion string,
	expectedConfigPath string,
) managedBackgroundStatus {
	compatible := status != nil && compatibleOnboardingGateway(*status, expectedVersion, expectedConfigPath)
	if receipt.BackgroundMode == "managed" && compatible &&
		(!jobRunning || !managedGatewayOwned(receipt, true, status.Runtime, expectedVersion, expectedConfigPath)) {
		return managedBackgroundDegraded
	}
	if !installed {
		return managedBackgroundAbsent
	}
	if !jobRunning {
		return managedBackgroundUnhealthy
	}
	if status == nil {
		return managedBackgroundStarting
	}
	if managedGatewayOwned(receipt, true, status.Runtime, expectedVersion, expectedConfigPath) {
		return managedBackgroundHealthy
	}
	return managedBackgroundConflicting
}

func (a *App) currentManagedBackgroundStatus(installed, jobRunning bool) managedBackgroundStatus {
	var receipt onboardingState
	if cfg, err := config.LoadConfig(config.Options{Path: a.configPath}); err == nil {
		receipt, _ = loadOnboardingState(onboardingStatePath(cfg, a.configPath))
	}
	var live *api.GatewayStatusResponse
	if status, err := a.readOnboardingGatewayStatus(); err == nil {
		live = &status
	}
	return classifyManagedBackground(installed, jobRunning, receipt, live, buildinfo.Version, a.configPath)
}

func managedBackgroundStatusLine(status managedBackgroundStatus) string {
	switch status {
	case managedBackgroundHealthy:
		return "Managed background: healthy (Service Ownership verified)"
	case managedBackgroundDegraded:
		return "Managed background: Runtime Degraded (compatible Gateway; Service Ownership not established)"
	case managedBackgroundStarting:
		return "Managed background: starting (service is running; Gateway is not ready yet)"
	case managedBackgroundUnhealthy:
		return "Managed background: unhealthy (service job is not running)"
	case managedBackgroundConflicting:
		return "Managed background: conflicting/unowned Gateway"
	default:
		return "Managed background: absent"
	}
}
