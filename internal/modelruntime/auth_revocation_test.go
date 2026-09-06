package modelruntime

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestVanishedCredentialIsForgotten pins cache invalidation. A credential whose
// source is gone must stop being served: both branches of reload used to keep
// the cached snapshot when loading failed, so deleting the credentials file or
// removing the provider entry left the daemon holding the old token — a logout
// that revoked nothing.
func TestVanishedCredentialIsForgotten(t *testing.T) {
	for _, kind := range []AuthKind{AuthOAuthFile, AuthStaticKey} {
		t.Run(map[AuthKind]string{AuthStaticKey: "static-key", AuthOAuthFile: "oauth-file"}[kind], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "auth.json")
			if err := os.WriteFile(path, []byte(`{"token":"live-token"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			present := true
			entry := &authEntry{
				ref: AuthRef{Kind: kind, Path: path},
				load: func(ref AuthRef) (AuthSnapshot, error) {
					if !present {
						return AuthSnapshot{}, fmt.Errorf("credential source is gone")
					}
					return AuthSnapshot{Token: "live-token", ExpiresAt: time.Now().Add(time.Hour)}, nil
				},
			}

			entry.reload()
			if entry.snap.Token != "live-token" {
				t.Fatalf("a present credential should load, got %q", entry.snap.Token)
			}

			// The person logs out: the file goes away.
			present = false
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			entry.reload()
			if entry.snap.Token != "" || entry.loaded {
				t.Fatalf("a vanished credential must be forgotten, still holding %q (loaded=%v)",
					entry.snap.Token, entry.loaded)
			}
		})
	}
}
