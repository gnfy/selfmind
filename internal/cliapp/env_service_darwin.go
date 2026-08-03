//go:build darwin

package cliapp

import (
	"os"
)

// launchdManagesGateway reports whether a launchd agent owns the daemon. It
// matters for `env refresh`: a launchd plist pins HOME and PATH, so a refreshed
// environment is not adopted by restarting the process — the service definition
// has to be reinstalled.
func launchdManagesGateway() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	_, statErr := os.Stat(gatewayServicePlistPath(home))
	return statErr == nil
}
