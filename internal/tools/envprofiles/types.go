// Package envprofiles is the tool environment profile catalog: the data that
// tells the execution engine which state a command-line tool needs in order to
// run inside an isolated sandbox.
//
// The problem it solves, measured on 2026-07-28: 26 of 293 sandboxed commands
// failed writing their OWN state directory under $HOME — gcloud cannot even
// READ credentials without writing `credentials.db`, so `gcloud auth list`,
// every `gcloud builds` call, and every GKE `kubectl` call (whose auth plugin
// shells out to gcloud) failed on first use. The model could not fix that from
// inside the sandbox, so it escalated to host execution until 28% of all
// commands ran outside isolation.
//
// The engine understands exactly five primitives — copy_in, map_ro, map_rw,
// env_redirect, write_back — and nothing about any particular tool. Everything
// vendor-specific is DATA in this catalog: a Go table, versioned with the
// binary and reviewed in a pull request, never configuration. It defines
// behaviour, not preference, and adding a tool must never require a new
// configuration key or a branch in engine code.
package envprofiles

// CredentialAccess declares what kind of host state a profile needs. The
// POLICY layer combines it with workspace trust; the profile never decides.
type CredentialAccess string

const (
	// CredentialAccessOperator needs the operator's real credential state
	// (gcloud, aws, gh, docker). Untrusted workspaces must not get it without
	// an explicit credential:read capability.
	CredentialAccessOperator CredentialAccess = "operator"
	// CredentialAccessToolchain needs only build/download caches with no
	// credential meaning (go, npm, pip, cargo). Withholding these does not
	// protect anything and makes every run a cold build, so untrusted
	// workspaces get them too — just redirected into SelfMind-owned storage.
	CredentialAccessToolchain CredentialAccess = "toolchain"
	// CredentialAccessNone needs no host state at all.
	CredentialAccessNone CredentialAccess = "none"
)

// StateSource locates host state as an ORDERED set of candidates rather than
// through string interpolation. An interpolation mini-language ("${VAR:-$HOME/x}")
// would be a second parser with its own runtime failure mode; a typed candidate
// list cannot fail to parse.
type StateSource struct {
	// EnvVar wins when the variable is present and non-empty.
	EnvVar string
	// HomeRelPath is the fallback, relative to the operator's home directory.
	HomeRelPath string
	// List marks a variable that legitimately holds SEVERAL paths separated by
	// the platform list separator. KUBECONFIG is the case that matters: treating
	// "a:b" as one path silently mapped a file that does not exist, so the
	// kubeconfig was never read and its credential helper was never detected.
	List bool
}

// CopyIn copies host state into the run's state overlay so the tool can write
// to it without touching the operator's real files. It is BOUNDED on purpose:
// `~/.config/gcloud` measured 231 MB, of which 231 MB was `logs/` — copying a
// directory wholesale is not viable, and "best effort" copying would silently
// produce a half-populated credential store.
type CopyIn struct {
	From StateSource
	// Include lists relative glob patterns to copy. Required: an empty include
	// list would mean "everything", which is the failure mode above.
	Include []string
	// Exclude removes matches from Include (logs, caches).
	Exclude []string
	// MaxBytes, MaxFiles, MaxDepth bound the copy. Exceeding any of them is a
	// hard, diagnosable failure — never a truncated copy.
	MaxBytes int64
	MaxFiles int
	MaxDepth int
}

// TargetKind selects where a redirected path lives.
type TargetKind int

const (
	// TargetLeaseState is the run's own state overlay: <lease>/state/<rel>.
	TargetLeaseState TargetKind = iota
	// TargetToolchain is the person-level persistent cache. It deliberately
	// outlives a run because a per-run build cache is always cold.
	TargetToolchain
	// TargetScratch is the run's shared temp directory.
	TargetScratch
	// TargetHostPath keeps the resolved host location (read-only mappings).
	TargetHostPath
)

