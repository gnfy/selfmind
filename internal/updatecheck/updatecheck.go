package updatecheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultRegistryURL = "https://registry.npmjs.org/-/package/@selfmind%2Fcli/dist-tags"
	// RequestTimeout bounds every registry lookup. Startup checks run in the
	// background, while explicit check/update commands need enough room for
	// WSL DNS and proxy handshakes that routinely exceed the old 8s budget.
	RequestTimeout = 15 * time.Second
	// defaultInterval throttles the background refresh: a TUI startup
	// re-checks unless the cache is younger than this. 15 minutes means
	// "effectively every startup, but never a request storm from rapid
	// restarts" — sized for the current daily beta release cadence (codex uses
	// 20h, hermes 6h; both are stable weekly-release products). Raise via
	// `updates.check_interval` when the release pace slows.
	defaultInterval = 15 * time.Minute
	// minInterval is the floor for configured intervals: values below it are
	// clamped UP to it, never silently replaced with the default (the old
	// behavior turned "30s" into 24h — the opposite of the user's intent).
	minInterval = time.Minute
)

type Result struct {
	Current   string    `json:"current"`
	Latest    string    `json:"latest"`
	Channel   string    `json:"channel"`
	CheckedAt time.Time `json:"checked_at"`
}

func (r Result) UpdateAvailable() bool {
	return Compare(r.Latest, r.Current) > 0
}

func CachePath() string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return filepath.Join(".selfmind", "update.json")
	}
	return filepath.Join(home, ".selfmind", "update.json")
}

// InvalidateCache removes the cached check result. Callers use it after an
// install performed by a binary whose own version is unreliable (e.g. the
// post-install verify step failed), so the next launch re-checks instead of
// trusting a row written by a replaced binary.
func InvalidateCache() error {
	err := os.Remove(CachePath())
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}

func ReadCache(path string) (Result, error) {
	if strings.TrimSpace(path) == "" {
		path = CachePath()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Result{}, err
	}
	var result Result
	if err := json.Unmarshal(data, &result); err != nil {
		return Result{}, err
	}
	return result, nil
}

func Check(ctx context.Context, current, channel string) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	channel = normalizeChannel(channel)
	registryURL := strings.TrimSpace(os.Getenv("SELFMIND_UPDATE_REGISTRY_URL"))
	if registryURL == "" {
		registryURL = defaultRegistryURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, registryURL, nil)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "selfmind/"+strings.TrimPrefix(current, "v"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Result{}, fmt.Errorf("npm registry returned HTTP %d", resp.StatusCode)
	}
	var tags map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return Result{}, fmt.Errorf("decode npm dist-tags: %w", err)
	}
	latest := strings.TrimSpace(tags[channel])
	if latest == "" {
		return Result{}, fmt.Errorf("npm dist-tag %q is not available", channel)
	}
	result := Result{
		Current:   normalizeVersion(current),
		Latest:    normalizeVersion(latest),
		Channel:   channel,
		CheckedAt: time.Now().UTC(),
	}
	// Cache persistence is advisory. A read-only HOME, a concurrent startup,
	// or a transient filesystem error must not turn a successful registry
	// lookup into "Update check failed". The next startup may re-check sooner,
	// but the caller still receives the authoritative dist-tag result.
	_ = writeCache(CachePath(), result)
	return result, nil
}

func Fresh(result Result, interval time.Duration) bool {
	if interval <= 0 {
		interval = defaultInterval
	}
	return !result.CheckedAt.IsZero() && time.Since(result.CheckedAt) < interval
}

func ParseInterval(raw string) time.Duration {
	interval, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return defaultInterval
	}
	if interval < minInterval {
		return minInterval
	}
	return interval
}

