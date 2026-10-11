package httpapi

import (
	"testing"

	"selfmind/internal/testutil/testhome"
)

// TestMain keeps this binary's tests off the developer's HOME: execution
// runtime roots and other per-user state land in a scratch home.
func TestMain(m *testing.M) { testhome.Main(m) }
