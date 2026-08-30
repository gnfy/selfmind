//go:build !unix

package modelchange

// Native Windows is unsupported. Keep the package buildable for tooling while
// process-local coordination remains the only available guarantee there.
func lockStateFile(string) (func(), error) { return func() {}, nil }
