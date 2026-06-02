//go:build !windows

package tools

import "syscall"

func processAlive(pid int) bool {
	_, err := syscall.Getpgid(pid)
	return err == nil
}
