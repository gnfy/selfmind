package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/platform/config"
)

func testHistoryConfig(t *testing.T, maxBytes int64) *config.Config {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	return &config.Config{History: config.HistoryConfig{
		Persistence: "save-all",
		MaxBytes:    maxBytes,
		LoadEntries: 200,
	}}
}

func TestInputHistoryStoreRoundTrip(t *testing.T) {
	cfg := testHistoryConfig(t, 524288)

	store := newInputHistoryStore(cfg, "chan-a")
	if store == nil {
		t.Fatal("expected a store when persistence is enabled")
	}
	store.Append("first input")
	store.Append("second input")
	store.Append("second input") // duplicate folds on load
	store.Close()

	reloaded := newInputHistoryStore(cfg, "chan-b")
	defer reloaded.Close()
	got := reloaded.Load(200)
	want := []string{"first input", "second input"}
	if len(got) != len(want) {
		t.Fatalf("loaded %d entries %v, want %v", len(got), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestInputHistoryStoreSkipsOversizedAndEmpty(t *testing.T) {
	cfg := testHistoryConfig(t, 524288)

	store := newInputHistoryStore(cfg, "chan")
	store.Append("")
	store.Append("   ")
	store.Append(strings.Repeat("x", maxInputHistoryEntryBytes+1))
	store.Append("kept")
	store.Close()

	got := newInputHistoryStore(cfg, "chan").Load(200)
	if len(got) != 1 || got[0] != "kept" {
		t.Fatalf("loaded %v, want [kept]", got)
	}
}

func TestInputHistoryStoreTrimsOversizedFile(t *testing.T) {
	cfg := testHistoryConfig(t, 2048)

	// Stay under inputHistoryQueueSize so no append is dropped by the
	// best-effort queue; 30 * ~160B still forces a trim past max_bytes 2048.
	store := newInputHistoryStore(cfg, "chan")
	for i := 0; i < 30; i++ {
		store.Append(strings.Repeat("a", 100) + string(rune('0'+i%10)))
	}
	store.Close()

	info, err := os.Stat(store.path)
	if err != nil {
		t.Fatalf("stat history file: %v", err)
	}
	if info.Size() > 2048 {
		t.Fatalf("file size %d exceeds max_bytes 2048 after trim", info.Size())
	}
	got := newInputHistoryStore(cfg, "chan").Load(200)
	if len(got) == 0 {
		t.Fatal("trim must retain the newest entries, got none")
	}
	last := got[len(got)-1]
	if !strings.HasSuffix(last, "9") {
		t.Fatalf("newest entry lost by trim: last = %q", last)
	}
}

func TestInputHistoryStoreSkipsCorruptLines(t *testing.T) {
	cfg := testHistoryConfig(t, 524288)

	store := newInputHistoryStore(cfg, "chan")
	store.Append("good entry")
	store.Close()

	f, err := os.OpenFile(store.path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open history file: %v", err)
	}
	if _, err := f.WriteString("{not json\n"); err != nil {
		t.Fatalf("write corrupt line: %v", err)
	}
	f.Close()

	store2 := newInputHistoryStore(cfg, "chan")
	store2.Append("after corruption")
	store2.Close()

	got := newInputHistoryStore(cfg, "chan").Load(200)
	want := []string{"good entry", "after corruption"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("loaded %v, want %v", got, want)
	}
}

func TestInputHistoryStoreDisabled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &config.Config{History: config.HistoryConfig{Persistence: "none"}}

	store, persisted := newInputHistoryState(cfg, "chan")
	if store != nil {
		t.Fatal("persistence none must yield a nil store")
	}
	if len(persisted) != 0 {
		t.Fatalf("persisted = %v, want empty", persisted)
	}
	// nil store is safe to use.
	store.Append("ignored")
	store.Close()
	if got := store.Load(10); got != nil {
		t.Fatalf("nil store Load = %v, want nil", got)
	}

	home, _ := os.UserHomeDir()
	if _, err := os.Stat(filepath.Join(home, ".selfmind", inputHistoryFileName)); !os.IsNotExist(err) {
		t.Fatal("disabled persistence must not create the history file")
	}
}

func TestInputHistoryStateSeedsMemoryHistory(t *testing.T) {
	cfg := testHistoryConfig(t, 524288)

	store := newInputHistoryStore(cfg, "chan")
	store.Append("from last session")
	store.Close()

	store2, persisted := newInputHistoryState(cfg, "chan")
	defer store2.Close()
	if len(persisted) != 1 || persisted[0] != "from last session" {
		t.Fatalf("persisted = %v, want [from last session]", persisted)
	}
}

func TestInputHistoryStoreLoadRespectsLimit(t *testing.T) {
	cfg := testHistoryConfig(t, 524288)

	store := newInputHistoryStore(cfg, "chan")
	store.Append("one")
	store.Append("two")
	store.Append("three")
	store.Close()

	got := newInputHistoryStore(cfg, "chan").Load(2)
	want := []string{"two", "three"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("loaded %v, want %v", got, want)
	}
}

func TestRecordInputHistorySkipsSecureInput(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.editor.SetSecure(true)
	model.recordInputHistory("hunter2")
	if len(model.inputHistory) != 0 {
		t.Fatalf("secure input recorded: %v", model.inputHistory)
	}
	model.editor.SetSecure(false)
	model.recordInputHistory("normal input")
	if len(model.inputHistory) != 1 {
		t.Fatalf("normal input not recorded: %v", model.inputHistory)
	}
}

func TestRecordInputHistorySkipsOversizedInput(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.recordInputHistory(strings.Repeat("x", maxInputHistoryEntryBytes+1))
	if len(model.inputHistory) != 0 {
		t.Fatalf("oversized input recorded (%d entries)", len(model.inputHistory))
	}
}

func TestInputHistoryFileEntriesCarryMetadata(t *testing.T) {
	cfg := testHistoryConfig(t, 524288)

	store := newInputHistoryStore(cfg, "session-xyz")
	store.Append("hello")
	store.Close()

	data, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatalf("read history file: %v", err)
	}
	var entry inputHistoryEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &entry); err != nil {
		t.Fatalf("parse entry: %v", err)
	}
	if entry.Text != "hello" || entry.Channel != "session-xyz" || entry.TS <= 0 {
		t.Fatalf("entry = %+v, want text/channel/ts populated", entry)
	}
}
