package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"selfmind/internal/platform/config"
	"selfmind/internal/tools"
)

// configuredSkillStorage resolves the one filesystem root shared by app
// wiring, tools, the curator, and post-run learning audit. Eval overrides the
// existing Evolution.SkillsDir compatibility field with a throwaway base.
func ResolveSkillStorage(cfg *config.Config) (*tools.SkillStorage, error) {
	if cfg == nil {
		return nil, fmt.Errorf("configuration is required to resolve skill storage")
	}
	baseDir := strings.TrimSpace(cfg.Evolution.SkillsDir)
	if baseDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		baseDir = filepath.Join(home, ".selfmind")
	}
	return tools.NewSkillStorage(baseDir)
}

func configuredSkillStorage(cfg *config.Config) (*tools.SkillStorage, error) {
	return ResolveSkillStorage(cfg)
}
