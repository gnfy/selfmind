//go:build !darwin

package cliapp

// launchdManagesGateway is macOS-only. Elsewhere the daemon inherits the
// environment it was started with, which `env refresh --restart` replaces
// directly.
func launchdManagesGateway() bool { return false }
