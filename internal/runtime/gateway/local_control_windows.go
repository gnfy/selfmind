//go:build windows

package gateway

import "errors"

// Native Windows is unsupported; WSL runs the Unix implementation.
var errLocalControlUnsupported = errors.New("the local control socket requires Linux or macOS (use WSL on Windows)")

func preparePrivateSocketDir(string) error { return errLocalControlUnsupported }

func checkPrivateSocketDir(string) error { return errLocalControlUnsupported }

func inspectSocketDir(string) (LocalControlDirState, string) {
	return LocalControlDirUnsafe, errLocalControlUnsupported.Error()
}
