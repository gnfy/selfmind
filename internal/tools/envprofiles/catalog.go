package envprofiles

// Catalog is the built-in tool environment profile table.
//
// Adding a tool means adding DATA here, never a branch in engine code. Every
// entry is validated at build time by catalog_test.go: unique ids, globally
// unique executables, acyclic requires, non-empty include lists with all three
// bounds set, no traversal in relative paths, and no write_back in P0.
//
// The include lists are deliberately explicit. `~/.config/gcloud` measured
// 231 MB on a real machine, essentially all of it `logs/`; the state that
// actually matters is ~50 KB, and copying exactly that made `gcloud auth list`,
// `gcloud auth print-access-token` and GKE `kubectl get ns` all succeed on
// first use inside the sandbox while the operator's own directory stayed
// byte-identical.
var Catalog = []EnvProfile{
	{
		ID:               "gcloud",
		MatchExecutables: []string{"gcloud", "gsutil", "bq"},
		CredentialAccess: CredentialAccessOperator,
		CopyIn: []CopyIn{{
			From: StateSource{EnvVar: "CLOUDSDK_CONFIG", HomeRelPath: ".config/gcloud"},
			// The credential stores, the active configuration, and the legacy
			// per-account credential files. gcloud opens the *.db files
			// read-write even for a pure read, which is why a read-only mapping
			// is not enough.
			Include: []string{
				"*.db",
				"active_config",
				"config_sentinel",
				"configurations/**",
				"legacy_credentials/**",
			},
			// logs/ is the 231 MB; cache/ is rebuildable. Both are recreated as
			// writable directories below instead of being copied.
			Exclude:  []string{"logs/**", "cache/**"},
			MaxBytes: 5 << 20,
			MaxFiles: 100,
			MaxDepth: 5,
		}},
		MapRW: []MapRW{
			{Key: "gcloud/logs"},
			{Key: "gcloud/cache"},
		},
		EnvRedirect: []EnvRedirect{
			{Name: "CLOUDSDK_CONFIG", Kind: TargetLeaseState, RelPath: "gcloud"},
		},
	},
	{
		ID:               "kubernetes",
		MatchExecutables: []string{"kubectl"},
		CredentialAccess: CredentialAccessOperator,
		// kubectl itself needs its kubeconfig readable and its discovery cache
		// writable. WHICH credential helper it invokes is a property of the
		// kubeconfig, not of kubectl, so the helper's profile is pulled in only
		// when the kubeconfig actually names it. A GKE kubeconfig authenticates
		// through gcloud (that indirection is why GKE kubectl calls failed while
		// aws calls succeeded); an EKS one does not, and must not pay for it.
		MapRO: []MapRO{
			{From: StateSource{EnvVar: "KUBECONFIG", HomeRelPath: ".kube/config", List: true}},
		},
		MapRW: []MapRW{
			{Key: "kube-cache"},
		},
		EnvRedirect: []EnvRedirect{
			// The discovery cache is written on almost every call; leaving it
			// read-only works but emits noise and re-fetches discovery each time.
			{Name: "KUBECACHEDIR", Kind: TargetLeaseState, RelPath: "kube-cache"},
		},
		ConditionalRequires: []ConditionalRequire{
			{
				From:     StateSource{EnvVar: "KUBECONFIG", HomeRelPath: ".kube/config", List: true},
				Contains: "gke-gcloud-auth-plugin",
				Profile:  "gcloud",
				MaxBytes: 1 << 20,
			},
			{
				From:     StateSource{EnvVar: "KUBECONFIG", HomeRelPath: ".kube/config", List: true},
				Contains: "aws-iam-authenticator",
				Profile:  "aws",
				MaxBytes: 1 << 20,
			},
			{
				// The modern EKS plugin is `aws eks get-token`, but a kubeconfig
				// written by `aws eks update-kubeconfig` puts each argument on its
				// own line, so "eks get-token" never appears as one string. The
				// exec COMMAND does: match that instead.
				//
				// These markers are heuristics, and they fail in the safe
				// direction: a miss means the helper's state is simply not
				// prepared (the previous behaviour for that helper), never that
				// the wrong credentials are used.
				From:     StateSource{EnvVar: "KUBECONFIG", HomeRelPath: ".kube/config", List: true},
				Contains: "command: aws",
				Profile:  "aws",
				MaxBytes: 1 << 20,
			},
		},
	},
	{
		ID:               "helm",
		MatchExecutables: []string{"helm"},
		CredentialAccess: CredentialAccessOperator,
		// helm talks to a cluster, so it needs the kubeconfig and whichever
		// credential helper that kubeconfig names — the generic profile provides
		// both.
		RequiresProfiles: []string{"kubernetes"},
		// helm writes MORE than a discovery cache, which is why sharing the
		// kubectl profile was not enough. The original live failure was exactly
		// this: `open ~/.cache/helm/repository/argo-cd-9.2.1.tgz: read-only file
		// system` during a `helm template`.
		//
		//   HELM_CACHE_HOME  — downloaded chart archives and repository indexes.
		//                      Pure cache, expensive to refetch, no credential
		//                      meaning: person-level and persistent.
		//   HELM_CONFIG_HOME — repositories.yaml and registry/config.json (the
		//                      latter holds registry logins), rewritten by
		//                      `helm repo add|update` and `helm registry login`:
		//                      copied so the host file is never touched.
		//
		// HELM_DATA_HOME (plugins, starters) is deliberately NOT redirected:
		// redirecting it would hide the operator's installed plugins, and it is
		// only written by `helm plugin install`, a deliberate act that belongs in
		// approved host execution.
		CopyIn: []CopyIn{{
			From:     StateSource{EnvVar: "HELM_CONFIG_HOME", HomeRelPath: ".config/helm"},
			Include:  []string{"repositories.yaml", "repositories.lock", "registry/**"},
			MaxBytes: 2 << 20,
			MaxFiles: 40,
			MaxDepth: 3,
		}},
		MapRO: []MapRO{
			{From: StateSource{EnvVar: "HELM_DATA_HOME", HomeRelPath: ".local/share/helm"}},
		},
		MapRW: []MapRW{
			{Key: "helm-cache", Persistent: true},
		},
		EnvRedirect: []EnvRedirect{
			{Name: "HELM_CACHE_HOME", Kind: TargetToolchain, RelPath: "helm-cache"},
			{Name: "HELM_CONFIG_HOME", Kind: TargetLeaseState, RelPath: "helm"},
		},
	},
	{
		ID:               "aws",
		MatchExecutables: []string{"aws"},
		CredentialAccess: CredentialAccessOperator,
		// The AWS CLI reads config and static credentials without writing them,
		// which is why aws commands kept working in the sandbox while gcloud
		// failed. Those stay read-only, which is strictly safer.
		MapRO: []MapRO{
			{From: StateSource{EnvVar: "AWS_CONFIG_FILE", HomeRelPath: ".aws/config"}},
			{From: StateSource{EnvVar: "AWS_SHARED_CREDENTIALS_FILE", HomeRelPath: ".aws/credentials"}},
		},
		// The writable paths below live under `~/.aws`, and a host that has never
		// used SSO has no `sso/` directory at all — so there was no mount point
		// for them and the sandbox aborted before running anything. Declaring the
		// state root replaces it with a writable shell that keeps config and
		// credentials readable, which is what makes the nested caches mountable.
		SynthesizeDir: []SynthesizeDir{{
			At: StateSource{HomeRelPath: ".aws"},
			KeepReadOnly: []StateSource{
				{EnvVar: "AWS_CONFIG_FILE", HomeRelPath: ".aws/config"},
				{EnvVar: "AWS_SHARED_CREDENTIALS_FILE", HomeRelPath: ".aws/credentials"},
			},
		}},
		// SSO is the exception, and it is the same failure shape as gcloud: the
		// CLI WRITES its SSO token cache on every refresh, and the location is
		// derived from HOME (`~/.aws/sso/cache`) rather than from
		// AWS_CONFIG_FILE — so no environment redirect can move it. A writable
		// state directory is bound over that path instead, seeded from the host
		// so existing tokens stay usable. The host directory is never modified.
		MapRWAt: []MapRWAt{
			{
				Key: "aws/sso-cache",
				At:  StateSource{HomeRelPath: ".aws/sso/cache"},
				Seed: &CopyIn{
					From:     StateSource{HomeRelPath: ".aws/sso/cache"},
					Include:  []string{"*.json"},
					MaxBytes: 4 << 20,
					MaxFiles: 50,
					MaxDepth: 2,
				},
			},
			{
				// The CLI cache holds STS/assume-role results and is written on
				// every profile that uses them.
				Key: "aws/cli-cache",
				At:  StateSource{HomeRelPath: ".aws/cli/cache"},
				Seed: &CopyIn{
					From:     StateSource{HomeRelPath: ".aws/cli/cache"},
					Include:  []string{"*.json"},
					MaxBytes: 4 << 20,
					MaxFiles: 50,
					MaxDepth: 2,
				},
			},
		},
	},
	{
		ID:               "gh",
		MatchExecutables: []string{"gh"},
		CredentialAccess: CredentialAccessOperator,
		// GH_CONFIG_DIR is the variable gh actually honours (verified against
		// `gh help environment` and by observing where `gh config set` writes).
		// It must point at a WRITABLE copy: gh rewrites hosts.yml on token
		// refresh, so a read-only host mapping fails the same way gcloud did.
		// A permanent `gh auth login` still does not persist from inside the
		// sandbox — that needs write_back and belongs in approved host execution.
		CopyIn: []CopyIn{{
			From:     StateSource{EnvVar: "GH_CONFIG_DIR", HomeRelPath: ".config/gh"},
			Include:  []string{"config.yml", "hosts.yml"},
			Exclude:  []string{"extensions/**"},
			MaxBytes: 1 << 20,
			MaxFiles: 20,
			MaxDepth: 3,
		}},
		EnvRedirect: []EnvRedirect{
			{Name: "GH_CONFIG_DIR", Kind: TargetLeaseState, RelPath: "gh"},
		},
	},
	{
		ID:               "docker",
		MatchExecutables: []string{"docker", "podman"},
		CredentialAccess: CredentialAccessOperator,
		// The client config holds registry auth. It is copied (not mapped
		// read-only) because the CLI rewrites it on token refresh, and the copy
		// keeps the operator's file untouched. `docker login` from inside the
		// sandbox will not persist — same reason as gh.
		CopyIn: []CopyIn{{
			From:     StateSource{EnvVar: "DOCKER_CONFIG", HomeRelPath: ".docker"},
			Include:  []string{"config.json", "contexts/**"},
			Exclude:  []string{"buildx/**", "scan/**"},
			MaxBytes: 2 << 20,
			MaxFiles: 60,
			MaxDepth: 4,
		}},
		EnvRedirect: []EnvRedirect{
			{Name: "DOCKER_CONFIG", Kind: TargetLeaseState, RelPath: "docker"},
		},
	},
	{
		ID:               "go-toolchain",
		MatchExecutables: []string{"go", "gofmt"},
		CredentialAccess: CredentialAccessToolchain,
		// Person-level and persistent: a per-run build cache would turn every
		// run into a cold build. Go tolerates a read-only cache by silently
		// giving up caching, which is a slow, invisible regression rather than a
		// clean failure — so redirect rather than leave it read-only.
		MapRW: []MapRW{
			{Key: "go-build", Persistent: true},
			{Key: "go-mod", Persistent: true},
		},
		EnvRedirect: []EnvRedirect{
			{Name: "GOCACHE", Kind: TargetToolchain, RelPath: "go-build"},
			{Name: "GOMODCACHE", Kind: TargetToolchain, RelPath: "go-mod"},
		},
	},
	{
		ID:               "node-toolchain",
		MatchExecutables: []string{"npm", "pnpm", "yarn", "node"},
		CredentialAccess: CredentialAccessToolchain,
		MapRW: []MapRW{
			{Key: "npm-cache", Persistent: true},
		},
		EnvRedirect: []EnvRedirect{
			{Name: "NPM_CONFIG_CACHE", Kind: TargetToolchain, RelPath: "npm-cache"},
		},
	},
}

// byExecutable indexes the catalog for matching. Built once; the catalog is
// immutable data.
var byExecutable = func() map[string]*EnvProfile {
	out := make(map[string]*EnvProfile)
	for i := range Catalog {
		profile := &Catalog[i]
		for _, executable := range profile.MatchExecutables {
			out[executable] = profile
		}
	}
	return out
}()

// byID indexes the catalog by profile id for RequiresProfiles resolution.
var byID = func() map[string]*EnvProfile {
	out := make(map[string]*EnvProfile)
	for i := range Catalog {
		out[Catalog[i].ID] = &Catalog[i]
	}
	return out
}()
