//go:build linux

package gateway

import (
	"os"
	"strings"
)

func hostBootID() string {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
