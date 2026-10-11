package cli

import (
	"testing"

	"selfmind/internal/testutil/testhome"
)

// TestMain keeps this binary's tests off the developer's ~/.selfmind: config,
// model state, auth and input-history locks land in a scratch home.
func TestMain(m *testing.M) { testhome.Main(m) }
