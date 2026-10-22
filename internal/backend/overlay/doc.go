// Package overlay gives every session its own view path,
// sessions/<id>/root, backed by its own overlayfs: lower is the shared base
// checkout, upper and work live in the session's state directory.
//
// Three backends differ in where the overlay is mounted:
//
//   - overlay: kernel overlayfs on the host (root). The view is an ordinary
//     directory for everything on the machine. There is no helper process:
//     the mount outlives the CLI, and each command mounts again if it is gone.
//   - fuse-overlay: fuse-overlayfs on the host, for any user.
//   - overlay-ns: a mount namespace owned by a long-lived holder process, one
//     per session, which every command joins with nsenter; the host only sees
//     an empty mount point. Go cannot unshare a mount namespace from a
//     running multi-threaded process, so the holder is a re-execution of the
//     current binary with a sentinel argv[0]. As root only CLONE_NEWNS is
//     used; otherwise a user namespace mapping the caller's uid and gid to
//     themselves is added (needs Linux 5.11+ for unprivileged overlayfs).
//
// Sessions made by the earlier namespace-only overlay backend have a
// holder.json and keep working as overlay-ns.
//
// The backends are Linux-only; on other platforms the package registers
// nothing.
package overlay
