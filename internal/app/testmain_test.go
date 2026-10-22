package app

import (
	"testing"

	"selfmind/internal/testutil/testhome"
)

// TestMain keeps this binary's tests off the developer's ~/.selfmind: learning
// audit and other per-user writes land in a scratch home.
func TestMain(m *testing.M) { testhome.Main(m) }
