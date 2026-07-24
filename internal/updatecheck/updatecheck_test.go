package updatecheck

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestCompare(t *testing.T) {
	tests := []struct {
		left  string
		right string
		want  int
	}{
		{"v1.2.3", "1.2.3", 0},
		{"1.2.4", "1.2.3", 1},
		{"1.2.3-beta.1", "1.2.3", -1},
		{"2.0.0", "1.99.99", 1},
	}
	for _, test := range tests {
		t.Run(fmt.Sprintf("%s_%s", test.left, test.right), func(t *testing.T) {
			got := Compare(test.left, test.right)
			if got < 0 {
				got = -1
			} else if got > 0 {
				got = 1
			}
			if got != test.want {
				t.Fatalf("Compare(%q, %q) = %d, want %d", test.left, test.right, got, test.want)
			}
		})
	}
}

func TestCheckReadsDistTagAndWritesCache(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"latest":"1.2.3","next":"1.3.0-beta.1"}`)
	}))
	defer server.Close()

	t.Setenv("SELFMIND_UPDATE_REGISTRY_URL", server.URL)
	t.Setenv("HOME", t.TempDir())
	result, err := Check(context.Background(), "v1.2.0", "latest")
	if err != nil {
		t.Fatal(err)
	}
	if result.Latest != "1.2.3" || !result.UpdateAvailable() {
		t.Fatalf("unexpected result: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".selfmind", "update.json")); err != nil {
		t.Fatal(err)
	}
}
