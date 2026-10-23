package overlay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/tingly-dev/tingly-fuse/internal/backend"
)

// unprivileged reports whether the process lacks root, so kernel overlayfs
// cannot be mounted on the host.
func unprivileged() bool { return os.Geteuid() != 0 }

func register() {
	backend.Register(Backend{})
	backend.Register(FuseBackend{})
	backend.Register(NSBackend{})
	if unprivileged() {
		backend.Default = "fuse-overlay"
	} else {
		backend.Default = "overlay"
	}
}

// Backend mounts kernel overlayfs at the view in the host's mount
// namespace, so the tree is visible to everything on the machine: shells,
// editors, IDEs. It needs root. Sessions created by the earlier versions of
// this backend, which mounted inside a namespace, keep working that way.
type Backend struct{}

func (Backend) Name() string { return "overlay" }

func (Backend) Create(ctx context.Context, ws backend.Workspace) (string, error) {
	if legacy(ws) {
		return NSBackend{}.Create(ctx, ws)
	}
	if unprivileged() {
		return "", fmt.Errorf("the overlay backend mounts on the host and needs root; use --backend fuse-overlay for a host-visible view without root, or overlay-ns for a view only commands run by tingly-fuse can see")
	}
	return hostCreate(ws, kernelMount)
}

func (Backend) Command(ctx context.Context, ws backend.Workspace, argv []string) (*exec.Cmd, error) {
	if legacy(ws) {
		return NSBackend{}.Command(ctx, ws, argv)
	}
	return hostCommand(ctx, ws, argv, kernelMount)
}

func (Backend) Mount(ctx context.Context, ws backend.Workspace) error {
	if legacy(ws) {
		return NSBackend{}.Mount(ctx, ws)
	}
	return ensureMounted(ws, kernelMount)
}

func (Backend) Remove(ctx context.Context, ws backend.Workspace) error {
	if legacy(ws) {
		return NSBackend{}.Remove(ctx, ws)
	}
	return hostRemove(ws)
}

// legacy reports whether the session was made when this backend mounted in
// a namespace of its own, which left a holder file behind.
func legacy(ws backend.Workspace) bool {
	_, err := os.Stat(holderFile(ws))
	return err == nil
}

// FuseBackend mounts fuse-overlayfs at the view on the host. The view is
// visible to everything running as the same user, without root; reads and
// writes go through a FUSE daemon, which costs some speed. Shared paths are
// symlinks, as with the copy backend, since bind mounts need root.
type FuseBackend struct{}

func (FuseBackend) Name() string { return "fuse-overlay" }

func (FuseBackend) Create(ctx context.Context, ws backend.Workspace) (string, error) {
	view, err := hostCreate(ws, fuseMount)
	if err != nil {
		return "", err
	}
	if err := backend.LinkShares(view, ws.Shares); err != nil {
		_ = hostRemove(ws)
		return "", err
	}
	return view, nil
}

func (FuseBackend) Command(ctx context.Context, ws backend.Workspace, argv []string) (*exec.Cmd, error) {
	return hostCommand(ctx, ws, argv, fuseMount)
}

func (FuseBackend) Mount(ctx context.Context, ws backend.Workspace) error {
	return ensureMounted(ws, fuseMount)
}

func (FuseBackend) Remove(ctx context.Context, ws backend.Workspace) error {
	return hostRemove(ws)
}

// mounter puts the session's overlay, and any bind mounted shares, at the
// view. It runs with the session lock held and only when nothing is mounted
// there.
type mounter func(ws backend.Workspace, view string) error

func hostCreate(ws backend.Workspace, mount mounter) (string, error) {
	view := viewDir(ws)
	for _, d := range []string{upperDir(ws), workDir(ws), view} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", err
		}
	}
	if err := ensureMounted(ws, mount); err != nil {
		return "", err
	}
	return view, nil
}

// hostCommand returns a command run directly in the view. The mount outlives
// the tingly-fuse process, so there is no holder; but it does not outlive
// a reboot or an unmount, so each command first checks it and mounts again
// if it is gone.
func hostCommand(ctx context.Context, ws backend.Workspace, argv []string, mount mounter) (*exec.Cmd, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty command")
	}
	if err := ensureMounted(ws, mount); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = ws.ViewPath
	cmd.Env = []string{"PWD=" + ws.ViewPath}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PWD=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	return cmd, nil
}

// ensureMounted mounts the overlay at the view unless a working one is
// already there. Mounting the same upper dir twice is undefined behavior in
// the kernel, hence the lock and the check; a mount left behind by a dead
// FUSE daemon is detached first.
func ensureMounted(ws backend.Workspace, mount mounter) error {
	unlock, err := lock(ws)
	if err != nil {
		return err
	}
	defer unlock()

	view := viewDir(ws)
	switch mounts := mountsAt(view); {
	case len(mounts) == 0:
	case healthy(view):
		return nil
	default:
		if err := unmountAll(view); err != nil {
			return fmt.Errorf("view %s is mounted but unusable and cannot be unmounted: %w", view, err)
		}
	}
	if err := mount(ws, view); err != nil {
		_ = unmountAll(view)
		return err
	}
	return nil
}

