//go:build linux

package cliapp

import "os"

// managedServicePinsEnvironment reports whether a systemd user unit owns the
// daemon. Restarting that unit reuses its Environment entries; it cannot adopt
// an environment sampled by the CLI without reinstalling the definition.
func managedServicePinsEnvironment() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	_, statErr := os.Stat(gatewaySystemdUnitPath(home))
	return statErr == nil
}
