//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package scratchhome

import (
	"errors"
	"io/fs"
)

// fileOwner cannot report an owner here, so no stable path is ever trusted.
func fileOwner(fs.FileInfo) (uint32, bool) { return 0, false }

// lockStable has no exclusive lock to take here; Fresh then gives every run a
// home of its own.
func lockStable(string, uint32, func(fs.FileInfo) (uint32, bool)) (func(), bool, error) {
	return nil, false, errors.New("exclusive locks are unavailable on this platform")
}
