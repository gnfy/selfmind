//go:build !darwin

package tools

const hostStatePlantVerb = "selfmind-plant-host-state"

// runHostStatePlantHelper exists only where in-place state writes do.
func runHostStatePlantHelper() {}
