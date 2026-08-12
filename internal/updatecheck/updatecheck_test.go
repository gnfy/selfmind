package updatecheck

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
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

func TestCheckSucceedsWhenCacheCannotBeWritten(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"latest":"1.2.3"}`)
	}))
	defer server.Close()

	t.Setenv("SELFMIND_UPDATE_REGISTRY_URL", server.URL)
	// CachePath becomes <HOME>/.selfmind/update.json. Making .selfmind a
	// regular file makes persistence fail on every OS without relying on Unix
	// permission semantics (root can write through chmod-based tests).
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".selfmind"), []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	result, err := Check(context.Background(), "1.2.0", "latest")
	if err != nil {
		t.Fatalf("registry success must survive cache failure: %v", err)
	}
	if result.Latest != "1.2.3" || !result.UpdateAvailable() {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestWriteCacheConcurrentWritersRemainReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update.json")
	const writers = 16
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- writeCache(path, Result{
				Current:   "1.0.0",
				Latest:    fmt.Sprintf("1.0.%d", i),
				Channel:   "latest",
				CheckedAt: time.Now().UTC(),
			})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent write failed: %v", err)
		}
	}
	if _, err := ReadCache(path); err != nil {
		t.Fatalf("final cache is unreadable: %v", err)
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
		{"auto", "v0.1.0-dev", "next"},        // dev builds track next
		{"auto", "not-a-version", "next"},     // unparseable = non-release build
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

// TestParseInterval pins the clamp semantics: sub-minute values clamp UP to
// the one-minute floor (the old behavior silently replaced "30s" with the
// 24h default — the opposite of the user's intent), parse failures fall back
// to the default, and the default itself is the per-startup 15m throttle.
func TestParseInterval(t *testing.T) {
	if got := ParseInterval("30s"); got != time.Minute {
		t.Fatalf("ParseInterval(30s) = %v, want 1m clamp", got)
	}
	if got := ParseInterval("2h"); got != 2*time.Hour {
		t.Fatalf("ParseInterval(2h) = %v, want 2h", got)
	}
	if got := ParseInterval(""); got != defaultInterval {
		t.Fatalf("ParseInterval(\"\") = %v, want default %v", got, defaultInterval)
	}
	if got := ParseInterval("garbage"); got != defaultInterval {
		t.Fatalf("ParseInterval(garbage) = %v, want default %v", got, defaultInterval)
	}
	if defaultInterval != 15*time.Minute {
		t.Fatalf("defaultInterval = %v, want the per-startup 15m throttle", defaultInterval)
	}
}

// TestComparePrereleaseNumericRollover pins the 9->10 rollover that silently
// disabled the update prompt: lexical ordering put every beta.10+ release
// BELOW the running beta.9, so UpdateAvailable stayed false forever.
func TestComparePrereleaseNumericRollover(t *testing.T) {
	cases := []struct {
		left, right string
		want        int
	}{
		{"0.1.0-beta.10", "0.1.0-beta.9", 1},
		{"0.1.0-beta.11", "0.1.0-beta.9", 1},
		{"0.1.0-beta.100", "0.1.0-beta.20", 1},
		{"0.1.0-beta.9", "0.1.0-beta.10", -1},
		{"0.1.0-beta.9", "0.1.0-beta.9", 0},
		{"0.1.0-beta.9", "0.1.0-beta.8", 1},
		// Numeric identifiers rank below alphanumeric ones (SemVer 11.4.3).
		{"0.1.0-beta.1", "0.1.0-beta.rc", -1},
		// A longer identifier set wins when the prefix matches (SemVer 11.4.4).
		{"0.1.0-beta.1.1", "0.1.0-beta.1", 1},
		// Release always outranks any prerelease of the same numbers.
		{"0.1.0", "0.1.0-beta.10", 1},
	}
	for _, tc := range cases {
		if got := Compare(tc.left, tc.right); got != tc.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", tc.left, tc.right, got, tc.want)
		}
	}
}

func TestUpdateAvailableAcrossBetaRollover(t *testing.T) {
	result := Result{Current: "0.1.0-beta.9", Latest: "0.1.0-beta.10"}
	if !result.UpdateAvailable() {
		t.Fatal("beta.10 must be offered as an update over beta.9")
	}
}
