//go:build darwin

package cliapp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLaunchdReplacementWaitsForAbsenceBeforeOneTransientRetry(t *testing.T) {
	var calls []string
	prints := []launchdJobStatus{
		{Loaded: true, State: "running"},
		{Loaded: true, State: "exited"},
		{},
		{},
	}
	bootstrapAttempts := 0
	controller := launchdServiceController{
		inspect: func(context.Context, string) (launchdJobStatus, error) {
			calls = append(calls, "print")
			if len(prints) == 0 {
				t.Fatal("unexpected launchd inspection")
			}
			status := prints[0]
			prints = prints[1:]
			return status, nil
		},
		run: func(_ context.Context, args ...string) error {
			calls = append(calls, strings.Join(args, " "))
			if len(args) > 0 && args[0] == "bootstrap" {
				bootstrapAttempts++
				if bootstrapAttempts == 1 {
					return errors.New("transient input/output error")
				}
			}
			return nil
		},
		proveAbsent: func(context.Context) error {
			calls = append(calls, "runtime absent")
			return nil
		},
		pause: func(context.Context, time.Duration) error { return nil },
	}
	if err := controller.replace(context.Background(), "gui/501", "/tmp/com.selfmind.gateway.plist"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"print",
		"bootout gui/501/com.selfmind.gateway",
		"print",
		"print",
		"runtime absent",
		"bootstrap gui/501 /tmp/com.selfmind.gateway.plist",
		"print",
		"runtime absent",
		"bootstrap gui/501 /tmp/com.selfmind.gateway.plist",
		"kickstart -k gui/501/com.selfmind.gateway",
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("launchd reconciliation calls = %#v, want %#v", calls, want)
	}
}

func TestLaunchdHealthRejectsLoadedRestartLoop(t *testing.T) {
	if (launchdJobStatus{Loaded: true, State: "spawn scheduled"}).Healthy() {
		t.Fatal("a loaded launchd restart loop was reported healthy")
	}
	if (launchdJobStatus{Loaded: true, State: "exited"}).Healthy() {
		t.Fatal("an exited launchd job was reported healthy")
	}
	if !(launchdJobStatus{Loaded: true, State: "running"}).Healthy() {
		t.Fatal("a running launchd job was not reported healthy")
	}
}

func TestLaunchdPermanentFailureDoesNotRecommendRoot(t *testing.T) {
	controller := launchdServiceController{
		inspect: func(context.Context, string) (launchdJobStatus, error) { return launchdJobStatus{}, nil },
		run: func(_ context.Context, args ...string) error {
			if len(args) > 0 && args[0] == "bootstrap" {
				return errors.New("Bootstrap failed: 5: Input/output error\nTry re-running the command as root for richer errors.")
			}
			return nil
		},
		pause: func(context.Context, time.Duration) error { return nil },
	}
	err := controller.replace(context.Background(), "gui/501", "/tmp/com.selfmind.gateway.plist")
	if err == nil {
		t.Fatal("permanent bootstrap failure unexpectedly succeeded")
	}
	if strings.Contains(strings.ToLower(err.Error()), "root") {
		t.Fatalf("personal launchd error recommended administrator access: %v", err)
	}
	if strings.Contains(err.Error(), "Bootstrap failed") || strings.Contains(err.Error(), "Input/output") {
		t.Fatalf("personal launchd error exposed raw platform output: %v", err)
	}
	if !strings.Contains(err.Error(), "one safe retry") {
		t.Fatalf("launchd error did not explain bounded retry: %v", err)
	}
}

func TestLaunchdPrintFailureDoesNotProveAbsence(t *testing.T) {
	if status, err := parseLaunchdJobStatus([]byte(`Could not find service "com.selfmind.gateway" in domain for user gui: 501`), errors.New("exit status 113")); err != nil || status.Loaded {
		t.Fatalf("missing job status=%+v err=%v", status, err)
	}
	if _, err := parseLaunchdJobStatus([]byte("operation not permitted"), errors.New("exit status 1")); err == nil {
		t.Fatal("launchd inspection failure was treated as proven absence")
	}
}

func foreignHomeWithLaunchdService(t *testing.T) (home, plist string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(serviceHomeEnv, "")
	plist = gatewayServicePlistPath(home)
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plist, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	return home, plist
}

func TestLaunchdChangesRefuseAHomeThatIsNotTheAccountsOwn(t *testing.T) {
	verbs := fakeServiceManager(t)
	home, plist := foreignHomeWithLaunchdService(t)
	assertForeignHomeCannotChangeService(t, home, plist, verbs)
}

func TestLaunchdWithoutAServiceFileIsUnaffectedByHome(t *testing.T) {
	assertHomeWithoutServiceIsUnaffected(t, fakeServiceManager(t))
}

func TestLaunchdChangesProceedFromTheHomeTheOperatorNamed(t *testing.T) {
	verbs := fakeServiceManager(t)
	home, _ := foreignHomeWithLaunchdService(t)
	t.Setenv(serviceHomeEnv, home)
	_, _, err := gatewayServiceStartIfInstalled("")
	if err == nil || strings.Contains(err.Error(), "refusing") {
		t.Fatalf("the named HOME did not reach the service manager: %v", err)
	}
	if got := verbs(); len(got) != 1 || got[0] != "kickstart" {
		t.Fatalf("service manager verbs = %v, want one kickstart", got)
	}
}
