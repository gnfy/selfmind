package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"selfmind/internal/platform/config"
)

// inputHistoryEntry is one line of ~/.selfmind/input_history.jsonl.
type inputHistoryEntry struct {
	TS      int64  `json:"ts"`
	Channel string `json:"channel,omitempty"`
	Text    string `json:"text"`
}

// inputHistoryStore persists composer input history across CLI sessions
// (codex-style ~/.codex/history.jsonl). All disk writes go through a single
// writer goroutine fed by a bounded queue, so the key-handling path never
// blocks on IO: when the queue is full the entry is dropped (best-effort —
// it survives in the in-memory session history).
//
// Concurrency: multiple selfmind processes may share the file. Every write
// (append + possible trim) and every read runs under an advisory flock on a
// sidecar .lock file. This is a file-integrity lock on a client-local file,
// same class as modelruntime's auth-file lock — not a business-level
// cross-process lock (AGENTS.md).
type inputHistoryStore struct {
	path     string
	channel  string
	maxBytes int64

	queue chan inputHistoryEntry
	done  chan struct{}

	mu     sync.Mutex // guards closed and the send-vs-close race on queue
	closed bool
}

const (
	// maxInputHistoryEntryBytes bounds one persisted entry. Inputs above it
	// (large pastes) are not recorded at all: a truncated command is worse
	// than a missing one, and re-submitting megabytes from history is never
	// what the user meant.
	maxInputHistoryEntryBytes = 4096
	inputHistoryFileName      = "input_history.jsonl"
	// inputHistoryQueueSize bounds the async write queue; overflow drops.
	inputHistoryQueueSize = 64
	// trimKeepRatio: when the file exceeds maxBytes, rewrite keeping the tail
	// down to this fraction (codex's soft-cap strategy), so trims are rare.
	trimKeepRatio = 0.8
)

// newInputHistoryStore returns a running store, or nil when persistence is
// disabled ("history.persistence: none") or no home directory is available.
// A nil *inputHistoryStore is safe to use: Append/Close are no-ops.
func newInputHistoryStore(cfg *config.Config, channel string) *inputHistoryStore {
	if cfg == nil || !cfg.History.PersistEnabled() {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return nil
	}
	maxBytes := cfg.History.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 524288
	}
	s := &inputHistoryStore{
		path:     filepath.Join(home, ".selfmind", inputHistoryFileName),
		channel:  channel,
		maxBytes: maxBytes,
		queue:    make(chan inputHistoryEntry, inputHistoryQueueSize),
		done:     make(chan struct{}),
	}
	go s.writeLoop()
	return s
}

// newInputHistoryState builds the persistence store and preloads the prior
// sessions' entries that seed the in-memory history (persistent prefix +
// in-session suffix, same merge model as codex's composer history). Both
// values are usable when persistence is off: nil store, empty history.
func newInputHistoryState(cfg *config.Config, channel string) (*inputHistoryStore, []string) {
	store := newInputHistoryStore(cfg, channel)
	if store == nil {
		return nil, []string{}
	}
	loadEntries := 200
	if cfg != nil && cfg.History.LoadEntries > 0 {
		loadEntries = cfg.History.LoadEntries
	}
	persisted := store.Load(loadEntries)
	if persisted == nil {
		persisted = []string{}
	}
	return store, persisted
}

// Load returns up to maxEntries most recent persisted inputs, oldest first,
// with adjacent duplicates folded. Best-effort: any error reads as empty.
func (s *inputHistoryStore) Load(maxEntries int) []string {
	if s == nil || maxEntries <= 0 {
		return nil
	}
	unlock := lockHistoryFile(s.path, false)
	data, err := os.ReadFile(s.path)
	unlock()
	if err != nil {
		return nil
	}
	return parseHistoryLines(data, maxEntries)
}

// Append queues one input for persistence. Non-blocking: a full queue drops
// the entry. Oversized and empty entries are skipped.
func (s *inputHistoryStore) Append(text string) {
	if s == nil {
		return
	}
	text = strings.TrimSpace(text)
	if text == "" || len(text) > maxInputHistoryEntryBytes {
		return
	}
	entry := inputHistoryEntry{TS: time.Now().Unix(), Channel: s.channel, Text: text}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.queue <- entry:
	default: // queue full: drop, never block the key-handling path
	}
}

// Close flushes queued entries and stops the writer. Safe to call more than
// once and on nil.
func (s *inputHistoryStore) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.queue)
	}
	s.mu.Unlock()
	<-s.done
}

func (s *inputHistoryStore) writeLoop() {
	defer close(s.done)
	for entry := range s.queue {
		s.persist(entry)
	}
}

// persist appends one JSONL line under an exclusive advisory lock, then trims
// the file in place if it grew past maxBytes. Errors are swallowed: history
// persistence must never surface as a CLI failure.
func (s *inputHistoryStore) persist(entry inputHistoryEntry) {
	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return
	}
	unlock := lockHistoryFile(s.path, true)
	defer unlock()

	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, werr := f.Write(append(line, '\n'))
	info, serr := f.Stat()
	f.Close()
	if werr != nil || serr != nil || info.Size() <= s.maxBytes {
		return
	}
	s.trimLocked()
}

// trimLocked rewrites the file keeping the newest entries whose total size
// fits trimKeepRatio*maxBytes. Caller must hold the exclusive lock.
func (s *inputHistoryStore) trimLocked() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	budget := int64(float64(s.maxBytes) * trimKeepRatio)
	lines := bytes.Split(data, []byte("\n"))
	var kept [][]byte
	var size int64
	for i := len(lines) - 1; i >= 0; i-- {
		ln := bytes.TrimSpace(lines[i])
		if len(ln) == 0 {
			continue
		}
		size += int64(len(ln)) + 1
		if size > budget {
			break
		}
		kept = append(kept, ln)
	}
	var out bytes.Buffer
	for i := len(kept) - 1; i >= 0; i-- {
		out.Write(kept[i])
		out.WriteByte('\n')
	}
	// In-place rewrite (not tmp+rename): the advisory lock is on a sidecar
	// file, so replacing the inode here is safe, but in-place keeps the
	// happy path a single open.
	_ = os.WriteFile(s.path, out.Bytes(), 0o600)
}

// parseHistoryLines extracts the last maxEntries valid texts, oldest first,
// folding adjacent duplicates (matches recordInputHistory's dedup).
func parseHistoryLines(data []byte, maxEntries int) []string {
	var texts []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), maxInputHistoryEntryBytes*4)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var entry inputHistoryEntry
		if json.Unmarshal(line, &entry) != nil {
			continue // skip corrupt lines (partial writes, manual edits)
		}
		text := strings.TrimSpace(entry.Text)
		if text == "" || len(text) > maxInputHistoryEntryBytes {
			continue
		}
		if len(texts) > 0 && texts[len(texts)-1] == text {
			continue
		}
		texts = append(texts, text)
	}
	if len(texts) > maxEntries {
		texts = texts[len(texts)-maxEntries:]
	}
	return texts
}
