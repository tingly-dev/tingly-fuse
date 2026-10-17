// Package overlay gives every session the same view path (by default
// /workspace/<repo>) backed by its own overlayfs: lower is the shared base
// checkout, upper and work live in the session's state directory.
//
// Each command runs in a fresh mount namespace where the session's overlay
// is mounted at the view path, so sessions never see each other's writes and
// the host never sees the mount. Go cannot unshare a mount namespace from a
// running multi-threaded process, so Command re-executes the current binary
// with a sentinel argv[0]; the child is cloned into the new namespace,
// mounts the overlay, and execs the real command. As root only CLONE_NEWNS
// is used; otherwise a user namespace mapping the caller's uid and gid to
// themselves is added (needs Linux 5.11+ for unprivileged overlayfs).
//
// The backend is Linux-only; on other platforms the package registers
// nothing.
package overlay
