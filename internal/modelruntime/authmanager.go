package modelruntime

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// ExternalAuthManager is the process-global authority for external-CLI auth
// reuse (Codex ChatGPT OAuth, MiniMax OAuth, …). It exists so that, under a
// worker pool, concurrent token refreshes do not stampede: the per-closure lock
// in the old credential path only serialized within one resolved Credential, so
// N workers each holding their own closure could refresh the same rotating
// refresh_token in parallel and break the login. This manager keys auth state
// by source (absolute file path for OAuth, provider id for static keys) and
// collapses concurrent refreshes with single-flight.
//
// Design: docs/external-auth-manager.md.
type ExternalAuthManager struct {
	skew    time.Duration
	mu      sync.Mutex // guards entries map (lookup/create only)
	entries map[string]*authEntry
	sf      singleflight.Group
}

// AuthKind distinguishes refreshable OAuth-file sources from static API keys.
type AuthKind int

const (
	AuthStaticKey AuthKind = iota
	AuthOAuthFile
)

// AuthRef identifies one external auth source.
type AuthRef struct {
	Provider string
	Kind     AuthKind
	Path     string // absolute auth-file path for AuthOAuthFile; "" for static keys
}

func (r AuthRef) key() string {
	if r.Kind == AuthOAuthFile && r.Path != "" {
		if abs, err := filepath.Abs(r.Path); err == nil {
			return "file:" + abs
		}
		return "file:" + r.Path
	}
	return "static:" + r.Provider
}

// AuthSnapshot is the in-memory view of an auth source.
type AuthSnapshot struct {
	Token        string
	RefreshToken string
	ExpiresAt    time.Time // zero = unknown (do not force refresh on expiry alone)
	AccountID    string    // e.g. ChatGPT account_id; "" if none
	FileMtime    time.Time // mtime when loaded (OAuth files); enables consistent reload
}

// AuthError separates transient failures (retryable) from permanent ones
// (quarantine + actionable message). It implements error.
type AuthError struct {
	Permanent  bool
	Reason     string
	Actionable string
	Cause      error
}

func (e *AuthError) Error() string {
	switch {
	case e == nil:
		return ""
	case e.Actionable != "":
		return e.Actionable
	case e.Cause != nil:
		return e.Cause.Error()
	default:
		return e.Reason
	}
}

// LoadFunc reads and parses the current auth state from the source.
type LoadFunc func(ref AuthRef) (AuthSnapshot, error)

// RefreshFunc rotates the token and atomically persists it (OAuth sources),
// returning the new snapshot. nil for static-key sources.
type RefreshFunc func(ref AuthRef, prev AuthSnapshot) (AuthSnapshot, *AuthError)

type authEntry struct {
	ref     AuthRef
	load    LoadFunc
	refresh RefreshFunc // nil => static key
	mu      sync.Mutex  // guards snap/loaded/quar
	snap    AuthSnapshot
	loaded  bool
	quar    *AuthError // permanent-failure quarantine; cleared when the file mtime advances
}

// NewExternalAuthManager creates a manager with a 60s expiry skew.
func NewExternalAuthManager() *ExternalAuthManager {
	return &ExternalAuthManager{skew: 60 * time.Second, entries: map[string]*authEntry{}}
}

// globalAuthManager is the process-global manager shared by all credential
// resolution and (later) all worker-pool agents, so concurrent refreshes of the
// same auth file are collapsed regardless of how many Agents resolve it.
var globalAuthManager = NewExternalAuthManager()

// ExternalAuth returns the process-global auth manager.
func ExternalAuth() *ExternalAuthManager { return globalAuthManager }