// MapRO maps approved host state read-only into the sandbox.
type MapRO struct {
	From StateSource
}

// MapRW creates a writable directory for the tool's own churn (logs, caches).
// Persistent selects the person-level toolchain root instead of the run's
// state overlay.
type MapRW struct {
	Key        string
	Persistent bool
}

// MapRWAt binds a writable state directory OVER a host path inside the sandbox.
//
// It exists for state whose location is not configurable. The AWS CLI derives
// its SSO token cache from HOME (`~/.aws/sso/cache`), not from AWS_CONFIG_FILE,
// so no env_redirect can move it — and it is WRITTEN on every token refresh.
// Without this, SSO-based accounts kept failing inside the sandbox for exactly
// the reason gcloud used to: a state directory the tool must write is read-only.
//
// The host is never modified: the bind shadows the path inside the sandbox only.
// Host-mode execution has no mount namespace, so this primitive does nothing
// there and the tool uses its real directory, which is the pre-existing
// behaviour.
type MapRWAt struct {
	// Key is the state directory (under the run's state overlay) that becomes
	// writable.
	Key string
	// At is the host location it is bound over.
	At StateSource
	// Seed copies the host content in first, so existing tokens stay visible.
	Seed *CopyIn
}

// EnvRedirect points a tool's own configuration variable at the mapped location.
// Only generic, tool-declared variable names appear here; the engine has no
// knowledge of what any of them mean.
type EnvRedirect struct {
	Name    string
	Kind    TargetKind
	RelPath string
}

// WriteBackSpec is reserved. Committing a modified SQLite credential store back
// to the operator's real files involves locking, WAL state, and conflict
// resolution that a file copy cannot provide, so P0 leaves the protocol slot
// empty and routes permanent identity changes (`gcloud auth login`,
// `docker login`) through approved host execution instead.
type WriteBackSpec struct {
	Paths       []string
	RequiresCap string
}

// ConditionalRequire pulls in another profile only when a mapped configuration
// file actually names the tool it depends on.
//
// It exists because `kubectl` is not a GCP tool. Making every kubectl and helm
// invocation depend on the gcloud profile was wrong twice over: it broke nothing
// for GKE but did nothing for EKS, AKS or a local cluster, and it copied the
// operator's Google credentials into the run overlay for commands that never
// touch Google — widening the credential surface for no benefit.
//
// The mechanism stays generic: a profile declares "if this file mentions this
// marker, that profile is needed". Any plugin-based tool can use it; the engine
// knows nothing about Kubernetes or any vendor.
type ConditionalRequire struct {
	// From is the configuration file to inspect.
	From StateSource
	// Contains is the marker to look for — normally the exec-plugin command
	// name, which is the only reliable signal of which credential helper a
	// kubeconfig will actually invoke.
	Contains string
	// Profile is the profile id to require when the marker is present.
	Profile string
	// MaxBytes bounds the inspection. A configuration file is small; refusing to
	// read an enormous one is safer than scanning it.
	MaxBytes int64
}

// EnvProfile is one tool's execution-environment contract.
type EnvProfile struct {
	ID string
	// MatchExecutables are program basenames. Globally unique across the
	// catalog, enforced by test, so matching can never be ambiguous.
	MatchExecutables []string
	// RequiresProfiles pulls in another profile's state unconditionally. Use it
	// only when the dependency is inherent to the tool, never to cover a
	// possibility — a conditional dependency belongs in ConditionalRequires.
	RequiresProfiles []string
	// ConditionalRequires pulls in another profile only when a configuration
	// file proves it is needed.
	ConditionalRequires []ConditionalRequire
	CredentialAccess    CredentialAccess
	CopyIn              []CopyIn
	MapRO               []MapRO
	MapRW               []MapRW
	MapRWAt             []MapRWAt
	EnvRedirect         []EnvRedirect
	// WriteBack stays nil in P0; the field exists so the protocol slot is
	// reserved rather than retrofitted later.
	WriteBack *WriteBackSpec
}
