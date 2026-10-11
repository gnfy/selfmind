package kernel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An archived skill becomes instructions the agent follows. It is written beside
// the person's other skills, in a directory a confined command with a home-wide
// root can write, so it must not be written through a name the command planted.
func TestArchiveSkillNeverWritesThroughAPlantedTempName(t *testing.T) {
	dir := t.TempDir()
	attacker := filepath.Join(dir, "attacker-owned")
	if err := os.WriteFile(attacker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "release-notes.md")
	if err := os.Symlink(attacker, target+".tmp"); err != nil {
		t.Fatal(err)
	}
	engine := NewReflectionEngine(nil, EvolutionConfig{SkillsDir: dir})
	content := "# release notes\n\nSummarise the merged changes by area.\n"
	if err := engine.ArchiveSkill(context.Background(), &ReviewResult{Action: "create", SkillName: "release-notes", Content: content}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(attacker); len(got) != 0 {
		t.Fatalf("the skill was written through a planted symlink: %q", got)
	}
	info, err := os.Lstat(target)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("the skill must be a regular file: %v %v", info, err)
	}
	if got, _ := os.ReadFile(target); !strings.Contains(string(got), "Summarise") {
		t.Fatalf("skill = %q", got)
	}
}
