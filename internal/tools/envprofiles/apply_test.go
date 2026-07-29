package envprofiles

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func applyContext(t *testing.T, home string, trust string, credentialRead bool) ApplyContext {
	t.Helper()
	base := t.TempDir()
	return ApplyContext{
		Home:              home,
		StateRoot:         filepath.Join(base, "state"),
		ScratchTmp:        filepath.Join(base, "tmp"),
		ToolchainRoot:     filepath.Join(base, "toolchain"),
		Lookup:            func(string) (string, bool) { return "", false },
		Trust:             trust,
		HasCredentialRead: credentialRead,
	}
}

// The include list is the whole point: a state directory measured 231 MB, almost
// all of it logs, so "copy the directory" is not an option and a silent partial
// copy would look like a broken login.
func TestCopyInIsBoundedAndSelective(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(home, ".config", "gcloud")
	if err := os.MkdirAll(filepath.Join(state, "logs", "day"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "credentials.db"), []byte("cred"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "logs", "day", "big.log"), make([]byte, 8<<20), 0o600); err != nil {
		t.Fatal(err)
	}

	profile, _ := ByID("gcloud")
	ctx := applyContext(t, home, TrustTrusted, false)
	result, err := Apply([]*EnvProfile{profile}, ctx)
	if err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(ctx.StateRoot, "gcloud")
	if _, err := os.Stat(filepath.Join(overlay, "credentials.db")); err != nil {
		t.Fatalf("included state must be copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(overlay, "logs", "day", "big.log")); err == nil {
		t.Fatal("excluded logs must not be copied, and must not count against the size bound")
	}
	// Permission bits survive: a copied credential file must not become 0644.
	info, err := os.Stat(filepath.Join(overlay, "credentials.db"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("copied credential file has mode %v, want 0600", info.Mode().Perm())
	}
	if result.CopiedFiles == 0 {
		t.Fatal("copy result must report what it moved")
	}
	if !strings.Contains(strings.Join(result.EnvOverrides, " "), "CLOUDSDK_CONFIG="+overlay) {
		t.Fatalf("redirect missing: %v", result.EnvOverrides)
	}
}

func TestCopyInRefusesToExceedItsBounds(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(home, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "big.db"), make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	profile := &EnvProfile{
		ID:               "bounded",
		MatchExecutables: []string{"bounded"},
		CredentialAccess: CredentialAccessOperator,
		CopyIn: []CopyIn{{
			From:     StateSource{HomeRelPath: "state"},
			Include:  []string{"*.db"},
			MaxBytes: 1024, MaxFiles: 10, MaxDepth: 3,
		}},
	}
	if _, err := Apply([]*EnvProfile{profile}, applyContext(t, home, TrustTrusted, false)); err == nil {
		t.Fatal("exceeding the byte bound must be a hard failure, not a truncated copy")
	}
}

// A symlink could point outside the source tree, and a device or socket has no
// meaning in a copied credential store. Both are refused rather than followed.
func TestCopyInRefusesNonRegularFiles(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(home, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.db")
	if err := os.WriteFile(outside, []byte("escaped"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(state, "link.db")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	profile := &EnvProfile{
		ID:               "symlinked",
		MatchExecutables: []string{"symlinked"},
		CredentialAccess: CredentialAccessOperator,
		CopyIn: []CopyIn{{
			From:     StateSource{HomeRelPath: "state"},
			Include:  []string{"*.db"},
			MaxBytes: 1 << 20, MaxFiles: 10, MaxDepth: 3,
		}},
	}
	if _, err := Apply([]*EnvProfile{profile}, applyContext(t, home, TrustTrusted, false)); err == nil {
		t.Fatal("a symlink in the copy set must be refused")
	}
}

func TestTrustDecidesOperatorCredentialAccess(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(home, ".config", "gcloud")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "credentials.db"), []byte("cred"), 0o600); err != nil {
		t.Fatal(err)
	}
	profile, _ := ByID("gcloud")

	withheld := applyContext(t, home, TrustUntrusted, false)
	result, err := Apply([]*EnvProfile{profile}, withheld)
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(filepath.Join(withheld.StateRoot, "gcloud", "credentials.db")); statErr == nil {
		t.Fatal("an untrusted workspace must not receive operator credentials")
	}
	if len(result.Notes) == 0 {
		t.Fatal("withholding must be explained, not silent")
	}
	// The redirect still applies, so the tool reports its own "not logged in"
	// state instead of a read-only filesystem error.
	if !strings.Contains(strings.Join(result.EnvOverrides, " "), "CLOUDSDK_CONFIG=") {
		t.Fatalf("redirect must still apply: %v", result.EnvOverrides)
	}

	granted := applyContext(t, home, TrustUntrusted, true)
	if _, err := Apply([]*EnvProfile{profile}, granted); err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(filepath.Join(granted.StateRoot, "gcloud", "credentials.db")); statErr != nil {
		t.Fatalf("credential:read must materialize the overlay: %v", statErr)
	}
}

// Toolchain caches carry no credential meaning; withholding them protects
// nothing and makes every run a cold build.
func TestToolchainProfileIsGrantedRegardlessOfTrust(t *testing.T) {
	profile, _ := ByID("go-toolchain")
	ctx := applyContext(t, t.TempDir(), TrustUntrusted, false)
	result, err := Apply([]*EnvProfile{profile}, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Notes) != 0 {
		t.Fatalf("a toolchain cache is not withheld: %v", result.Notes)
	}
	env := strings.Join(result.EnvOverrides, " ")
	// Persistent means person-level: a per-run build cache is always cold.
	if !strings.Contains(env, "GOCACHE="+filepath.Join(ctx.ToolchainRoot, "go-build")) {
		t.Fatalf("GOCACHE must point at the persistent toolchain root: %v", result.EnvOverrides)
	}
}

func TestMatchIsPureOverProgramNames(t *testing.T) {
	profiles := Match([]string{"kubectl"})
	if len(profiles) != 1 || profiles[0].ID != "kubernetes" {
		t.Fatalf("kubectl alone must match only the generic kubernetes profile, got %v", profileIDs(profiles))
	}
	if Match([]string{"totally-unknown-tool"}) != nil {
		t.Fatal("unknown programs must match nothing")
	}
}

// kubectl is not a GCP tool. Which credential helper it invokes is a property of
// the KUBECONFIG, so the helper's profile must be pulled in only when the
// kubeconfig actually names it — an EKS or local-cluster kubectl must never drag
// in the operator's Google credentials.
func TestConditionalRequiresFollowTheKubeconfigExecPlugin(t *testing.T) {
	cases := []struct {
		name       string
		kubeconfig string
		want       []string
	}{
		{
			name:       "gke exec plugin pulls in gcloud",
			kubeconfig: "users:\n- user:\n    exec:\n      command: gke-gcloud-auth-plugin\n",
			want:       []string{"gcloud", "kubernetes"},
		},
		{
			name: "eks exec plugin pulls in aws",
			// Shaped like a real `aws eks update-kubeconfig` output: one argument
			// per line, so a multi-word marker would never match.
			kubeconfig: "users:\n- user:\n    exec:\n      command: aws\n      args:\n      - --region\n      - us-east-1\n      - eks\n      - get-token\n",
			want:       []string{"aws", "kubernetes"},
		},
		{
			name:       "legacy eks authenticator pulls in aws",
			kubeconfig: "users:\n- user:\n    exec:\n      command: aws-iam-authenticator\n",
			want:       []string{"aws", "kubernetes"},
		},
		{
			name:       "a plain kubeconfig pulls in nothing extra",
			kubeconfig: "users:\n- user:\n    client-certificate-data: abc\n",
			want:       []string{"kubernetes"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			kubeDir := filepath.Join(home, ".kube")
			if err := os.MkdirAll(kubeDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(kubeDir, "config"), []byte(tc.kubeconfig), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx := applyContext(t, home, TrustTrusted, false)
			result, err := Apply(Match([]string{"kubectl"}), ctx)
			if err != nil {
				t.Fatal(err)
			}
			got := append([]string{}, result.Applied...)
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("applied %v, want %v", got, tc.want)
			}
			// Google credentials must not be materialized for a non-GKE cluster.
			_, statErr := os.Stat(filepath.Join(ctx.StateRoot, "gcloud"))
			wantsGcloud := strings.Contains(strings.Join(tc.want, ","), "gcloud")
			if wantsGcloud && statErr != nil {
				t.Fatalf("GKE kubeconfig must prepare the gcloud overlay: %v", statErr)
			}
			if !wantsGcloud && statErr == nil {
				t.Fatal("a non-GKE kubeconfig must not prepare a gcloud overlay")
			}
		})
	}
}

// A dependency must still be applied before its dependent: the dependent's
// redirects may point into directories the dependency creates.
func TestConditionalDependencyIsAppliedFirst(t *testing.T) {
	home := t.TempDir()
	kubeDir := filepath.Join(home, ".kube")
	if err := os.MkdirAll(kubeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kubeDir, "config"),
		[]byte("exec:\n  command: gke-gcloud-auth-plugin\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Apply(Match([]string{"kubectl"}), applyContext(t, home, TrustTrusted, false))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Applied) != 2 || result.Applied[0] != "gcloud" {
		t.Fatalf("gcloud must be applied before kubernetes, got %v", result.Applied)
	}
}

// gh honours GH_CONFIG_DIR (verified against `gh help environment` and by
// observing where `gh config set` writes). Redirecting anything else leaves gh
// writing the read-only host config, which fails exactly like gcloud did.
func TestGhProfileRedirectsTheVariableGhActuallyReads(t *testing.T) {
	home := t.TempDir()
	ghDir := filepath.Join(home, ".config", "gh")
	if err := os.MkdirAll(ghDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ghDir, "hosts.yml"), []byte("github.com:\n  oauth_token: t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	profile, _ := ByID("gh")
	ctx := applyContext(t, home, TrustTrusted, false)
	result, err := Apply([]*EnvProfile{profile}, ctx)
	if err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(ctx.StateRoot, "gh")
	if !strings.Contains(strings.Join(result.EnvOverrides, " "), "GH_CONFIG_DIR="+overlay) {
		t.Fatalf("gh must get GH_CONFIG_DIR pointed at a writable copy: %v", result.EnvOverrides)
	}
	if strings.Contains(strings.Join(result.EnvOverrides, " "), "GH_STATE_DIR") {
		t.Fatal("GH_STATE_DIR is not a variable gh reads; redirecting it does nothing")
	}
	// The copy must be writable, which is the whole point: gh rewrites hosts.yml
	// on token refresh.
	if err := os.WriteFile(filepath.Join(overlay, "hosts.yml"), []byte("rewritten"), 0o600); err != nil {
		t.Fatalf("the gh overlay must be writable: %v", err)
	}
}

func profileIDs(profiles []*EnvProfile) []string {
	ids := make([]string, 0, len(profiles))
	for _, profile := range profiles {
		ids = append(ids, profile.ID)
	}
	return ids
}

// Two profiles pointing one variable at different targets is a hard failure:
// silently picking one would give the command an environment neither describes.
func TestConflictingRedirectsFail(t *testing.T) {
	first := &EnvProfile{
		ID: "first", MatchExecutables: []string{"first"}, CredentialAccess: CredentialAccessNone,
		EnvRedirect: []EnvRedirect{{Name: "SHARED_CONFIG", Kind: TargetLeaseState, RelPath: "a"}},
	}
	second := &EnvProfile{
		ID: "second", MatchExecutables: []string{"second"}, CredentialAccess: CredentialAccessNone,
		EnvRedirect: []EnvRedirect{{Name: "SHARED_CONFIG", Kind: TargetLeaseState, RelPath: "b"}},
	}
	_, err := Apply([]*EnvProfile{first, second}, applyContext(t, t.TempDir(), TrustTrusted, false))
	if err == nil || !strings.Contains(err.Error(), "SHARED_CONFIG") {
		t.Fatalf("conflicting redirects must fail and name the variable, got %v", err)
	}
}

// An env var wins over the home-relative fallback: an operator who moved their
// config must not have the old location copied.
func TestStateSourcePrefersEnvironmentVariable(t *testing.T) {
	home := t.TempDir()
	moved := filepath.Join(t.TempDir(), "moved-gcloud")
	if err := os.MkdirAll(moved, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(moved, "credentials.db"), []byte("moved"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := applyContext(t, home, TrustTrusted, false)
	ctx.Lookup = func(name string) (string, bool) {
		if name == "CLOUDSDK_CONFIG" {
			return moved, true
		}
		return "", false
	}
	profile, _ := ByID("gcloud")
	if _, err := Apply([]*EnvProfile{profile}, ctx); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(ctx.StateRoot, "gcloud", "credentials.db"))
	if err != nil || string(data) != "moved" {
		t.Fatalf("the env-var location must win: %v %q", err, data)
	}
}

// helm writes more than a discovery cache. The original live failure was
// `open ~/.cache/helm/repository/argo-cd-9.2.1.tgz: read-only file system`
// during a plain `helm template`, so sharing the kubectl profile was not enough.
func TestHelmGetsItsOwnWritableState(t *testing.T) {
	home := t.TempDir()
	for _, dir := range []string{".config/helm/registry", ".local/share/helm/plugins", ".kube"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, ".config", "helm", "repositories.yaml"),
		[]byte("repositories:\n- name: argo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".kube", "config"),
		[]byte("users:\n- user:\n    client-certificate-data: abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	profiles := Match([]string{"helm"})
	ids := profileIDs(profiles)
	if len(ids) != 2 || ids[0] != "kubernetes" || ids[1] != "helm" {
		t.Fatalf("helm must build on the generic kubernetes profile, got %v", ids)
	}
	ctx := applyContext(t, home, TrustTrusted, false)
	result, err := Apply(profiles, ctx)
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Join(result.EnvOverrides, " ")

	// The cache is person-level and persistent: chart archives and repo indexes
	// are expensive to refetch and carry no credential meaning.
	wantCache := "HELM_CACHE_HOME=" + filepath.Join(ctx.ToolchainRoot, "helm-cache")
	if !strings.Contains(env, wantCache) {
		t.Fatalf("missing %q in %q", wantCache, env)
	}
	// The config directory is COPIED so `helm repo add` and `helm registry login`
	// can write without touching the operator's file.
	overlay := filepath.Join(ctx.StateRoot, "helm")
	if !strings.Contains(env, "HELM_CONFIG_HOME="+overlay) {
		t.Fatalf("HELM_CONFIG_HOME must point at the writable copy: %q", env)
	}
	data, readErr := os.ReadFile(filepath.Join(overlay, "repositories.yaml"))
	if readErr != nil || !strings.Contains(string(data), "argo") {
		t.Fatalf("repository configuration must be copied in: %v %q", readErr, data)
	}
	if err := os.WriteFile(filepath.Join(overlay, "repositories.yaml"), []byte("rewritten"), 0o600); err != nil {
		t.Fatalf("the helm config overlay must be writable: %v", err)
	}
	// The kubeconfig still comes from the generic profile.
	if !strings.Contains(env, "KUBECACHEDIR=") {
		t.Fatalf("helm must inherit the Kubernetes discovery cache redirect: %q", env)
	}
	// HELM_DATA_HOME is deliberately NOT redirected: doing so would hide the
	// operator's installed plugins.
	if strings.Contains(env, "HELM_DATA_HOME=") {
		t.Fatalf("HELM_DATA_HOME must not be redirected: %q", env)
	}
}
