package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/config"
)

// InitStorage loads config, creates the data directory, and wires up the
// SQLite storage provider and MemoryManager.
func InitStorage(cfg *config.Config) (*memory.MemoryManager, string, error) {
	dataDir := cfg.Storage.DataDir

	// Expand ~ to user home directory
	if strings.HasPrefix(dataDir, "~/") {
		home, _ := os.UserHomeDir()
		dataDir = filepath.Join(home, dataDir[2:])
	} else if dataDir == "~" {
		home, _ := os.UserHomeDir()
		dataDir = home
	}

	if dataDir == "" {
		home, _ := os.UserHomeDir()
		dataDir = filepath.Join(home, ".selfmind", "data")
	}

	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, "", fmt.Errorf("create data dir: %w", err)
	}

	storage, err := memory.NewSQLiteProvider(dataDir)
	if err != nil {
		fmt.Printf("[WARN] Failed to init SQLite, using nil storage: %s\n", err)
		return nil, dataDir, nil
	}

	mem := memory.NewMemoryManager(storage)
	return mem, dataDir, nil
}
