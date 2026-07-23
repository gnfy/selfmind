package tools

import (
	"context"
	"os/exec"
	"runtime"
)

func shellArgv(command string) []string {
	if runtime.GOOS == "windows" {
		return []string{"powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", command}
	}
	return []string{"/bin/bash", "-c", command}
}

func shellCommandContext(ctx context.Context, command string) *exec.Cmd {
	argv := shellArgv(command)
	return exec.CommandContext(ctx, argv[0], argv[1:]...)
}

func shellCommand(command string) *exec.Cmd {
	argv := shellArgv(command)
	return exec.Command(argv[0], argv[1:]...)
}
