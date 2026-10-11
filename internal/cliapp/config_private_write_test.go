package cliapp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// `selfmind config` rewrites the configuration and leaves a timestamped copy of
// it. Both go next to the file, in a directory a confined command may be able to
// write, so neither may go through a name the command could prepare.

func TestConfigRewriteNeverWritesThroughAPlantedTempName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	attacker := filepath.Join(dir, "attacker-owned")
	if err := os.WriteFile(attacker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(attacker, path+".tmp"); err != nil {
		t.Fatal(err)
	}
	if err := writeConfigBytesAtomic(path, []byte("delegation:\n  api_key: sk-secret\n")); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(attacker); len(got) != 0 {
		t.Fatalf("the configuration was written through a planted symlink: %q", got)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("the rewritten configuration must be a private regular file: %v %v", info, err)
	}
}

func TestConfigBackupNeverWritesThroughAPlantedName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	attacker := filepath.Join(dir, "attacker-owned")
	if err := os.WriteFile(attacker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// The backup name is the path and the second: predictable. Plant every name it
	// could take around now.
	now := time.Now()
	for offset := -2; offset <= 3; offset++ {
		name := path + ".bak-" + now.Add(time.Duration(offset)*time.Second).Format("20060102-150405")
		if err := os.Symlink(attacker, name); err != nil {
			t.Fatal(err)
		}
	}
	backup, err := backupConfigFile(path, []byte("delegation:\n  api_key: sk-secret\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(attacker); len(got) != 0 {
		t.Fatalf("the backup was written through a planted symlink: %q", got)
	}
	info, err := os.Lstat(backup)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("the backup must be a new private regular file: %v %v", info, err)
	}
	if !strings.HasPrefix(backup, path+".bak-") {
		t.Fatalf("a backup stays under the private config prefix, got %s", backup)
	}
	// Two backups in the same second do not collide or overwrite.
	second, err := backupConfigFile(path, []byte("second"))
	if err != nil || second == backup {
		t.Fatalf("a second backup must take its own name: %q %q %v", backup, second, err)
	}
	if got, _ := os.ReadFile(backup); !strings.Contains(string(got), "sk-secret") {
		t.Fatalf("the first backup was overwritten: %q", got)
	}
}
