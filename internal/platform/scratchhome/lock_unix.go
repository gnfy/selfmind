//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package scratchhome

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// fileOwner reports the uid that owns a file.
func fileOwner(info fs.FileInfo) (uint32, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return stat.Uid, true
}

// lockStable takes the exclusive lock at path without waiting; busy reports
// that another run holds it. The file is opened without following a symbolic
// link and used only when it is a regular file owned by me.
//
// unlock removes the file while still holding the lock, so no lock file is
// left behind. That is safe because a run counts its lock only once the file
// it locked is still the one at path: a run that opened the file just before
// its holder removed it finds a different file there, or none, and backs off.
func lockStable(path string, me uint32, ownerOf func(fs.FileInfo) (uint32, bool)) (unlock func(), busy bool, err error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("open lock file: %w", err)
	}
	locked, err := file.Stat()
	if err == nil {
		err = notOwned(path, locked, me, ownerOf, false)
	}
	if err != nil {
		_ = file.Close()
		return nil, false, err
	}
	fd := int(file.Fd())
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("lock %s: %w", path, err)
	}
	if current, err := os.Lstat(path); err != nil || !os.SameFile(locked, current) {
		_ = syscall.Flock(fd, syscall.LOCK_UN)
		_ = file.Close()
		return nil, true, nil
	}
	return func() {
		_ = os.Remove(path)
		_ = syscall.Flock(fd, syscall.LOCK_UN)
		_ = file.Close()
	}, false, nil
}
