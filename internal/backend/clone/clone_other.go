//go:build !darwin

package clone

// cloneTree has no clonefile(2) outside darwin; Create always copies. The
// backend is not registered there, this only keeps the package buildable
// and its tests runnable.
func cloneTree(src, dst string) error { return errUnsupported }
