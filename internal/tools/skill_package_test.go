package tools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSkillPackageHashCoversMainAndLinkedResources(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	createTestSkill(t, "default", "package-flow", "## Procedure\nDo it")
	skillDir := filepath.Join(home, ".selfmind", "default", "skills", "package-flow")
	resource := filepath.Join(skillDir, "references", "details.md")
	if err := os.MkdirAll(filepath.Dir(resource), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteFile(resource, "first"); err != nil {
		t.Fatal(err)
	}
	before, err := ReadSkillPackageForTenant("default", "package-flow")
	if err != nil {
		t.Fatal(err)
	}
	if len(before.ResourceManifest) != 1 || before.ResourceManifest[0].Path != "references/details.md" {
		t.Fatalf("manifest = %+v", before.ResourceManifest)
	}
	if err := atomicWriteFile(resource, "second"); err != nil {
		t.Fatal(err)
	}
	after, err := ReadSkillPackageForTenant("default", "package-flow")
	if err != nil {
		t.Fatal(err)
	}
	if before.VersionHash != after.VersionHash || before.PackageHash == after.PackageHash {
		t.Fatalf("main/package hashes before=%s/%s after=%s/%s", before.VersionHash, before.PackageHash, after.VersionHash, after.PackageHash)
	}
}
