package eval

import (
	"testing"

	"selfmind/internal/testutil/testhome"
)

// TestMain keeps this binary's tests off the developer's per-user cache: eval
// temp roots land in a scratch home.
func TestMain(m *testing.M) { testhome.Main(m) }
