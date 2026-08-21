//go:build !linux && !darwin

package promptassets

import "os"

// Native Windows is unsupported. Keep prompt loading buildable on other
// platforms, where a portable owner identity is not exposed through os.FileInfo.
func promptPathOwnedByCurrentUser(os.FileInfo) bool {
	return true
}
