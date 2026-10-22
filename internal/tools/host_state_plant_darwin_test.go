//go:build darwin

package tools

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// hostStatePlantVerb makes this test binary a planting helper when a sandboxed
// command runs it: the shell has no RENAME_SWAP or clonefile, so the helper
// performs exactly the one operation the review probe used.
const hostStatePlantVerb = "selfmind-plant-host-state"

// runHostStatePlantHelper runs the helper and exits when this binary was
// invoked as one; otherwise it returns and the tests run normally.
func runHostStatePlantHelper() {
	if len(os.Args) != 5 || os.Args[1] != hostStatePlantVerb {
		return
	}
	var err error
	switch os.Args[2] {
	case "swap":
		err = unix.RenamexNp(os.Args[3], os.Args[4], unix.RENAME_SWAP)
	case "detach-swap":
		// The review probe: leave the command's process group for a session
		// of its own, outlive the command, then plant.
		if _, err = unix.Setsid(); err == nil {
			time.Sleep(1500 * time.Millisecond)
			err = unix.RenamexNp(os.Args[3], os.Args[4], unix.RENAME_SWAP)
		}
	case "clone":
		err = unix.Clonefile(os.Args[3], os.Args[4], unix.CLONE_NOFOLLOW)
	default:
		err = fmt.Errorf("unknown operation %q", os.Args[2])
	}
	if err != nil {
		fmt.Printf("plant %s refused: %v\n", os.Args[2], err)
		os.Exit(3)
	}
	fmt.Printf("planted by %s\n", os.Args[2])
	os.Exit(0)
}
