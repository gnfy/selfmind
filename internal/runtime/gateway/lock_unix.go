//go:build !windows

package gateway

import (
	"os"

	"golang.org/x/sys/unix"
)

type runtimeFileLock struct {
	file *os.File
}

func acquireRuntimeLock(path string) (*runtimeFileLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &runtimeFileLock{file: file}, nil
}

func (l *runtimeFileLock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	return l.file.Close()
}
