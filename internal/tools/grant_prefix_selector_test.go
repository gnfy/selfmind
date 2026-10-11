package tools

import "testing"

// A CLI may let the last target selector win. Sorting repeated selectors into
// a remembered class loses that ordering, including when an alias repeats it.
func TestRepeatedBoundSelectorsRequireOneTimeApproval(t *testing.T) {
	cases := []struct{ name, first, reversed string }{
		{"same selector", "kubectl delete configmap app --context=staging --context=prod", "kubectl delete configmap app --context=prod --context=staging"},
		{"selector aliases", "docker container create alpine --context=staging -c prod", "docker container create alpine -c prod --context=staging"},
		{"attached alias", "docker container create alpine --context=staging -cprod", "docker container create alpine -cprod --context=staging"},
		{"equals alias", "docker container create alpine --context=staging -c=prod", "docker container create alpine -c=prod --context=staging"},
		{"same value", "kubectl delete configmap app --context=prod --context prod", "kubectl delete configmap app --context prod --context=prod"},
		{"cluster selector", "kubectl delete configmap app --cluster=staging --cluster prod", "kubectl delete configmap app --cluster prod --cluster=staging"},
		{"connection aliases", "podman container create alpine --connection=staging -c prod", "podman container create alpine -c prod --connection=staging"},
		{"doctl aliases", "doctl compute droplet delete app --context=staging -c prod", "doctl compute droplet delete app -c prod --context=staging"},
		{"scope aliases", "vercel deploy --scope=staging -S prod", "vercel deploy -S prod --scope=staging"},
		{"profile aliases", "aliyun ecs DeleteInstance --profile=staging -p prod", "aliyun ecs DeleteInstance -p prod --profile=staging"},
		{"abbreviated profile", "aws codebuild start-build --profile=staging --prof prod", "aws codebuild start-build --prof prod --profile=staging"},
		{"abbreviated account", "gcloud builds submit --account=stage@example.com --accou prod@example.com", "gcloud builds submit --accou prod@example.com --account=stage@example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			asks := &classAsks{}
			person, _ := classKeyScope(t, ApprovalOnRequest, asks)
			if ran, err := runClassKeyCall(person, tc.first); !ran || err != nil || len(asks.requests) != 1 {
				t.Fatalf("first call must ask and run: ran=%v err=%v asks=%d", ran, err, len(asks.requests))
			}
			if got := asks.requests[0].GrantClass; got != "" {
				t.Errorf("repeated selectors offered a remember option %q", got)
			}
			if ran, err := runClassKeyCall(person, tc.reversed); !ran || err != nil || len(asks.requests) != 2 {
				t.Fatalf("changed final selector must ask again: ran=%v err=%v asks=%d", ran, err, len(asks.requests))
			}
			if got := asks.requests[1].GrantClass; got != "" {
				t.Errorf("reversed selectors offered a remember option %q", got)
			}
		})
	}
}

func TestIndependentBoundSelectorsKeepReusableApproval(t *testing.T) {
	cases := []struct{ name, first, next string }{
		{"single context", "kubectl delete configmap app --context=prod", "kubectl delete configmap other --context prod"},
		{"single long alias", "docker container create alpine --context=prod", "docker container create busybox --context prod"},
		{"single short alias", "docker container create alpine -c prod", "docker container create busybox -cprod"},
		{"single abbreviated profile", "aws codebuild start-build --prof prod --project-name app", "aws codebuild start-build --profile=prod --project-name other"},
		{"distinct selectors", "kubectl delete configmap app --context=prod --user=ops", "kubectl delete configmap other --user ops --context prod"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			asks := &classAsks{}
			person, _ := classKeyScope(t, ApprovalOnRequest, asks)
			if ran, err := runClassKeyCall(person, tc.first); !ran || err != nil || len(asks.requests) != 1 {
				t.Fatalf("first call must ask and run: ran=%v err=%v asks=%d", ran, err, len(asks.requests))
			}
			if got := asks.requests[0].GrantClass; got == "" {
				t.Fatal("independent selectors must offer a remember option")
			}
			if ran, err := runClassKeyCall(person, tc.next); !ran || err != nil || len(asks.requests) != 1 {
				t.Fatalf("same bound class must reuse approval: ran=%v err=%v asks=%d", ran, err, len(asks.requests))
			}
		})
	}
}
