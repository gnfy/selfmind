//go:build !darwin

package cliapp

import "fmt"

func gatewayServiceInstall(string) (string, error) {
	return "", fmt.Errorf("launchd service management is only available on macOS")
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

func gatewayServiceDoctorLine() string {
	return ""
}
