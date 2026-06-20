package weixin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAES128ECBRoundTrip(t *testing.T) {
	key := []byte("1234567890abcdef")
	plain := []byte("hello weixin media")
	encrypted, err := aes128ECBEncrypt(plain, key)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(encrypted, plain) {
		t.Fatal("ciphertext should differ from plaintext")
	}
	decrypted, err := aes128ECBDecrypt(encrypted, key)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decrypted, plain) {
		t.Fatalf("decrypted = %q, want %q", decrypted, plain)
	}
}

func TestSplitTextForDelivery(t *testing.T) {
	parts := splitTextForDelivery("a\nb\nc", 2000, true)
	if len(parts) != 3 || parts[0] != "a" || parts[2] != "c" {
		t.Fatalf("line split = %+v", parts)
	}
	parts = splitTextForDelivery(strings.Repeat("x", 11), 5, false)
	if len(parts) != 3 {
		t.Fatalf("chunk split = %+v", parts)
	}
}

func TestExtractMediaPathsSupportsMarkerAndExistingLocalFiles(t *testing.T) {
	dir := t.TempDir()
	image := filepath.Join(dir, "out.png")
	if err := os.WriteFile(image, []byte("png"), 0600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "main.go")
	if err := os.WriteFile(source, []byte("package main"), 0600); err != nil {
		t.Fatal(err)
	}
	paths, cleaned := extractMediaPaths("MEDIA:" + image + "\nSee " + image + "\nCode " + source)
	if strings.Contains(cleaned, "MEDIA:") {
		t.Fatalf("marker was not removed: %q", cleaned)
	}
	if len(paths) != 1 || paths[0] != image {
		t.Fatalf("paths = %+v", paths)
	}
}

func TestCredentialsAndContextTokenPersistence(t *testing.T) {
	home := t.TempDir()
	cred := &Credentials{AccountID: "wx-account", Token: "secret", BaseURL: DefaultBaseURL}
	if err := SaveCredentials(home, cred); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadCredentials(home, "wx-account")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.AccountID != cred.AccountID || loaded.Token != cred.Token {
		t.Fatalf("credentials = %+v", loaded)
	}
	store := NewContextTokenStore(home)
	store.Set("wx-account", "peer-1", "ctx-token")
	restored := NewContextTokenStore(home)
	restored.Restore("wx-account")
	if got := restored.Get("wx-account", "peer-1"); got != "ctx-token" {
		t.Fatalf("context token = %q", got)
	}
	if filepath.Base(AccountFilePath(home, "wx-account")) == "" {
		t.Fatal("account file path should be non-empty")
	}
}

func TestSendTextUsesIlinkMessageFormat(t *testing.T) {
	var captured map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+epSendMessage {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			t.Fatalf("authorization = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"ret":0,"errcode":0}`))
	}))
	defer server.Close()

	client := NewClient(RuntimeConfig{
		AccountID:        "wx-account",
		Token:            "token",
		BaseURL:          server.URL,
		CDNBaseURL:       server.URL,
		SendChunkRetries: 1,
		SendChunkDelay:   1,
		HomeDir:          t.TempDir(),
	})
	if err := client.Send(context.Background(), "peer-1", "[doc](https://example.com)"); err != nil {
		t.Fatal(err)
	}
	msg, _ := captured["msg"].(map[string]interface{})
	if msg == nil || msg["to_user_id"] != "peer-1" {
		t.Fatalf("msg = %+v", captured["msg"])
	}
	items, _ := msg["item_list"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("items = %+v", msg["item_list"])
	}
	textItem := nestedMap(items[0].(map[string]interface{}), "text_item")
	if got := stringFromMap(textItem, "text"); got != "doc (https://example.com)" {
		t.Fatalf("text = %q", got)
	}
	if captured["base_info"] == nil {
		t.Fatalf("base_info missing: %+v", captured)
	}
}