func Compare(left, right string) int {
	a, okA := parseVersion(left)
	b, okB := parseVersion(right)
	if !okA || !okB {
		return strings.Compare(normalizeVersion(left), normalizeVersion(right))
	}
	for i := 0; i < 3; i++ {
		if a.numbers[i] < b.numbers[i] {
			return -1
		}
		if a.numbers[i] > b.numbers[i] {
			return 1
		}
	}
	if a.pre == b.pre {
		return 0
	}
	if a.pre == "" {
		return 1
	}
	if b.pre == "" {
		return -1
	}
	return comparePrerelease(a.pre, b.pre)
}

// comparePrerelease orders two non-empty prerelease segments by SemVer §11.4:
// dot-separated identifiers compare field by field, numeric identifiers compare
// NUMERICALLY, a numeric identifier ranks below a non-numeric one, and when one
// side is a prefix of the other the longer set wins.
//
// A plain strings.Compare here silently broke the update prompt at the 9->10
// rollover: "beta.10" sorts BEFORE "beta.9" lexicographically, so every release
// from beta.10 onward read as older than the running beta.9 and no update was
// ever offered.
func comparePrerelease(left, right string) int {
	a := strings.Split(left, ".")
	b := strings.Split(right, ".")
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] == b[i] {
			continue
		}
		aNum, aIsNum := prereleaseNumber(a[i])
		bNum, bIsNum := prereleaseNumber(b[i])
		switch {
		case aIsNum && bIsNum:
			if aNum != bNum {
				if aNum < bNum {
					return -1
				}
				return 1
			}
		case aIsNum:
			// Numeric identifiers have lower precedence than alphanumeric ones.
			return -1
		case bIsNum:
			return 1
		default:
			return strings.Compare(a[i], b[i])
		}
	}
	if len(a) == len(b) {
		return 0
	}
	if len(a) < len(b) {
		return -1
	}
	return 1
}

// prereleaseNumber reports whether an identifier is purely numeric, and its
// value. Leading zeros are not special-cased: SemVer forbids them, and treating
// "01" as 1 keeps a malformed tag ordered sanely instead of flipping to the
// lexical branch.
func prereleaseNumber(identifier string) (int, bool) {
	if identifier == "" {
		return 0, false
	}
	value, err := strconv.Atoi(identifier)
	if err != nil || value < 0 {
		return 0, false
	}
	return value, true
}

func normalizeChannel(channel string) string {
	switch strings.ToLower(strings.TrimSpace(channel)) {
	case "next", "beta":
		return "next"
	default:
		return "latest"
	}
}

// IsPrerelease reports whether the version carries a prerelease segment
// (e.g. 0.1.0-beta.5). Unparseable versions count as prerelease: they can
// only come from non-release builds.
func IsPrerelease(version string) bool {
	parsed, ok := parseVersion(version)
	if !ok {
		return true
	}
	return parsed.pre != ""
}

// ResolveChannel turns the configured channel into an effective dist-tag.
// The channel identity lives in the artifact, not in config: "auto" (or
// empty) follows the version line the running binary came from — a
// prerelease build can only have been installed from `next`, a stable build
// from `latest`. Explicit "latest"/"next" ("beta" normalizes to next) is a
// user pin and always wins.
func ResolveChannel(configured, runningVersion string) string {
	switch strings.ToLower(strings.TrimSpace(configured)) {
	case "next", "beta":
		return "next"
	case "latest":
		return "latest"
	default: // "auto", "", or anything unrecognized: follow the binary
		if IsPrerelease(runningVersion) {
			return "next"
		}
		return "latest"
	}
}

func normalizeVersion(version string) string {
	return strings.TrimPrefix(strings.TrimSpace(version), "v")
}

type parsedVersion struct {
	numbers [3]int
	pre     string
}

func parseVersion(version string) (parsedVersion, bool) {
	version = normalizeVersion(version)
	main, pre, _ := strings.Cut(version, "-")
	parts := strings.Split(main, ".")
	if len(parts) != 3 {
		return parsedVersion{}, false
	}
	var parsed parsedVersion
	for i, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 {
			return parsedVersion{}, false
		}
		parsed.numbers[i] = value
	}
	parsed.pre = pre
	return parsed, true
}

func writeCache(path string, result Result) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("update cache path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".update-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