// snapshot returns a copy of the current snapshot (after a consistency reload),
// without forcing a refresh. Used at credential-resolve time to read
// token/expiry/account together.
func (m *ExternalAuthManager) snapshot(ref AuthRef) (AuthSnapshot, bool) {
	e := m.entry(ref)
	if e == nil {
		return AuthSnapshot{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reload()
	return e.snap, true
}

// Register adds an auth source. Idempotent: the first registration for a key
// wins, so concurrent workers resolving the same file share one entry.
func (m *ExternalAuthManager) Register(ref AuthRef, load LoadFunc, refresh RefreshFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := ref.key()
	if _, ok := m.entries[k]; ok {
		return
	}
	m.entries[k] = &authEntry{ref: ref, load: load, refresh: refresh}
}

func (m *ExternalAuthManager) entry(ref AuthRef) *authEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.entries[ref.key()]
}

// fileMtime returns the file's mtime, or zero if unavailable.
func fileMtime(path string) time.Time {
	if path == "" {
		return time.Time{}
	}
	if fi, err := os.Stat(path); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}

// reload refreshes the in-memory snapshot from the source when needed. For
// OAuth files it reloads when the mtime advances (picks up an external
// `codex login` and clears any quarantine); for static keys it loads once.
// Caller must hold e.mu.
func (e *authEntry) reload() {
	// A credential that can no longer be loaded must STOP BEING USED. Both
	// branches below used to keep the cached snapshot when loading failed, so
	// deleting the credentials file or removing the provider entry left the
	// daemon holding the old token for the rest of its life: a logout that
	// revoked nothing. Losing a credential is a state change, not a read error
	// to ride out.
	if e.ref.Kind != AuthOAuthFile {
		// A static key backed by a file (the selfmind-auth store) can still be
		// deleted, and load-once never noticed. With no path there is nothing to
		// observe — the key came from config or the environment — so it keeps the
		// historical behavior and the resolver owns its lifetime.
		if e.loaded {
			if e.ref.Path != "" && fileMtime(e.ref.Path).IsZero() {
				e.forget()
			}
			return
		}
		snap, err := e.load(e.ref)
		if err != nil {
			e.forget()
			return
		}
		e.snap, e.loaded = snap, true
		return
	}
	mt := fileMtime(e.ref.Path)
	if e.loaded && !mt.After(e.snap.FileMtime) {
		// The file going away is exactly the case a same-or-older mtime hides:
		// fileMtime reports a zero time for a missing file, which never advances.
		if mt.IsZero() {
			e.forget()
		}
		return
	}
	snap, err := e.load(e.ref)
	if err != nil {
		e.forget()
		return
	}
	snap.FileMtime = mt
	e.snap, e.loaded = snap, true
	e.quar = nil // a newer file means a fresh login → leave quarantine
}

// forget drops a cached credential whose source is gone. Callers hold e.mu.
func (e *authEntry) forget() {
	e.snap = AuthSnapshot{}
	e.loaded = false
}

func (e *authEntry) fresh(skew time.Duration) bool {
	if e.snap.Token == "" {
		return false
	}
	return e.snap.ExpiresAt.IsZero() || time.Until(e.snap.ExpiresAt) > skew
}

// Token returns a currently-valid token, refreshing (single-flight) if stale.
// Returns the quarantine error (permanent) without a network call when set.
func (m *ExternalAuthManager) Token(ref AuthRef) (string, error) {
	e := m.entry(ref)
	if e == nil {
		return "", fmt.Errorf("modelruntime: auth source not registered: %s", ref.key())
	}
	e.mu.Lock()
	e.reload()
	if e.quar != nil {
		q := e.quar
		e.mu.Unlock()
		return "", q
	}
	if e.refresh == nil || e.fresh(m.skew) {
		tok := e.snap.Token
		e.mu.Unlock()
		return tok, nil
	}
	e.mu.Unlock()
	return m.doRefresh(e, false)
}

// ForceRefresh refreshes regardless of apparent validity (the 401-replay path),
// single-flight. Static-key sources just return the key.
func (m *ExternalAuthManager) ForceRefresh(ref AuthRef) (string, error) {
	e := m.entry(ref)
	if e == nil {
		return "", fmt.Errorf("modelruntime: auth source not registered: %s", ref.key())
	}
	return m.doRefresh(e, true)
}

// AccountID returns the provider account id (e.g. ChatGPT account_id) without
// refreshing.
func (m *ExternalAuthManager) AccountID(ref AuthRef) string {
	e := m.entry(ref)
	if e == nil {
		return ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reload()
	return e.snap.AccountID
}

// doRefresh performs the actual refresh under single-flight, so N concurrent
// callers trigger exactly one refresh (and one rotation of the refresh token).
func (m *ExternalAuthManager) doRefresh(e *authEntry, force bool) (string, error) {
	v, err, _ := m.sf.Do("refresh:"+e.ref.key(), func() (interface{}, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.reload()
		if e.quar != nil {
			return nil, e.quar
		}
		if !force && e.fresh(m.skew) {
			return e.snap.Token, nil
		}
		if e.refresh == nil { // static key
			return e.snap.Token, nil
		}
		// Cross-process lock: another SelfMind process sharing this auth file may
		// be refreshing concurrently. Hold an exclusive file lock, then reload —
		// if it already refreshed, reuse its token instead of rotating again.
		if e.ref.Kind == AuthOAuthFile && e.ref.Path != "" {
			release := lockAuthFile(e.ref.Path)
			defer release()
			e.reload()
			if e.quar != nil {
				return nil, e.quar
			}
			if !force && e.fresh(m.skew) {
				return e.snap.Token, nil
			}
		}
		next, aerr := e.refresh(e.ref, e.snap)
		if aerr != nil {
			if aerr.Permanent {
				e.quar = aerr
			}
			return nil, aerr
		}
		next.FileMtime = fileMtime(e.ref.Path)
		e.snap = next
		return next.Token, nil
	})
	if err != nil {
		if ae, ok := err.(*AuthError); ok {
			return "", ae
		}
		return "", err
	}
	if v == nil {
		return "", nil
	}
	return v.(string), nil
}

// atomicWriteFile writes data to path via a temp file + rename, mode 0600, so a
// concurrent reader never sees a partial file. Centralized here so every OAuth
// refresh writeback is crash/concurrency safe (Codex previously used a plain
// write; MiniMax already did temp+rename).
func atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".auth-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
