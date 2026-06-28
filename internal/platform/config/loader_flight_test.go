package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFlightRecorderConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("flight_recorder:\n  enabled: true\n  keep: 7\n  dir: /tmp/fl\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(Options{Path: path})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.FlightRecorder.Enabled || cfg.FlightRecorder.Keep != 7 || cfg.FlightRecorder.Dir != "/tmp/fl" {
		t.Fatalf("flight_recorder parsed wrong: %+v", cfg.FlightRecorder)
	}
}
