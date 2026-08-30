//go:build !darwin && !linux

package cliapp

import "fmt"

func gatewayServiceInstall(string, int, []string) (gatewayServiceInstallReceipt, error) {
	return gatewayServiceInstallReceipt{}, fmt.Errorf("operating-system service management is unavailable")
}

func gatewayServiceStatus() (bool, string, error) {
	return false, "launchd service management is only available on macOS.", nil
}

func gatewayServiceUninstall() (bool, string, error) {
	return false, "launchd service management is only available on macOS.", nil
}

func gatewayServiceStartIfInstalled(string) (bool, string, error) {
	return false, "", nil
}

func gatewayServiceRestartIfInstalled(string) (bool, string, error) {
	return false, "", nil
}

func gatewayServiceStopIfInstalled() (bool, string, error) {
	return false, "", nil
}

func gatewayServiceSupported() bool {
	return false
}

func gatewayServicePreflight() error { return nil }

func gatewayServiceHealthy() bool { return false }

func gatewayServiceKind() string {
	return "on-demand"
}

func gatewayServiceDoctorLine() string {
	return ""
}
