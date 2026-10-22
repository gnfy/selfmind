package eval

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCanaryFixturesAreStableIsolatedAndModelNeutral(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "canary")
	paths, err := InitCanary(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 5 {
		t.Fatalf("paths=%v", paths)
	}
	for _, path := range paths {
		c, err := LoadCase(path)
		if err != nil {
			t.Fatal(err)
		}
		if c.Provider != "" || c.Model != "" || c.Workspace != "isolated" || c.Repeat != 3 || c.ApprovalMode != "smart" || c.Expect.MinToolCalls != 0 {
			t.Fatalf("fixture imposes route or calls: %+v", c)
		}
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0o600 {
			t.Fatal("canary not private")
		}
	}
	if _, err := InitCanary(dir); err == nil {
		t.Fatal("fixture overwrite allowed")
	}
	other, err := InitCanary(filepath.Join(t.TempDir(), "same"))
	if err != nil {
		t.Fatal(err)
	}
	for i, path := range paths {
		a, _ := os.ReadFile(path)
		b, _ := os.ReadFile(other[i])
		if string(a) != string(b) {
			t.Fatal("fixture generation changed between builds")
		}
	}
}

func TestCapturedDraftCannotReachRuntime(t *testing.T) {
	r, err := RunCase(context.Background(), &Case{ID: "draft", CaptureReviewRequired: true}, RunOptions{ConfigPath: "does-not-exist"})
	if r != nil || err == nil {
		t.Fatalf("unreviewed capture reached runtime: %+v %v", r, err)
	}
}
