package clone

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"

	"github.com/tingly-dev/tingly-fuse/internal/backend"
)

func init() {
	backend.Register(Backend{})
	backend.Default = "clone"
}

// cloneTree clones the whole directory hierarchy at src to dst in one call.
// NOFOLLOW clones symlinks as links; NOOWNERCOPY makes the caller own the
// result, matching what a copy would produce.
func cloneTree(src, dst string) error {
	err := unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW|unix.CLONE_NOOWNERCOPY)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EXDEV):
		return fmt.Errorf("%w: %v", errUnsupported, err)
	default:
		return fmt.Errorf("clonefile %s: %w", src, err)
	}
}
