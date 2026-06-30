//go:build !unix

package modelruntime

// lockAuthFile is a no-op on platforms without flock; cross-process refresh
// coordination is unavailable there (the in-process singleflight still applies).
func lockAuthFile(path string) func() { return func() {} }
