package tools

import (
	"fmt"
	"os"
	"path/filepath"
)

// atomicWriteFile replaces path only after the complete new contents have
// reached a same-directory temporary file. Keeping the temporary file beside
// the target makes the final replace atomic on supported local filesystems.
func atomicWriteFile(path, content string) error {
	return atomicWriteBytes(path, []byte(content), 0644)
}

func atomicWriteBytes(path string, data []byte, defaultMode os.FileMode) error {
	if path == "" {
		return fmt.Errorf("path is required")
	}
	dir := filepath.Dir(path)
	mode := defaultMode
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	} else if !os.IsNotExist(err) {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".selfmind-write-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}

	if err := tmp.Chmod(mode); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := atomicReplaceFile(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return syncParentDirectory(dir)
}
