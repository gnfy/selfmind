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

// Channel identity lives in the artifact: "auto" follows the running
// version's line, explicit values are pins that always win.
func TestResolveChannel(t *testing.T) {
	cases := []struct {
		configured string
		running    string
		want       string
	}{
		{"auto", "v0.1.0-beta.5", "next"},
		{"auto", "0.2.0", "latest"},
		{"", "v0.1.0-beta.5", "next"},
		{"", "1.0.0", "latest"},
		{"auto", "v0.1.0-dev", "next"},   // dev builds track next
		{"auto", "not-a-version", "next"}, // unparseable = non-release build
		{"latest", "v0.1.0-beta.5", "latest"}, // pin wins over inference
		{"next", "1.0.0", "next"},
		{"beta", "1.0.0", "next"},
		{"NEXT", "1.0.0", "next"},
	}
	for _, tc := range cases {
		if got := ResolveChannel(tc.configured, tc.running); got != tc.want {
			t.Errorf("ResolveChannel(%q, %q) = %q, want %q", tc.configured, tc.running, got, tc.want)
		}
	}
}

func TestIsPrerelease(t *testing.T) {
	if !IsPrerelease("v0.1.0-beta.5") || IsPrerelease("1.2.3") || !IsPrerelease("garbage") {
		t.Fatalf("prerelease detection wrong: beta=%v stable=%v garbage=%v",
			IsPrerelease("v0.1.0-beta.5"), IsPrerelease("1.2.3"), IsPrerelease("garbage"))
	}
}
