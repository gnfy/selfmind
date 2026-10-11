package runpool

import (
	"os"
	"path/filepath"
)

// physicalPathWithin supplements lexical ancestry with filesystem identity.
// Case aliases and bind mounts can name the same directory without being
// symlinks. Compare each target ancestor with the root's existing anchor;
// retain the unresolved suffix for paths that have not been created yet.
func physicalPathWithin(root, target string) bool {
	_, rootTail, rootInfo := existingPathAnchor(root)
	if rootInfo == nil {
		return false
	}
	parent, tail, info := existingPathAnchor(target)
	for info != nil {
		if os.SameFile(rootInfo, info) {
			return pathWithin(rootTail, tail)
		}
		next := filepath.Dir(parent)
		if next == parent {
			break
		}
		tail = filepath.Join(filepath.Base(parent), tail)
		parent = next
		info, _ = os.Stat(parent)
	}
	return false
}

func existingPathAnchor(path string) (string, string, os.FileInfo) {
	tail := "."
	for {
		if info, err := os.Stat(path); err == nil {
			return path, tail, info
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path, tail, nil
		}
		tail = filepath.Join(filepath.Base(path), tail)
		path = parent
	}
}
