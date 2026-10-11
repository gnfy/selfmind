package tools

import (
	"testing"

	"selfmind/internal/testutil/testhome"
)

// TestMain keeps this binary's tests off the developer's HOME: the code
// sandbox, learning audit and test runtime roots land in a scratch home. Run
// inside a sandbox as a planting helper, the binary does only that and exits.
func TestMain(m *testing.M) {
	runHostStatePlantHelper()
	testhome.Main(m)
}
