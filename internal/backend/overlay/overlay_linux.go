package overlay

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/tingly-dev/tingly-fuse/internal/backend"
)

// EnvViewRoot overrides the directory views are mounted under. Views are
// mounted at <root>/<repo>.
const EnvViewRoot = "TINGLY_FUSE_VIEW_ROOT"

// DefaultViewRoot is used when EnvViewRoot is unset.
const DefaultViewRoot = "/workspace"

// enterArg0 marks a re-executed child that must mount the overlay and exec
// the real command.
const enterArg0 = "tingly-fuse-overlay-enter"

const (
	capDACOverride       = 1
	capSysAdmin          = 21
	prCapAmbient         = 47
	prCapAmbientClearAll = 4
)

func init() {
	if len(os.Args) > 0 && os.Args[0] == enterArg0 {
		enter(os.Args[1:]) // never returns
	}
	backend.Register(Backend{})
	backend.Default = "overlay"
}

type Backend struct{}

func (Backend) Name() string { return "overlay" }

func upperDir(ws backend.Workspace) string { return filepath.Join(ws.StateDir, "upper") }
func workDir(ws backend.Workspace) string  { return filepath.Join(ws.StateDir, "work") }

func (Backend) Create(ctx context.Context, ws backend.Workspace) (string, error) {
	root := os.Getenv(EnvViewRoot)
	if root == "" {
		root = DefaultViewRoot
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	view := filepath.Join(root, ws.Repo)
	for _, d := range []string{upperDir(ws), workDir(ws)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", err
		}
	}
	// The mount point must exist on the host; it stays an empty directory
	// shared by every session of the repo. An unprivileged user cannot
	// create it from inside the namespace either, so fail early with a hint.
	if err := os.MkdirAll(view, 0o755); err != nil {
		return "", fmt.Errorf("create mount point: %w (create %s yourself or set %s)", err, root, EnvViewRoot)
	}
	return view, nil
}

func (Backend) Command(ctx context.Context, ws backend.Workspace, argv []string) (*exec.Cmd, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty command")
	}
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate own executable: %w", err)
	}
	args := append([]string{ws.BaseDir, upperDir(ws), workDir(ws), ws.ViewPath, "--"}, argv...)
	cmd := exec.CommandContext(ctx, self, args...)
	cmd.Args[0] = enterArg0
	// cmd.Dir stays unset: the child changes into the view after mounting,
	// otherwise its cwd would be the host's empty mount point.
	attr := &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNS}
	if os.Geteuid() != 0 {
		attr.Cloneflags |= syscall.CLONE_NEWUSER
		attr.UidMappings = []syscall.SysProcIDMap{{ContainerID: os.Getuid(), HostID: os.Getuid(), Size: 1}}
		attr.GidMappings = []syscall.SysProcIDMap{{ContainerID: os.Getgid(), HostID: os.Getgid(), Size: 1}}
		attr.GidMappingsEnableSetgroups = false
		// execve as a non-zero uid drops the capabilities the new user
		// namespace grants; keep the ones mounting needs as ambient (overlayfs
		// also needs DAC override to use its 000-mode work/work dir). enter
		// clears them again before running the real command.
		attr.AmbientCaps = []uintptr{capSysAdmin, capDACOverride}
	}
	cmd.SysProcAttr = attr
	return cmd, nil
}

func (Backend) Remove(ctx context.Context, ws backend.Workspace) error {
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
	return errors.Join(errs...)
}

// enter runs in the re-executed child, already inside its own mount
// namespace: args are lower, upper, work, view, "--", argv...
func enter(args []string) {
	fail := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "tingly-fuse overlay: "+format+"\n", a...)
		os.Exit(127)
	}
	if len(args) < 6 || args[4] != "--" {
		fail("bad arguments %q", args)
	}
	lower, upper, work, view, argv := args[0], args[1], args[2], args[3], args[5:]
	// Capabilities are per thread: clear and exec on the same one.
	runtime.LockOSThread()

	// Keep our mount out of the parent namespace.
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		fail("make mounts private: %v", err)
	}
	opts := "lowerdir=" + escape(lower) + ",upperdir=" + escape(upper) + ",workdir=" + escape(work)
	if os.Geteuid() != 0 || !inInitUserNS() {
		// Unprivileged overlayfs cannot write trusted.* xattrs.
		opts += ",userxattr"
	}
	if err := syscall.Mount("overlay", view, "overlay", 0, opts); err != nil {
		fail("mount overlay at %s: %v", view, err)
	}
	if err := os.Chdir(view); err != nil {
		fail("%v", err)
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		fail("%v", err)
	}
	// PWD is inherited from the caller; point it at the view so shells and
	// tools that trust it agree with getcwd.
	env := []string{"PWD=" + view}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PWD=") {
			env = append(env, kv)
		}
	}
	// Drop the ambient capabilities so the command runs unprivileged.
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prCapAmbient, prCapAmbientClearAll, 0); e != 0 {
		fail("clear ambient capabilities: %v", e)
	}
	if err := syscall.Exec(path, argv, env); err != nil {
		fail("exec %s: %v", argv[0], err)
	}
}

// inInitUserNS reports whether we run in the initial user namespace, where
// uid 0 really is root.
func inInitUserNS() bool {
	data, err := os.ReadFile("/proc/self/uid_map")
	if err != nil {
		return true
	}
	f := strings.Fields(string(data))
	return len(f) == 3 && f[0] == "0" && f[1] == "0" && f[2] == "4294967295"
}

// escape quotes characters overlayfs treats as separators in mount options.
func escape(p string) string {
	r := strings.NewReplacer(`\`, `\\`, `,`, `\,`, `:`, `\:`)
	return r.Replace(p)
}
