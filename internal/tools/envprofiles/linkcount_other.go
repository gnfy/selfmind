//go:build !unix

package envprofiles

import "io/fs"

// linkCount is unknown here; in-place state writes exist only on unix hosts.
func linkCount(fs.FileInfo) uint64 { return 1 }
