// Package dbtemplate seeds many isolated SQLite databases from one built copy.
//
// Creating a database is expensive when creation means running an ordered
// migration chain, and under the race detector — where pure-Go SQLite is
// instrumented on every memory access — it dominates everything else. Paying it
// once per install is right; paying it once per test multiplies the number of
// tests by the number of schema versions, so each new migration slows down
// every existing test. That product is what pushed two CI jobs past a
// twelve-minute budget on 2026-09-04.
//
// This package holds the mechanics only: build once, then copy the files. It
// knows nothing about any particular schema, and deliberately does not import
// `testing`, so callers own how a failure is reported. The single definition of
// WHICH files make up a database lives here, because a copy that silently omits
// one is the failure mode that would make seeded databases differ from real
// ones.
package dbtemplate

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Files is one built database: file name to contents.
type Files map[string][]byte

// Template builds a database at most once and hands out its bytes.
type Template struct {
	// Name is the main database file, e.g. "control.db".
	Name string
	// Build creates the database inside the directory it is given. It must
	// close everything it opened before returning, or a write-ahead log may
	// still hold objects the copy would miss.
	Build func(dir string) error

	once  sync.Once
	files Files
	err   error
}

// sidecarSuffixes are SQLite's companion files. A clean close checkpoints the
// write-ahead log into the main file, but copying whatever exists keeps this
// correct even if that stops being true.
var sidecarSuffixes = []string{"", "-wal", "-shm"}

// Files builds on first use and returns the same bytes thereafter.
func (t *Template) Files() (Files, error) {
	t.once.Do(func() {
		if t.Name == "" || t.Build == nil {
			t.err = fmt.Errorf("dbtemplate: Name and Build are required")
			return
		}
		dir, err := os.MkdirTemp("", "dbtemplate-")
		if err != nil {
			t.err = err
			return
		}
		defer os.RemoveAll(dir)

		if err := t.Build(dir); err != nil {
			t.err = fmt.Errorf("dbtemplate: build %s: %w", t.Name, err)
			return
		}
		files := Files{}
		for _, suffix := range sidecarSuffixes {
			name := t.Name + suffix
			data, readErr := os.ReadFile(filepath.Join(dir, name))
			if readErr != nil {
				if os.IsNotExist(readErr) {
					continue
				}
				t.err = readErr
				return
			}
			files[name] = data
		}
		if len(files[t.Name]) == 0 {
			t.err = fmt.Errorf("dbtemplate: build produced no %s", t.Name)
			return
		}
		t.files = files
	})
	return t.files, t.err
}

// Seed writes a built copy into dir, creating it if needed. Each call produces
// an independent database: isolation is preserved, only the building is shared.
func (t *Template) Seed(dir string) error {
	files, err := t.Files()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("dbtemplate: create %s: %w", dir, err)
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			return fmt.Errorf("dbtemplate: seed %s: %w", name, err)
		}
	}
	return nil
}
