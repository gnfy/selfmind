package executionenv

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Environment snapshots.
//
// A long-lived daemon cannot treat its own os.Environ() as the truth for tool
// child processes. The daemon inherits whatever shell first started it and
// keeps that copy for its whole lifetime, so a run's PATH can point at a
// session-scoped directory belonging to a shell that has since exited, and a
// re-read halfway through a run can silently change account or toolchain.
//
// A Snapshot is therefore taken once, bound to a run through its lease, and
// reused for every command of that run including retries. Values live only in
// this process; durable state stores the snapshot id, its generation, and three
// fingerprints. Nothing here imports the gateway or the tool layer, so the whole
// registry moves with the execution side if it is ever split out.

// Snapshot is one immutable environment binding.
type Snapshot struct {
	ID         string
	Generation int64
	SampledAt  time.Time
	// Source records how the environment was obtained: "inherited" (the
	// daemon's own environment) or "login-shell" (an explicit re-sample).
	Source string
	// PrincipalFingerprint identifies the acting account/profile/context.
	PrincipalFingerprint string
	// EnvironmentFingerprint covers PATH/HOME/proxy/toolchain AFTER
	// normalization, so a new session-scoped directory does not read as a
	// changed environment.
	EnvironmentFingerprint string
	// CredentialSourceHash covers credential source names and configuration
	// paths, never values. A token rotation inside the same source does not
	// change it; switching account or config location does.
	CredentialSourceHash string
	// VolatileCount reports how many session-scoped entries were dropped
	// before fingerprinting. Diagnostics only; never part of a fingerprint.
	VolatileCount int

	env []string // never exported, never serialized
}

// Env returns a defensive copy of the child-process environment.
func (s *Snapshot) Env() []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s.env))
	copy(out, s.env)
	return out
}

// Age reports how long ago the snapshot was sampled.
func (s *Snapshot) Age(now time.Time) time.Duration {
	if s == nil || s.SampledAt.IsZero() {
		return 0
	}
	return now.Sub(s.SampledAt)
}

// Matches reports whether another sample describes the same environment. All
// three fingerprints must agree: a principal change means a different account,
// an environment change means a different toolchain or proxy, and a credential
// source change means the credential is being read from somewhere else.
func (s *Snapshot) Matches(other *Snapshot) bool {
	if s == nil || other == nil {
		return false
	}
	return s.PrincipalFingerprint == other.PrincipalFingerprint &&
		s.EnvironmentFingerprint == other.EnvironmentFingerprint &&
		s.CredentialSourceHash == other.CredentialSourceHash
}

// Registry owns the process's environment snapshots.
type Registry struct {
	mu         sync.RWMutex
	current    *Snapshot
	byID       map[string]*Snapshot
	byLease    map[string]string // lease id -> snapshot id
	generation int64
}

func NewRegistry() *Registry {
	return &Registry{byID: map[string]*Snapshot{}, byLease: map[string]string{}}
}

