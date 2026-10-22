//go:build !windows

package gateway

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// preparePrivateSocketDir creates the socket directory or repairs its mode.
// A directory owned by another user is refused: under /tmp anyone may create
// the name first.
func preparePrivateSocketDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	info, err := ownedDirectory(dir)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("restrict local control socket directory %s: %w", dir, err)
		}
	}
	return nil
}

// checkPrivateSocketDir is the client's check before trusting a socket.
func checkPrivateSocketDir(dir string) error {
	info, err := ownedDirectory(dir)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("local control socket directory %s is accessible to other users (mode %04o); restart the daemon to repair it", dir, info.Mode().Perm())
	}
	return nil
}

// inspectSocketDir classifies the directory for diagnostics: absent (nothing
// has opened a socket here), safe, or one the client refuses to trust.
func inspectSocketDir(dir string) (LocalControlDirState, string) {
	err := checkPrivateSocketDir(dir)
	switch {
	case err == nil:
		return LocalControlDirPrivate, "private (0700, owned by you)"
	case errors.Is(err, os.ErrNotExist):
		return LocalControlDirMissing, "missing"
	default:
		return LocalControlDirUnsafe, err.Error()
	}
}

func ownedDirectory(dir string) (os.FileInfo, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("local control socket directory %s is not a directory", dir)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("cannot read the owner of %s", dir)
	}
	if int(stat.Uid) != os.Getuid() {
		return nil, fmt.Errorf("local control socket directory %s belongs to uid %d, not this user", dir, stat.Uid)
	}
	return info, nil
}
