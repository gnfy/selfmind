package cliapp

import (
	"testing"

	"selfmind/internal/testutil/testhome"
)

// TestMain keeps this binary's tests off the developer's ~/.selfmind and
// caches: first-run config and eval temp roots land in a scratch home.
func TestMain(m *testing.M) { testhome.Main(m) }
