package app

import (
	"os"
	"path/filepath"
	"strings"
)

// CheckHermesSkills checks if Hermes skills exist.
func CheckHermesSkills() (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	hermesSkillsDir := filepath.Join(home, ".hermes", "skills")
	if _, err := os.Stat(hermesSkillsDir); err == nil {
		return hermesSkillsDir, true
	}
	return "", false
}

// NeedsMigration checks if the user has Hermes skills but SelfMind skills are empty.
func NeedsMigration() bool {
	_, hermesExists := CheckHermesSkills()
	if !hermesExists {
		return false
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	selfmindSkillsDir := filepath.Join(home, ".selfmind", "skills")
	
	entries, err := os.ReadDir(selfmindSkillsDir)
	// If the directory doesn't exist or is empty, we need migration
	if err != nil || len(entries) == 0 {
		return true
	}
	return false
}

// MigrateHermesSkills copies skills from Hermes to SelfMind.
func MigrateHermesSkills(hermesDir string) (int, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return 0, err
	}
	selfmindSkillsDir := filepath.Join(home, ".selfmind", "skills")
	if err := os.MkdirAll(selfmindSkillsDir, 0755); err != nil {
		return 0, err
	}

	count := 0
	err = filepath.Walk(hermesDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".md") {
			return nil
		}

		// Read and adapt
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		// Basic adaptation: replace tool names if necessary
		// Hermes use snake_case mostly, SelfMind too.
		// One common change might be read_file -> read_file (already matches).
		// We can add more complex logic here later.
		
		destPath := filepath.Join(selfmindSkillsDir, info.Name())
		if _, err := os.Stat(destPath); err == nil {
			// Skip if exists
			return nil
		}

		if err := os.WriteFile(destPath, content, 0644); err != nil {
			return err
		}
		count++
		return nil
	})

	return count, err
}
