//go:build !darwin && !linux

package cliapp

// Unsupported platforms use an on-demand daemon whose environment can be
// replaced directly by `env refresh --restart`.
func managedServicePinsEnvironment() bool { return false }
