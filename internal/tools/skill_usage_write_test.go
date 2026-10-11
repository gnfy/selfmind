package tools

import (
	"os"
	"path/filepath"
	"testing"
)

// The skill usage sidecar records which skills the agent leans on. It is written
// in the skills directory, which a confined command with a home-wide root can
// also write, so it must not be written through a name the command planted.
func TestSkillUsageSidecarNeverWritesThroughAPlantedTempName(t *testing.T) {
	dir := t.TempDir()
	attacker := filepath.Join(dir, "attacker-owned")
	if err := os.WriteFile(attacker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sidecar := usageFilePath(dir)
	if err := os.Symlink(attacker, sidecar+".tmp"); err != nil {
		t.Fatal(err)
	}
	if err := saveSkillUsageForDir(dir, map[string]SkillUsageRecord{"demo": {}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(attacker); len(got) != 0 {
		t.Fatalf("the sidecar was written through a planted symlink: %q", got)
	}
	if info, err := os.Lstat(sidecar); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("the sidecar must be a regular file: %v %v", info, err)
	}
	if records, err := loadSkillUsageForDir(dir); err != nil || len(records) != 1 {
		t.Fatalf("the sidecar did not round-trip: %v %v", records, err)
	}
}
