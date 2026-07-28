//go:build !linux

package gateway

func processRestartEnvironment(int) []string {
	return nil
}
