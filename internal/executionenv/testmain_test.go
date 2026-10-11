package executionenv

import (
	"testing"

	"selfmind/internal/testutil/testhome"
)

// TestMain keeps this binary's tests off the developer's HOME: runtime roots
// that must sit outside the system temp tree land in a scratch home there.
func TestMain(m *testing.M) { testhome.Main(m) }
