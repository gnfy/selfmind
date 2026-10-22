//go:build !darwin && !linux

package peercred

import (
	"context"
	"time"
)

func socketPeer(int) (Credentials, error) { return Credentials{}, ErrUnsupported }

func lookupProcess(int) (process, error) { return process{}, ErrUnsupported }

// EnableChildSubreaper is unavailable on this platform.
func EnableChildSubreaper() (bool, error) { return false, nil }

// ReapAdoptedOrphans has nothing to reap without a subreaper.
func ReapAdoptedOrphans(context.Context, time.Duration, *GroupTracker) {}
