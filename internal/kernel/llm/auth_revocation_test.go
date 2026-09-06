package llm

import "testing"

// TestApiKeyFromDoesNotResurrectARevokedCredential pins what a logout means.
// The transport captures a key at construction and is also given a dynamic
// getter; when the getter answers empty — the credentials file was deleted, the
// provider entry removed — that IS the answer. Falling back to the captured key
// meant the daemon kept authenticating with a credential the person had
// revoked, for as long as it stayed running.
func TestApiKeyFromDoesNotResurrectARevokedCredential(t *testing.T) {
	revoked := func() string { return "" }
	if got := apiKeyFrom("stale-key-captured-at-startup", revoked); got != "" {
		t.Fatalf("a revoked credential must not fall back to the captured key, got %q", got)
	}

	// A live getter still wins over the captured key.
	if got := apiKeyFrom("stale-key-captured-at-startup", func() string { return " fresh " }); got != "fresh" {
		t.Fatalf("dynamic credential should be used and trimmed, got %q", got)
	}

	// With no getter installed there is nothing dynamic to consult, so the
	// static key remains the credential.
	if got := apiKeyFrom(" static-only ", nil); got != "static-only" {
		t.Fatalf("static-only transport should keep its key, got %q", got)
	}
}
