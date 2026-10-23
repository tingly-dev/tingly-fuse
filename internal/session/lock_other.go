//go:build !unix

package session

// lock is a no-op where flock(2) does not exist; concurrent label updates
// of one session are then not serialized.
func lock(path string) (func(), error) { return func() {}, nil }
