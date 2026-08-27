package cliapp

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const gatewaySystemdUnit = "selfmind-gateway.service"

type systemdUnitStatus struct {
	LoadState   string
	ActiveState string
}

func (s systemdUnitStatus) stopped() bool {
	load := strings.ToLower(strings.TrimSpace(s.LoadState))
	active := strings.ToLower(strings.TrimSpace(s.ActiveState))
	return load == "not-found" || active == "inactive" || active == "failed"
}

type systemdServiceController struct {
	inspect     func(context.Context, string) (systemdUnitStatus, error)
	run         func(context.Context, ...string) error
	proveAbsent func(context.Context) error
	pause       func(context.Context, time.Duration) error
}

func (c systemdServiceController) replace(ctx context.Context, unit string) error {
	status, err := c.inspect(ctx, unit)
	if err != nil {
		return err
	}
	if !status.stopped() {
		if err := c.run(ctx, "stop", unit); err != nil {
			latest, inspectErr := c.inspect(ctx, unit)
			if inspectErr != nil || !latest.stopped() {
				return fmt.Errorf("stop existing personal systemd service: %w", err)
			}
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, err = c.inspect(ctx, unit)
		if err != nil {
			return err
		}
		if status.stopped() {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("existing personal systemd service did not finish stopping; retry after current SelfMind work finishes")
		}
		if err := c.pause(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
	if c.proveAbsent != nil {
		if err := c.proveAbsent(ctx); err != nil {
			return err
		}
	}
	if err := c.run(ctx, "daemon-reload"); err != nil {
		return err
	}
	start := []string{"enable", "--now", unit}
	if err := c.run(ctx, start...); err != nil {
		status, inspectErr := c.inspect(ctx, unit)
		if inspectErr != nil {
			return inspectErr
		}
		if !status.stopped() {
			return fmt.Errorf("register personal systemd service: %w", err)
		}
		if c.proveAbsent != nil {
			if absentErr := c.proveAbsent(ctx); absentErr != nil {
				return absentErr
			}
		}
		if retryErr := c.run(ctx, start...); retryErr != nil {
			return fmt.Errorf("register personal systemd service after one safe retry: %w", retryErr)
		}
	}
	return nil
}
