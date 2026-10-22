// Package overlay gives every session its own view path,
// sessions/<id>/root, backed by its own overlayfs: lower is the shared base
// checkout, upper and work live in the session's state directory.
//
// The overlay is mounted in a mount namespace owned by a long-lived holder
// process, one per session, and every command joins it with nsenter, so
// sessions never see each other's writes and the host only sees an empty
// mount point. Go cannot unshare a mount namespace from a running
// multi-threaded process, so the holder is a re-execution of the current
// binary with a sentinel argv[0]. As root only CLONE_NEWNS is used;
// otherwise a user namespace mapping the caller's uid and gid to themselves
// is added (needs Linux 5.11+ for unprivileged overlayfs).
//
// The backend is Linux-only; on other platforms the package registers
// nothing.
package overlay