// Install records a new environment sample and makes it current. env must
// already be filtered by the caller's process-environment policy: the registry
// stores what it is given and never reads os.Environ() for a child.
//
// A sample identical to the current one keeps the existing snapshot and
// generation, so an idle refresh does not force new runs onto a new binding.
func (r *Registry) Install(env []string, source, principalFingerprint string, credentialSources []string) *Snapshot {
	normalized, volatile := normalizeEnvForFingerprint(env)
	candidate := &Snapshot{
		Source:                 strings.TrimSpace(source),
		PrincipalFingerprint:   strings.TrimSpace(principalFingerprint),
		EnvironmentFingerprint: fingerprint(normalized),
		CredentialSourceHash:   fingerprint(normalizeCredentialSources(env, credentialSources)),
		VolatileCount:          volatile,
		env:                    append([]string{}, env...),
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current != nil && r.current.Matches(candidate) {
		// Same environment: keep the existing snapshot, generation and VALUES.
		// A snapshot is immutable once installed — that immutability is the
		// whole point of binding a run to one, so an idle re-sample must not
		// mutate what an in-flight run resolves. A credential rotated inside an
		// unchanged source is picked up where it actually lives (the credential
		// file read at command time), not by rewriting a bound environment.
		return r.current
	}
	r.generation++
	candidate.Generation = r.generation
	candidate.SampledAt = time.Now()
	candidate.ID = fmt.Sprintf("envsnap_%d_%s", candidate.Generation, shortHash(candidate.EnvironmentFingerprint))
	if r.byID == nil {
		r.byID = map[string]*Snapshot{}
	}
	r.byID[candidate.ID] = candidate
	r.current = candidate
	return candidate
}

// Sample builds a snapshot WITHOUT installing it, so a caller can compare a
// fresh reading against the binding a run already holds.
func (r *Registry) Sample(env []string, source, principalFingerprint string, credentialSources []string) *Snapshot {
	normalized, volatile := normalizeEnvForFingerprint(env)
	return &Snapshot{
		Source:                 strings.TrimSpace(source),
		PrincipalFingerprint:   strings.TrimSpace(principalFingerprint),
		EnvironmentFingerprint: fingerprint(normalized),
		CredentialSourceHash:   fingerprint(normalizeCredentialSources(env, credentialSources)),
		VolatileCount:          volatile,
		env:                    append([]string{}, env...),
	}
}

// Current returns the newest installed snapshot, or nil before the first
// Install.
func (r *Registry) Current() *Snapshot {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.current
}

// Get resolves a snapshot by id.
func (r *Registry) Get(id string) (*Snapshot, bool) {
	if r == nil || strings.TrimSpace(id) == "" {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	snapshot, ok := r.byID[strings.TrimSpace(id)]
	return snapshot, ok
}

// BindLease pins a lease to a snapshot so every command of that run — including
// a retry after a failure — resolves the same environment.
func (r *Registry) BindLease(leaseID, snapshotID string) {
	leaseID = strings.TrimSpace(leaseID)
	snapshotID = strings.TrimSpace(snapshotID)
	if r == nil || leaseID == "" || snapshotID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byLease == nil {
		r.byLease = map[string]string{}
	}
	r.byLease[leaseID] = snapshotID
}

// ForLease resolves the snapshot bound to a lease.
func (r *Registry) ForLease(leaseID string) (*Snapshot, bool) {
	leaseID = strings.TrimSpace(leaseID)
	if r == nil || leaseID == "" {
		return nil, false
	}
	r.mu.RLock()
	snapshotID := r.byLease[leaseID]
	r.mu.RUnlock()
	return r.Get(snapshotID)
}

// ReleaseLease drops a finished run's binding. The snapshot itself stays
// available for other runs on the same generation.
func (r *Registry) ReleaseLease(leaseID string) {
	leaseID = strings.TrimSpace(leaseID)
	if r == nil || leaseID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byLease, leaseID)
}

// EnvironmentChangedError reports that a run's recorded environment no longer
// matches the host. It is a lifecycle decision, not a diagnosable failure: the
// caller parks the work as waiting_user rather than continuing under an
// environment the run never started with.
type EnvironmentChangedError struct {
	LeaseID string
	Changed []string
}

func (e *EnvironmentChangedError) Error() string {
	changed := strings.Join(e.Changed, ", ")
	if changed == "" {
		changed = "environment"
	}
	return "environment_changed: " + changed + " changed since this run started"
}

// DescribeEnvironmentChange names which non-secret dimensions moved. It never
// includes a value, a path, or a credential name.
func DescribeEnvironmentChange(lease *Lease, snapshot *Snapshot) []string {
	if lease == nil || snapshot == nil {
		return nil
	}
	changed := make([]string, 0, 3)
	if lease.PrincipalFingerprint != snapshot.PrincipalFingerprint {
		changed = append(changed, "account/profile")
	}
	if lease.EnvironmentFingerprint != snapshot.EnvironmentFingerprint {
		changed = append(changed, "PATH/HOME/proxy")
	}
	if lease.CredentialSourceHash != snapshot.CredentialSourceHash {
		changed = append(changed, "credential source")
	}
	return changed
}

var (
	defaultRegistryMu sync.RWMutex
	defaultRegistry   = NewRegistry()
)

// SetDefaultRegistry installs the process-wide registry. Ownership is explicit:
// the application wiring installs it at startup; nothing constructs one lazily.
func SetDefaultRegistry(r *Registry) {
	if r == nil {
		return
	}
	defaultRegistryMu.Lock()
	defer defaultRegistryMu.Unlock()
	defaultRegistry = r
}

// DefaultRegistry returns the process-wide registry.
func DefaultRegistry() *Registry {
	defaultRegistryMu.RLock()
	defer defaultRegistryMu.RUnlock()
	return defaultRegistry
}

// fingerprintedEnvKeys are the variables whose values define the execution
// environment for fingerprint purposes. Everything else is either irrelevant to
// "is this the same environment" or belongs to the credential-source hash.
var fingerprintedEnvKeys = map[string]bool{
	"PATH": true, "HOME": true, "SHELL": true, "LANG": true, "LC_ALL": true,
	"NO_PROXY": true, "HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true,
}

// volatileEnvPathMarkers identify session-scoped directories that belong to one
// shell invocation. A daemon started from such a shell keeps the path forever,
// and every later sample sees a different one, so including them would make the
// environment look changed on every restart while nothing meaningful moved.
// These are generic conventions, not tool-specific paths.
var volatileEnvPathMarkers = []string{"/run/user/", "/fnm_multishells/", "/nvm/versions/", "/.cache/nodenv/"}

// normalizeEnvForFingerprint reduces an environment to a stable, ordered
// description. It returns the description lines plus the number of volatile
// entries dropped from PATH.
func normalizeEnvForFingerprint(env []string) ([]string, int) {
	volatile := 0
	lines := make([]string, 0, len(fingerprintedEnvKeys))
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		key := strings.ToUpper(strings.TrimSpace(name))
		if !ok || !fingerprintedEnvKeys[key] {
			continue
		}
		if key == "PATH" {
			kept, dropped := normalizePathValue(value)
			volatile += dropped
			lines = append(lines, "PATH="+strings.Join(kept, string(os.PathListSeparator)))
			continue
		}
		lines = append(lines, key+"="+strings.TrimSpace(value))
	}
	sort.Strings(lines)
	return lines, volatile
}

