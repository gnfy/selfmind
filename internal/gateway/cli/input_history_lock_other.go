//go:build !unix

package cli

// lockHistoryFile is a no-op on platforms without flock; cross-process
// append/trim coordination is unavailable there (Linux is the official
// release target; single-process use is still safe via the writer goroutine).
func lockHistoryFile(path string, exclusive bool) func() { return func() {} }
