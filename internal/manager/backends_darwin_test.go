package manager_test

import (
	"testing"

	"github.com/tingly-dev/tingly-fuse/internal/backend"
	_ "github.com/tingly-dev/tingly-fuse/internal/backend/clone"
)

// Importing clone makes it the default; the other tests in this package
// are written against copy.
func init() { backend.Default = "copy" }

func platformBackends() []string { return []string{"copy", "clone"} }

func usable(*testing.T, string) error { return nil }
