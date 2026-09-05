// Package controltest builds isolated control stores for tests without paying
// the migration chain for each one.
//
// Every test needs its OWN database — sharing one makes tests see each other's
// rows and become order-dependent — but the schema they all arrive at is
// identical, and computing it is expensive: under -race one OpenStore costs
// ~4.6s, because a fresh database runs InitSchema plus the whole ordered
// migration chain and pure-Go SQLite is instrumented on every access.
//
// Production pays that once per install. Tests paid it hundreds of times per
// run, so the race jobs grew as (tests x schema versions) — a product, not a
// sum — and each new migration made every existing test slower.
//
// So: run the real creation path once per process, then seed each test from
// that copy. Isolation is unchanged; only the redundant recomputation is gone.
// The guard test asserts a seeded schema is indistinguishable from a freshly
// created one, because a suite that runs fast against a schema production never
// has would be worse than a slow one.
//
// In-package `package control` tests cannot import this (it would be an import
// cycle); they use the equivalent helper in the control package's own test
// files, which shares these mechanics through internal/platform/dbtemplate.
package controltest

import (
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/platform/dbtemplate"
)

var controlTemplate = &dbtemplate.Template{
	Name: "control.db",
	Build: func(dir string) error {
		store, err := control.OpenStore(dir)
		if err != nil {
			return err
		}
		return store.Close()
	},
}

// SeedDir writes a fully migrated control database into dir. Use it when the
// test needs the directory itself — a reopen, a backup, a second store over the
// same data.
func SeedDir(t testing.TB, dir string) {
	t.Helper()
	if err := controlTemplate.Seed(dir); err != nil {
		t.Fatalf("seed control store: %v", err)
	}
}

// NewStore returns an isolated store in the test's own temp directory, closed
// when the test ends.
func NewStore(t testing.TB) *control.Store {
	t.Helper()
	return NewStoreInDir(t, t.TempDir())
}

// NewStoreInDir is NewStore with the directory chosen by the caller.
func NewStoreInDir(t testing.TB, dir string) *control.Store {
	t.Helper()
	SeedDir(t, dir)
	store, err := control.OpenStore(dir)
	if err != nil {
		t.Fatalf("open control store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
