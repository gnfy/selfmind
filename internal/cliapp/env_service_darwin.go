//go:build darwin

package cliapp

import (
	"os"
)

// managedServicePinsEnvironment reports whether a launchd agent owns the
// daemon. It matters for `env refresh`: a launchd plist pins the stable service
// environment, so the definition has to be reinstalled to adopt changes.
func managedServicePinsEnvironment() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	_, statErr := os.Stat(gatewayServicePlistPath(home))
	return statErr == nil
}
