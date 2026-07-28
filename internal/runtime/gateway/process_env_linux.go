//go:build linux

package gateway

import (
	"fmt"
	"os"
)

func processRestartEnvironment(pid int) []string {
	if pid <= 0 {
		return nil
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return nil
	}
	return restartEnvironmentFromBlock(data)
}
