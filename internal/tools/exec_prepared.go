package tools

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"sync"

	"selfmind/internal/tools/envprofiles"
)

// Per-lease profile preparation.
//
// Preparation is a LEASE-level fact, not a per-command one: the state overlay
// lives in the lease's state directory, and every command of that lease sees the
// same one. Re-running it per command would copy the same credential state
// again for every call — bounded, but pure waste once the host inventory is
// prepared for the whole lease.
//
// The cache is keyed by the lease's state directory PLUS the exact profile set
// and the inputs that can change what preparation produces (home, trust,
// credential access). A changed key prepares again rather than reusing a
// preparation that was built under a different policy.
var preparedProfiles = struct {
	sync.Mutex
	entries map[string]envprofiles.Result
}{entries: map[string]envprofiles.Result{}}

// preparedProfilesLimit bounds the cache. Entries are cheap (paths and notes,
// never credential values), but a long-lived daemon must not accumulate one per
// lease forever; the map is cleared wholesale when it grows past the limit,
// which costs one re-preparation per active lease and cannot leak.
const preparedProfilesLimit = 512

func preparedProfilesKey(stateDir string, profiles []*envprofiles.EnvProfile, ctx envprofiles.ApplyContext) string {
	ids := make([]string, 0, len(profiles))
	for _, profile := range profiles {
		if profile != nil {
			ids = append(ids, profile.ID)
		}
	}
	return fmt.Sprintf("%s|%s|%s|%v|%s",
		strings.TrimSpace(stateDir),
		strings.Join(ids, ","),
		strings.TrimSpace(ctx.Trust),
		ctx.HasCredentialRead,
		shortHash(ctx.Home+"\x00"+ctx.ToolchainRoot))
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum[:8])
}

// lookupPreparedProfiles returns a previous preparation for this lease, after
// verifying the state directory still exists. The scratch sweeper may have
// removed it, in which case the cached absolute paths point at nothing and the
// preparation must be redone rather than handed to a sandbox.
func lookupPreparedProfiles(stateDir string, profiles []*envprofiles.EnvProfile, ctx envprofiles.ApplyContext) (envprofiles.Result, bool) {
	key := preparedProfilesKey(stateDir, profiles, ctx)
	preparedProfiles.Lock()
	result, ok := preparedProfiles.entries[key]
	preparedProfiles.Unlock()
	if !ok {
		return envprofiles.Result{}, false
	}
	if info, err := os.Stat(stateDir); err != nil || !info.IsDir() {
		preparedProfiles.Lock()
		delete(preparedProfiles.entries, key)
		preparedProfiles.Unlock()
		return envprofiles.Result{}, false
	}
	return result, true
}

func storePreparedProfiles(stateDir string, profiles []*envprofiles.EnvProfile, ctx envprofiles.ApplyContext, result envprofiles.Result) {
	key := preparedProfilesKey(stateDir, profiles, ctx)
	preparedProfiles.Lock()
	defer preparedProfiles.Unlock()
	if len(preparedProfiles.entries) >= preparedProfilesLimit {
		preparedProfiles.entries = map[string]envprofiles.Result{}
	}
	preparedProfiles.entries[key] = result
}

// ForgetPreparedProfiles drops cached preparations for one lease's state
// directory. Callers that delete a lease's scratch use it so a recreated lease
// with the same id cannot inherit paths that no longer exist.
func ForgetPreparedProfiles(stateDir string) {
	prefix := strings.TrimSpace(stateDir)
	if prefix == "" {
		return
	}
	preparedProfiles.Lock()
	defer preparedProfiles.Unlock()
	for key := range preparedProfiles.entries {
		if strings.HasPrefix(key, prefix+"|") {
			delete(preparedProfiles.entries, key)
		}
	}
}
