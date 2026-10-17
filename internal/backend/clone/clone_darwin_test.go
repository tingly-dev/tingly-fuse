package clone

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCloneTreeNative checks that the temp volume really clones instead of
// silently taking the copy fallback. It fails on a non-APFS TMPDIR.
func TestCloneTreeNative(t *testing.T) {
	base := makeBase(t)
	dst := filepath.Join(t.TempDir(), "root")
	if err := cloneTree(base, dst); err != nil {
		t.Fatalf("clonefile on %s: %v", os.TempDir(), err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "a.txt")); err != nil || string(got) != "base\n" {
		t.Fatalf("cloned content = %q, %v", got, err)
	}
}
