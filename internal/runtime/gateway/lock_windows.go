//go:build windows

package gateway

import (
	"os"

	"golang.org/x/sys/windows"
)

type runtimeFileLock struct {
	file       *os.File
	overlapped windows.Overlapped
}

func acquireRuntimeLock(path string) (*runtimeFileLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	lock := &runtimeFileLock{file: file}
	err = windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		&lock.overlapped,
	)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return lock, nil
}

func (l *runtimeFileLock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = windows.UnlockFileEx(windows.Handle(l.file.Fd()), 0, 1, 0, &l.overlapped)
	return l.file.Close()
}