// normalizePathValue keeps the PATH entries that still exist and are not
// session-scoped, preserving order.
func normalizePathValue(value string) (kept []string, dropped int) {
	for _, dir := range filepath.SplitList(value) {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		if isVolatileEnvPath(dir) {
			dropped++
			continue
		}
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			dropped++
			continue
		}
		kept = append(kept, dir)
	}
	return kept, dropped
}

func isVolatileEnvPath(dir string) bool {
	normalized := strings.ToLower(filepath.ToSlash(dir))
	for _, marker := range volatileEnvPathMarkers {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

// credentialSourcePathKeyMarkers select variables that POINT AT credential or
// configuration state. Their values are locations, not secrets, and a change of
// location is a change of credential source. Matching is by generic naming
// convention so no per-vendor list is required.
var credentialSourcePathKeyMarkers = []string{"CONFIG", "_HOME", "PROFILE", "ACCOUNT", "CONTEXT"}

// normalizeCredentialSources describes where credentials come from: the
// caller-supplied source names plus any configuration-location variables. Only
// names and paths are included; a value that is itself credential-shaped is
// excluded by the caller before it reaches here.
func normalizeCredentialSources(env []string, credentialSources []string) []string {
	lines := make([]string, 0, len(credentialSources)+4)
	for _, source := range credentialSources {
		if source = strings.TrimSpace(source); source != "" {
			lines = append(lines, "source="+strings.ToUpper(source))
		}
	}
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		key := strings.ToUpper(strings.TrimSpace(name))
		value = strings.TrimSpace(value)
		if value == "" || isCredentialShapedEnvName(key) {
			continue
		}
		for _, marker := range credentialSourcePathKeyMarkers {
			if strings.Contains(key, marker) {
				lines = append(lines, key+"="+value)
				break
			}
		}
	}
	sort.Strings(lines)
	return uniqueSorted(lines)
}

// isCredentialShapedEnvName mirrors the tool layer's credential-shape test so a
// secret value can never be folded into a fingerprint input, even indirectly.
func isCredentialShapedEnvName(name string) bool {
	name = strings.ToUpper(strings.TrimSpace(name))
	for _, marker := range []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "API_KEY", "APIKEY", "CREDENTIAL", "PRIVATE_KEY", "ACCESS_KEY"} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

func uniqueSorted(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func fingerprint(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return fmt.Sprintf("%x", sum[:12])
}

func shortHash(value string) string {
	if len(value) >= 8 {
		return value[:8]
	}
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum[:4])
}
