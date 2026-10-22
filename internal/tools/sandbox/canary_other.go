//go:build !darwin

package sandbox

// ProcessesUnderCanary exists only where seatbelt profiles carry a canary.
func ProcessesUnderCanary(string) ([]int, error) { return nil, nil }

// StopProcessesUnderCanary exists only where seatbelt profiles carry a canary.
func StopProcessesUnderCanary(string) ([]int, error) { return nil, nil }

// LeftoverStopSupported is false where no profile carries a canary.
func LeftoverStopSupported() bool { return false }
