package llm

import "testing"

// TestApiKeyFromTreatsAnEmptyOverrideAsNoOverride pins what an empty dynamic
// getter MEANS. buildKeyGetter returns "" when the tenant secret store holds no
// per-provider override, and its own comment says the adapter should then fall
// back to its configured key. Reading that as "the credential was revoked" sends
// an empty key for every provider whose credential lives in config or the auth
// file — the ordinary case — and the daemon answered HTTP 401 on the very first
// turn after such a change.
//
// Revocation belongs where the credential lives: the auth manager stops serving
// a token whose source is gone (see modelruntime). It is not something to infer
// from a missing override.
func TestApiKeyFromTreatsAnEmptyOverrideAsNoOverride(t *testing.T) {
	if got := apiKeyFrom("configured-key", func() string { return "" }); got != "configured-key" {
		t.Fatalf("an empty override must fall back to the configured key, got %q", got)
	}

	// A real override wins, and is trimmed.
	if got := apiKeyFrom("configured-key", func() string { return " tenant-key " }); got != "tenant-key" {
		t.Fatalf("a tenant override should be used and trimmed, got %q", got)
	}

	// With no getter installed there is nothing dynamic to consult.
	if got := apiKeyFrom(" configured-key ", nil); got != "configured-key" {
		t.Fatalf("static-only transport should keep its key, got %q", got)
	}

	// And nothing anywhere yields nothing, rather than a stale value.
	if got := apiKeyFrom("", func() string { return "" }); got != "" {
		t.Fatalf("no credential at all must resolve to empty, got %q", got)
	}
}