func healthy(view string) bool {
	_, err := os.Stat(view)
	return err == nil
}

func kernelMount(ws backend.Workspace, view string) error {
	opts := "lowerdir=" + escape(ws.BaseDir) + ",upperdir=" + escape(upperDir(ws)) + ",workdir=" + escape(workDir(ws))
	if !inInitUserNS() {
		opts += ",userxattr"
	}
	if err := syscall.Mount("overlay", view, "overlay", 0, opts); err != nil {
		return fmt.Errorf("mount overlay at %s: %w", view, err)
	}
	for _, sh := range ws.Shares {
		if err := bindShare(view, sh.Path, sh.Source); err != nil {
			return fmt.Errorf("share %s: %w", sh.Path, err)
		}
	}
	return nil
}

func fuseMount(ws backend.Workspace, view string) error {
	bin, err := exec.LookPath("fuse-overlayfs")
	if err != nil {
		return fmt.Errorf("the fuse-overlay backend needs fuse-overlayfs: %w", err)
	}
	opts := "lowerdir=" + escape(ws.BaseDir) + ",upperdir=" + escape(upperDir(ws)) + ",workdir=" + escape(workDir(ws))
	// It forks into the background once mounted, so Run returns when the
	// mount is ready or has failed.
	out, err := exec.Command(bin, "-o", opts, view).CombinedOutput()
	if err != nil {
		return fmt.Errorf("fuse-overlayfs at %s: %w: %s", view, err, bytes.TrimSpace(out))
	}
	return nil
}

// hostRemove unmounts the view and deletes the session's state. It stops
// before deleting anything if the view cannot be unmounted, because the
// shared paths are mounted into it and deleting through them would reach
// the shared directories.
func hostRemove(ws backend.Workspace) error {
	if _, err := os.Stat(ws.StateDir); err != nil {
		return nil
	}
	if err := unmountAll(viewDir(ws)); err != nil {
		return fmt.Errorf("unmount %s: %w", viewDir(ws), err)
	}
	var errs []error
	for _, d := range []string{upperDir(ws), workDir(ws)} {
		// overlayfs leaves work/work with mode 000; make everything
		// writable so RemoveAll can descend.
		_ = filepath.WalkDir(d, func(p string, e fs.DirEntry, err error) error {
			if err == nil && e.IsDir() {
				_ = os.Chmod(p, 0o700)
			}
			return nil
		})
		errs = append(errs, os.RemoveAll(d))
	}
	errs = append(errs, os.Remove(viewDir(ws)), os.Remove(lockFile(ws)))
	return ignoreNotExist(errors.Join(errs...))
}

// mountEntry is one line of /proc/self/mountinfo.
type mountEntry struct {
	point, fstype string
}

// mountsAt returns the mounts at dir and below it, deepest first.
func mountsAt(dir string) []mountEntry {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	var out []mountEntry
	for _, line := range strings.Split(string(data), "\n") {
		before, after, ok := strings.Cut(line, " - ")
		if !ok {
			continue
		}
		f := strings.Fields(before)
		g := strings.Fields(after)
		if len(f) < 5 || len(g) < 1 {
			continue
		}
		point := unescapeMountinfo(f[4])
		if point == dir || strings.HasPrefix(point, dir+"/") {
			out = append(out, mountEntry{point, g[0]})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i].point) > len(out[j].point) })
	return out
}

// unescapeMountinfo undoes the octal escapes (\040 for a space) the kernel
// puts in mountinfo paths.
func unescapeMountinfo(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// unmountAll unmounts everything at and below dir. A mount that is in use
// is an error, not lazily detached, so processes never lose their view.
func unmountAll(dir string) error {
	var errs []error
	for _, m := range mountsAt(dir) {
		var err error
		if strings.HasPrefix(m.fstype, "fuse") {
			err = fuseUnmount(m.point)
		} else {
			err = syscall.Unmount(m.point, 0)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", m.point, err))
		}
	}
	return errors.Join(errs...)
}

func fuseUnmount(point string) error {
	for _, name := range []string{"fusermount3", "fusermount"} {
		bin, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		// -z is a lazy detach: needed to clear a mount whose daemon died,
		// but it would also hide a busy one, so try the plain unmount first.
		out, err := exec.Command(bin, "-u", point).CombinedOutput()
		if err == nil {
			return nil
		}
		if _, serr := os.Stat(point); errors.Is(serr, syscall.ENOTCONN) {
			if out2, err2 := exec.Command(bin, "-uz", point).CombinedOutput(); err2 == nil {
				return nil
			} else {
				out = out2
			}
		}
		return fmt.Errorf("%s -u: %w: %s", name, err, bytes.TrimSpace(out))
	}
	return fmt.Errorf("fusermount not found")
}
