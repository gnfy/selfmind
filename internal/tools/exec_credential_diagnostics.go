package tools

import (
	"runtime"

	"selfmind/internal/tools/envprofiles"
)

// CredentialProfileDiagnostic describes one operator-credential profile on this
// host: whether its state exists here, whether this platform's isolation
// backend can run it, and which host paths a sandboxed run would make
// writable. It holds paths and reasons only, never what any file contains.
type CredentialProfileDiagnostic struct {
	Profile string
	// HostState reports that a state location the profile declares exists.
	HostState bool
	// Runnable reports that this platform's isolation backend can enforce the
	// profile's plan; Reason says why it cannot.
	Runnable bool
	Reason   string
	// InPlaceWrites are host subpaths a backend that cannot mount writes in
	// place. OverlayTargets are host paths a mounting backend shadows with
	// private directories instead, leaving the host unmodified.
	InPlaceWrites  []string
	OverlayTargets []string
}

// CredentialProfileDiagnostics evaluates every operator-credential profile
// against the daemon's environment snapshot and execution policy the way
// preparation would for a call that could reach any of them: credentials
// allowed and every configured profile considered. It plans only; nothing on
// the host changes.
func CredentialProfileDiagnostics() []CredentialProfileDiagnostic {
	enabled, _, _ := execSandboxPolicy()
	env := leaseProcessEnv(nil)
	ctx := envprofiles.ApplyContext{
		Home:           envValue(env, "HOME"),
		Lookup:         func(name string) (string, bool) { return lookupEnv(env, name) },
		Trust:          envprofiles.TrustTrusted,
		OpaquePrograms: true,
		InPlaceState:   true,
	}
	backend := IsolationBackendForPlatform(runtime.GOOS)
	available := backend != nil && backend.Available()
	out := make([]CredentialProfileDiagnostic, 0, len(envprofiles.Catalog))
	for _, id := range envprofiles.IDs() {
		profile, ok := envprofiles.ByID(id)
		if !ok || profile.CredentialAccess != envprofiles.CredentialAccessOperator {
			continue
		}
		diagnostic := CredentialProfileDiagnostic{Profile: id, HostState: envprofiles.HasHostState(profile, ctx)}
		profiles := []*envprofiles.EnvProfile{profile}
		switch {
		case !diagnostic.HostState:
		case !enabled:
			diagnostic.Reason = "exec_sandbox is disabled by configuration, so commands run on the host"
		case !available:
			diagnostic.Reason = "no isolation backend is available on this host"
		case backend.MountsToolState():
			diagnostic.Runnable = true
			diagnostic.OverlayTargets = envprofiles.MountStateTargets(profiles, ctx)
		default:
			plan, err := envprofiles.PlanHostStateWrites(profiles, ctx)
			if err != nil {
				diagnostic.Reason = err.Error()
				break
			}
			diagnostic.Runnable = true
			diagnostic.InPlaceWrites = plan.Paths()
		}
		out = append(out, diagnostic)
	}
	return out
}
