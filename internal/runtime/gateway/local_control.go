package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// EnsureLocalControlToken creates the local privileged-control credential once.
// It is separate from the public gateway bearer token and never enters a tool
// child environment.
func EnsureLocalControlToken(dataDir string) (string, error) {
	path := ResolvePaths(dataDir).LocalControlTokenPath
	if token, err := ReadLocalControlToken(dataDir); err == nil {
		return token, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(ResolvePaths(dataDir).RuntimeDir, 0700); err != nil {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate local control token: %w", err)
	}
	token := hex.EncodeToString(raw)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return ReadLocalControlToken(dataDir)
	}
	if err != nil {
		return "", err
	}
	if _, err = file.WriteString(token + "\n"); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	return token, nil
}

func ReadLocalControlToken(dataDir string) (string, error) {
	data, err := os.ReadFile(ResolvePaths(dataDir).LocalControlTokenPath)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("local control token is empty")
	}
	return token, nil
}
