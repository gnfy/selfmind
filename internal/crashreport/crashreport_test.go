package crashreport

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConsumeNoticeReturnsNewestCrashOnce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := Dir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	older := filepath.Join(dir, "20260724T010000.000000000Z.log")
	newer := filepath.Join(dir, "20260724T020000.000000000Z.log")
	if err := os.WriteFile(older, []byte("older"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newer, []byte("newer"), 0600); err != nil {
		t.Fatal(err)
	}

	path, ok := ConsumeNotice()
	if !ok || path != newer {
		t.Fatalf("first notice = (%q, %v), want (%q, true)", path, ok, newer)
	}
	if path, ok := ConsumeNotice(); ok || path != "" {
		t.Fatalf("second notice = (%q, %v), want empty", path, ok)
	}

	latest := filepath.Join(dir, "20260724T030000.000000000Z.log")
	if err := os.WriteFile(latest, []byte("latest"), 0600); err != nil {
		t.Fatal(err)
	}
	path, ok = ConsumeNotice()
	if !ok || path != latest {
		t.Fatalf("new crash notice = (%q, %v), want (%q, true)", path, ok, latest)
	}
}
