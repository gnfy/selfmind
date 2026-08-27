package cliapp

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSystemdReplacementStopsAndProvesAbsenceBeforeOneTransientRetry(t *testing.T) {
	var calls []string
	statuses := []systemdUnitStatus{
		{LoadState: "loaded", ActiveState: "active"},
		{LoadState: "loaded", ActiveState: "deactivating"},
		{LoadState: "loaded", ActiveState: "inactive"},
		{LoadState: "loaded", ActiveState: "failed"},
	}
	startAttempts := 0
	controller := systemdServiceController{
		inspect: func(context.Context, string) (systemdUnitStatus, error) {
			calls = append(calls, "inspect")
			if len(statuses) == 0 {
				t.Fatal("unexpected systemd inspection")
			}
			status := statuses[0]
			statuses = statuses[1:]
			return status, nil
		},
		run: func(_ context.Context, args ...string) error {
			calls = append(calls, strings.Join(args, " "))
			if len(args) > 0 && args[0] == "enable" {
				startAttempts++
				if startAttempts == 1 {
					return errors.New("transient failure")
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
	if err := controller.replace(context.Background(), gatewaySystemdUnit); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"inspect", "stop selfmind-gateway.service", "inspect", "inspect", "runtime absent",
		"daemon-reload", "enable --now selfmind-gateway.service", "inspect", "runtime absent",
		"enable --now selfmind-gateway.service",
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("systemd reconciliation calls = %#v, want %#v", calls, want)
	}
}

func TestSystemdReplacementDoesNotRetryWhileUnitIsActive(t *testing.T) {
	startAttempts := 0
	statuses := []systemdUnitStatus{
		{LoadState: "loaded", ActiveState: "inactive"},
		{LoadState: "loaded", ActiveState: "inactive"},
		{LoadState: "loaded", ActiveState: "active"},
	}
	controller := systemdServiceController{
		inspect: func(context.Context, string) (systemdUnitStatus, error) {
			status := statuses[0]
			statuses = statuses[1:]
			return status, nil
		},
		run: func(_ context.Context, args ...string) error {
			if len(args) > 0 && args[0] == "enable" {
				startAttempts++
				return errors.New("command lost its reply")
			}
			return nil
		},
		pause: func(context.Context, time.Duration) error { return nil },
	}
	if err := controller.replace(context.Background(), gatewaySystemdUnit); err == nil {
		t.Fatal("active unit after a failed command was treated as safe to retry")
	}
	if startAttempts != 1 {
		t.Fatalf("start attempts = %d, want 1", startAttempts)
	}
}
