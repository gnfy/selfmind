package gateway

import (
	"selfmind/internal/app"
	"selfmind/internal/platform/config"
)

func loadConfigOrDefault(path string) (*config.Config, error) {
	cfg, err := config.LoadConfig(config.Options{Path: path})
	if err != nil {
		return &config.Config{}, nil
	}
	return cfg, nil
}

func LoadConfigForCLI(path string) (*config.Config, error) {
	return loadConfigOrDefault(path)
}

func resolveDataDir(cfg *config.Config) string {
	return app.ResolveDataDir(cfg)
}
