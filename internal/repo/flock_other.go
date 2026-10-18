//go:build !unix

package repo

// flock is a no-op where flock(2) does not exist: tingly-fuse still builds
// there (the copy backend works anywhere), but concurrent spawns, fetches
// and base removal are not serialized.
func flock(path string, exclusive bool) (func(), error) { return func() {}, nil }
