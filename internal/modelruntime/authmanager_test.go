package modelruntime

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func expiredSnap(token string) AuthSnapshot {
	return AuthSnapshot{Token: token, ExpiresAt: time.Now().Add(-time.Hour)}
}

func validSnap(token string) AuthSnapshot {
	return AuthSnapshot{Token: token, ExpiresAt: time.Now().Add(time.Hour)}
}

func writeAuthFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAuthManagerSingleFlightRefresh is the core guarantee: N workers all
// seeing an expired token trigger exactly ONE refresh (one refresh_token
// rotation), and all receive the new token.
func TestAuthManagerSingleFlightRefresh(t *testing.T) {
	m := NewExternalAuthManager()
	ref := AuthRef{Provider: "codex-cli", Kind: AuthOAuthFile, Path: writeAuthFile(t)}
	var refreshCount int32
	m.Register(ref,
		func(AuthRef) (AuthSnapshot, error) { return expiredSnap("old"), nil },
		func(AuthRef, AuthSnapshot) (AuthSnapshot, *AuthError) {
			atomic.AddInt32(&refreshCount, 1)
			time.Sleep(50 * time.Millisecond) // hold the flight so callers overlap
			return validSnap("new"), nil
		},
	)

	const N = 24
	var wg sync.WaitGroup
	tokens := make([]string, N)
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tokens[i], errs[i] = m.Token(ref)
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&refreshCount); got != 1 {
		t.Fatalf("expected exactly 1 refresh under %d concurrent callers, got %d", N, got)
	}
	for i := 0; i < N; i++ {
		if errs[i] != nil || tokens[i] != "new" {
			t.Fatalf("caller %d got token=%q err=%v, want token=new", i, tokens[i], errs[i])
		}
	}
}

func TestAuthManagerQuarantinesPermanentFailureAndClearsOnRelogin(t *testing.T) {
	m := NewExternalAuthManager()
	path := writeAuthFile(t)
	ref := AuthRef{Provider: "codex-cli", Kind: AuthOAuthFile, Path: path}
	loggedIn := false
	var refreshCount int32
	m.Register(ref,
		func(AuthRef) (AuthSnapshot, error) {
			if loggedIn {
				return validSnap("fresh"), nil
			}
			return expiredSnap("old"), nil
		},
		func(AuthRef, AuthSnapshot) (AuthSnapshot, *AuthError) {
			atomic.AddInt32(&refreshCount, 1)
			return AuthSnapshot{}, &AuthError{Permanent: true, Reason: "refresh_token_reused", Actionable: "Codex login expired — run `codex login`."}
		},
	)

	if _, err := m.Token(ref); err == nil || !strings.Contains(err.Error(), "codex login") {
		t.Fatalf("want actionable permanent error, got %v", err)
	}
	if _, err := m.Token(ref); err == nil {
		t.Fatal("second call should still be quarantined")
	}
	if got := atomic.LoadInt32(&refreshCount); got != 1 {
		t.Fatalf("quarantine must stop retries: got %d refreshes", got)
	}

	// Simulate `codex login`: token becomes valid + file mtime advances.
	loggedIn = true
	future := time.Now().Add(time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	tok, err := m.Token(ref)
	if err != nil || tok != "fresh" {
		t.Fatalf("re-login should clear quarantine and return fresh token: token=%q err=%v", tok, err)
	}
}

func TestAuthManagerStaticKeyNeverRefreshes(t *testing.T) {
	m := NewExternalAuthManager()
	ref := AuthRef{Provider: "kimi-coding", Kind: AuthStaticKey}
	m.Register(ref,
		func(AuthRef) (AuthSnapshot, error) { return AuthSnapshot{Token: "sk-kimi-xxx"}, nil },
		nil, // no refresher
	)
	if tok, err := m.Token(ref); err != nil || tok != "sk-kimi-xxx" {
		t.Fatalf("static key Token = %q, %v", tok, err)
	}
	// ForceRefresh must be safe (no panic) and just return the key.
	if tok, err := m.ForceRefresh(ref); err != nil || tok != "sk-kimi-xxx" {
		t.Fatalf("static key ForceRefresh = %q, %v", tok, err)
	}
}

func TestAuthManagerTransientErrorIsRetryable(t *testing.T) {
	m := NewExternalAuthManager()
	ref := AuthRef{Provider: "minimax-oauth", Kind: AuthOAuthFile, Path: writeAuthFile(t)}
	var calls int32
	m.Register(ref,
		func(AuthRef) (AuthSnapshot, error) { return expiredSnap("old"), nil },
		func(AuthRef, AuthSnapshot) (AuthSnapshot, *AuthError) {
			if atomic.AddInt32(&calls, 1) == 1 {
				return AuthSnapshot{}, &AuthError{Permanent: false, Reason: "timeout"}
			}
			return validSnap("recovered"), nil
		},
	)

	if _, err := m.Token(ref); err == nil {
		t.Fatal("first (transient) refresh should error")
	}
	tok, err := m.Token(ref) // not quarantined → retried
	if err != nil || tok != "recovered" {
		t.Fatalf("transient failure must be retryable: token=%q err=%v", tok, err)
	}
}

func TestAtomicWriteFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := atomicWriteFile(path, []byte(`{"token":"x"}`)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != `{"token":"x"}` {
		t.Fatalf("read back = %q, %v", data, err)
	}
	if fi, _ := os.Stat(path); fi != nil && fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
}
