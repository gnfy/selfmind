package buildinfo

import "testing"

func TestFingerprintUsesReleaseIdentity(t *testing.T) {
	oldCommit, oldBuiltAt := Commit, BuiltAt
	t.Cleanup(func() {
		Commit, BuiltAt = oldCommit, oldBuiltAt
	})

	Commit = "0123456789abcdef"
	BuiltAt = "2026-07-20T01:02:03Z"

	if got, want := Fingerprint(), "v0.1.0+0123456789ab@2026-07-20T01:02:03Z"; got != want {
		t.Fatalf("Fingerprint() = %q, want %q", got, want)
	}
	info := Current()
	if info.Commit != "0123456789ab" || info.BuiltAt != BuiltAt || info.Fingerprint == "" {
		t.Fatalf("Current() = %+v", info)
	}
}

func TestFingerprintKeepsLocalBuildReadable(t *testing.T) {
	oldCommit, oldBuiltAt := Commit, BuiltAt
	t.Cleanup(func() {
		Commit, BuiltAt = oldCommit, oldBuiltAt
	})

	Commit = ""
	BuiltAt = ""
	if got := Fingerprint(); got != Version {
		t.Fatalf("Fingerprint() = %q, want %q", got, Version)
	}
}
