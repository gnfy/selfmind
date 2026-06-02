package cliapp

import "testing"

func TestGatewayURLPrefersGatewayEnv(t *testing.T) {
	t.Setenv("SELF_GATEWAY_URL", "http://127.0.0.1:9001/")
	t.Setenv("SELF_DAEMON_URL", "http://127.0.0.1:9002/")
	t.Setenv("SELF_GATEWAY_ADDR", "127.0.0.1:9003")
	t.Setenv("SELF_DAEMON_ADDR", "127.0.0.1:9004")

	app := &App{}
	if got, want := app.gatewayURL(), "http://127.0.0.1:9001"; got != want {
		t.Fatalf("gatewayURL() = %q, want %q", got, want)
	}
}

func TestGatewayURLFallsBackToDaemonEnv(t *testing.T) {
	t.Setenv("SELF_GATEWAY_URL", "")
	t.Setenv("SELF_DAEMON_URL", "")
	t.Setenv("SELF_GATEWAY_ADDR", "")
	t.Setenv("SELF_DAEMON_ADDR", "127.0.0.1:9004")

	app := &App{}
	if got, want := app.gatewayURL(), "http://127.0.0.1:9004"; got != want {
		t.Fatalf("gatewayURL() = %q, want %q", got, want)
	}
}

func TestPlatformUserIDPrefersExplicitEnv(t *testing.T) {
	t.Setenv("SELF_PLATFORM_USER_ID", "person-local")
	t.Setenv("USERNAME", "windows-user")
	t.Setenv("USER", "unix-user")

	if got, want := platformUserID(), "person-local"; got != want {
		t.Fatalf("platformUserID() = %q, want %q", got, want)
	}
}
