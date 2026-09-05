package control

import (
	"testing"

	"selfmind/internal/platform/dbtemplate"
)

// In-package tests cannot import internal/control/controltest without an import
// cycle, so they get the same helper here. Both share the copy mechanics — and
// crucially the definition of which files make up a database — through
// internal/platform/dbtemplate, so the two cannot drift apart.
var storeTemplate = &dbtemplate.Template{
	Name: "control.db",
	Build: func(dir string) error {
		store, err := OpenStore(dir)
		if err != nil {
			return err
		}
		return store.Close()
	},
}

// seedStoreDir writes a fully migrated control database into dir.
func seedStoreDir(t testing.TB, dir string) {
	t.Helper()
	if err := storeTemplate.Seed(dir); err != nil {
		t.Fatalf("seed control store: %v", err)
	}
}

// newTestStore is the default way for an in-package test to get a store: its
// own isolated database, without recomputing the schema. A test that is ABOUT
// database creation, migration, or upgrade must keep calling OpenStore on a
// bare directory instead — that path is what those tests exist to exercise.
func newTestStore(t testing.TB) *Store {
	t.Helper()
	dir := t.TempDir()
	seedStoreDir(t, dir)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("open control store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
