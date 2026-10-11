// Package atomicfile replaces a file that holds a secret without ever writing
// through a name someone else could have prepared.
//
// The usual write-then-rename pattern names its temporary file after the
// destination (`auth.json.tmp`). Every tool a person runs shares their user, so a
// command that can write the directory can plant a symlink or an open file at
// that predictable name before the daemon writes: the secret then lands in the
// command's file, and the rename makes the destination point at it. Write
// creates its temporary file exclusively under a random name instead, so there is
// nothing to prepare.
package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// Write replaces path with data, leaving path with mode perm. The data is
// written and synced to a fresh file in the same directory, created with
// O_EXCL under a name no one can predict, and renamed over path in one step:
// readers see the old content or the new, never a partial file, and an existing
// path, including a symlink, is replaced rather than written through.
//
// The temporary file's name begins with path and a dot (see TempName), so a
// private-name prefix that covers path covers it too.
func Write(path string, data []byte, perm os.FileMode) error {
	if path == "" {
		return fmt.Errorf("atomicfile: path is required")
	}
	temp, err := newTemp(path)
	if err != nil {
		return err
	}
	name := temp.Name()
	fail := func(err error) error {
		_ = temp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := temp.Chmod(perm); err != nil {
		return fail(err)
	}
	if _, err := temp.Write(data); err != nil {
		return fail(err)
	}
	if err := temp.Sync(); err != nil {
		return fail(err)
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// TempName is the prefix every temporary file Write makes for path begins with.
func TempName(path string) string { return path + "." }

func newTemp(path string) (*os.File, error) {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	// CreateTemp opens with O_EXCL and mode 0600.
	return os.CreateTemp(dir, base+".*.tmp")
}
