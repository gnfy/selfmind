//go:build windows

package tools

import "golang.org/x/sys/windows"

func atomicReplaceFile(from, to string) error {
	fromPtr, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	toPtr, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(fromPtr, toPtr, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

// Windows does not expose a portable directory fsync equivalent. MoveFileEx
// with WRITE_THROUGH supplies the durability guarantee for the replacement.
func syncParentDirectory(string) error { return nil }
