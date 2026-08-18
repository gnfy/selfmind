package control

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenExistingStoreReadOnlyNeverCreatesDatabase(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "missing-data")
	if _, err := OpenExistingStoreReadOnly(dataDir); err == nil {
		t.Fatal("missing control.db was accepted")
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("read-only open created data dir: %v", err)
	}
}

func TestOpenExistingStoreReadOnlyListsReleasedPersons(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := OpenStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := store.ResolveOrCreateAccount(ctx, DefaultTenantID, "cli", "readonly-test", "Read only")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	readonly, err := OpenExistingStoreReadOnly(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer readonly.Close()
	persons, err := readonly.ListPersonIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(persons) != 1 || persons[0] != identity.PersonID {
		t.Fatalf("persons=%v want=%s", persons, identity.PersonID)
	}
}

func TestOpenExistingStoreReadOnlyEscapesPathCharacters(t *testing.T) {
	root := t.TempDir()
	normalDir := filepath.Join(root, "normal-data")
	dataDir := filepath.Join(root, "control?#data")
	store, err := OpenStore(normalDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(normalDir, dataDir); err != nil {
		t.Fatal(err)
	}
	readonly, err := OpenExistingStoreReadOnly(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer readonly.Close()
	if _, err := readonly.ListPersonIDs(context.Background()); err != nil {
		t.Fatal(err)
	}
}
