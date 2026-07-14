//go:build !windows

package tools

import "os"

func atomicReplaceFile(from, to string) error {
	return os.Rename(from, to)
}

func syncParentDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
