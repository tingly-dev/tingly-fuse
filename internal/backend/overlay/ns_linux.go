package overlay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tingly-dev/tingly-fuse/internal/backend"
)

// holderArg0 marks a re-executed child that must mount the overlay in its
// fresh mount namespace and then stay alive to keep that namespace.
const holderArg0 = "tingly-fuse-overlay-holder"

// chdirArg1 marks a re-executed child that nsenter started inside the
// session's namespace: it changes into the view and execs the command.
// (nsenter --wd resolves its path before entering, i.e. on the host.)
// nsenter cannot set argv[0], so the marker is the first argument.
const chdirArg1 = "tingly-fuse-overlay-chdir"

const (
	capDACOverride       = 1
	capSysAdmin          = 21
	prCapAmbient         = 47
	prCapAmbientClearAll = 4
)

func init() {
	if len(os.Args) > 0 && os.Args[0] == holderArg0 {
		hold(os.Args[1:]) // never returns
	}
	if len(os.Args) > 1 && os.Args[1] == chdirArg1 {
		chdirExec(os.Args[2:]) // never returns
	}
	register()
}

// NSBackend mounts the overlay inside the session's own mount namespace
// (and a user namespace when unprivileged), where only processes started
// through Command see it. The host sees an empty directory at the view.
type NSBackend struct{}

func (NSBackend) Name() string { return "overlay-ns" }

func viewDir(ws backend.Workspace) string    { return filepath.Join(ws.StateDir, "root") }
func upperDir(ws backend.Workspace) string   { return filepath.Join(ws.StateDir, "upper") }
func workDir(ws backend.Workspace) string    { return filepath.Join(ws.StateDir, "work") }
func holderFile(ws backend.Workspace) string { return filepath.Join(ws.StateDir, "holder.json") }
func lockFile(ws backend.Workspace) string   { return filepath.Join(ws.StateDir, "holder.lock") }

func (NSBackend) Create(ctx context.Context, ws backend.Workspace) (string, error) {
	// The view is a different path per session, like the other backends
	// (ADR 0014). The mount point stays an empty directory on the host.
	view := viewDir(ws)
	for _, d := range []string{upperDir(ws), workDir(ws), view} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", err
		}
	}
	return view, nil
}

// Command joins the session's mount namespace, starting its holder first
// if needed, so every command of a session shares one overlay mount.
func (NSBackend) Command(ctx context.Context, ws backend.Workspace, argv []string) (*exec.Cmd, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty command")
	}
	nsenter, err := exec.LookPath("nsenter")
	if err != nil {
		return nil, fmt.Errorf("overlay backend needs nsenter (util-linux): %w", err)
	}
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate own executable: %w", err)
	}
	pid, err := ensureHolder(ws)
	if err != nil {
		return nil, err
	}
	args := []string{"--target", strconv.Itoa(pid), "--mount"}
	if unprivileged() {
		args = append(args, "--user", "--preserve-credentials")
	}
	args = append(args, "--", self, chdirArg1, ws.ViewPath)
	cmd := exec.CommandContext(ctx, nsenter, append(args, argv...)...)
	// PWD is inherited from the caller; point it at the view so shells and
	// tools that trust it agree with getcwd.
	cmd.Env = []string{"PWD=" + ws.ViewPath}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PWD=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	return cmd, nil
}

func (NSBackend) Remove(ctx context.Context, ws backend.Workspace) error {
	var errs []error
	if err := stopHolder(ws); err != nil {
		errs = append(errs, err)
	}
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
	errs = append(errs, os.Remove(viewDir(ws)), os.Remove(holderFile(ws)), os.Remove(lockFile(ws)))
	return ignoreNotExist(errors.Join(errs...))
}

func ignoreNotExist(err error) error {
	if err == nil {
		return nil
	}
	var kept []error
	for _, e := range unwrapJoin(err) {
		if !errors.Is(e, fs.ErrNotExist) {
			kept = append(kept, e)
		}
	}
	return errors.Join(kept...)
}

func unwrapJoin(err error) []error {
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		return j.Unwrap()
	}
	return []error{err}
}

// holderState is persisted in the session directory. NS identifies the
// mount namespace, so a reused pid is never mistaken for the holder.
type holderState struct {
	PID int    `json:"pid"`
	NS  string `json:"ns"`
}

// lock serializes holder management for one session across processes.
func lock(ws backend.Workspace) (unlock func(), err error) {
	f, err := os.OpenFile(lockFile(ws), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

func readState(ws backend.Workspace) (holderState, error) {
	var st holderState
	data, err := os.ReadFile(holderFile(ws))
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(data, &st)
}

func mountNS(pid int) string {
	ns, _ := os.Readlink("/proc/" + strconv.Itoa(pid) + "/ns/mnt")
	return ns
}

// ensureHolder returns a pid inside the session's mount namespace. If the
// holder died while commands it served are still running, their namespace
// (and its overlay) is still alive, so one of them is joined instead of
// mounting the same upper dir a second time. Only when the namespace is
// gone is a new holder started.
func ensureHolder(ws backend.Workspace) (int, error) {
	unlock, err := lock(ws)
	if err != nil {
		return 0, err
	}
	defer unlock()

	st, err := readState(ws)
	if err == nil && st.NS != "" {
		if mountNS(st.PID) == st.NS {
			return st.PID, nil
		}
		if pid := findInNS(st.NS); pid != 0 {
			return pid, nil
		}
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("read holder state: %w", err)
	}

	pid, err := startHolder(ws)
	if err != nil {
		return 0, err
	}
	st = holderState{PID: pid, NS: mountNS(pid)}
	if st.NS == "" {
		return 0, fmt.Errorf("holder %d exited right after mounting", pid)
	}
	data, _ := json.Marshal(st)
	tmp := holderFile(ws) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return 0, err
	}
	return pid, os.Rename(tmp, holderFile(ws))
}

// findInNS returns some live process in mount namespace ns, or 0.
func findInNS(ns string) int {
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err == nil && mountNS(pid) == ns {
			return pid
		}
	}
	return 0
}

// startHolder re-executes this binary into a new mount namespace (and a
// self-mapped user namespace when unprivileged), where it mounts the
// overlay and then waits. It reports readiness or a mount error over a
// pipe, and is detached so it outlives the CLI invocation that started it.
func startHolder(ws backend.Workspace) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("locate own executable: %w", err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	defer r.Close()
	args := []string{ws.BaseDir, upperDir(ws), workDir(ws), ws.ViewPath}
	for _, sh := range ws.Shares {
		args = append(args, sh.Path, sh.Source)
	}
	cmd := exec.Command(self, args...)
	cmd.Args[0] = holderArg0
	cmd.Dir = "/"
	cmd.ExtraFiles = []*os.File{w} // fd 3
	attr := &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNS, Setsid: true}
	if unprivileged() {
		attr.Cloneflags |= syscall.CLONE_NEWUSER
		attr.UidMappings = []syscall.SysProcIDMap{{ContainerID: os.Getuid(), HostID: os.Getuid(), Size: 1}}
		attr.GidMappings = []syscall.SysProcIDMap{{ContainerID: os.Getgid(), HostID: os.Getgid(), Size: 1}}
		attr.GidMappingsEnableSetgroups = false
		// execve as a non-zero uid drops the capabilities the new user
		// namespace grants; keep the ones mounting needs as ambient (overlayfs
		// also needs DAC override to use its 000-mode work/work dir).
		attr.AmbientCaps = []uintptr{capSysAdmin, capDACOverride}
	}
	cmd.SysProcAttr = attr
	err = cmd.Start()
	w.Close()
	if err != nil {
		return 0, fmt.Errorf("start overlay holder: %w", err)
	}
	// Reap it if it exits while we are still running; once we exit it is
	// reparented and keeps going.
	go cmd.Wait()
	msg, _ := io.ReadAll(r)
	if string(msg) != "ok" {
		_ = cmd.Process.Kill()
		if len(msg) == 0 {
			msg = []byte("holder exited without reporting")
		}
		return 0, fmt.Errorf("overlay holder: %s", bytes.TrimSpace(msg))
	}
	return cmd.Process.Pid, nil
}

// stopHolder terminates the session's holder, if it is still the one
// recorded. Commands still running in the namespace keep it (and the
// mount) alive until they exit.
func stopHolder(ws backend.Workspace) error {
	if _, err := os.Stat(ws.StateDir); err != nil {
		return nil
	}
	unlock, err := lock(ws)
	if err != nil {
		return err
	}
	defer unlock()
	st, err := readState(ws)
	if err != nil || mountNS(st.PID) != st.NS {
		return nil
	}
	if err := syscall.Kill(st.PID, syscall.SIGTERM); err != nil {
		return nil
	}
	for i := 0; i < 100 && mountNS(st.PID) == st.NS; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

// hold runs in the re-executed holder, already inside its own mount
// namespace: args are lower, upper, work, view, then a shared path and its
// source for each share. fd 3 is the status pipe.
func hold(args []string) {
	status := os.NewFile(3, "status")
	fail := func(format string, a ...any) {
		fmt.Fprintf(status, format, a...)
		os.Exit(1)
	}
	if len(args) < 4 || len(args)%2 != 0 {
		fail("bad arguments %q", args)
	}
	lower, upper, work, view := args[0], args[1], args[2], args[3]
	// Capabilities are per thread: mount and clear them on the same one.
	runtime.LockOSThread()

	// Keep our mount out of the parent namespace.
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		fail("make mounts private: %v", err)
	}
	opts := "lowerdir=" + escape(lower) + ",upperdir=" + escape(upper) + ",workdir=" + escape(work)
	if unprivileged() || !inInitUserNS() {
		// Unprivileged overlayfs cannot write trusted.* xattrs.
		opts += ",userxattr"
	}
	if err := syscall.Mount("overlay", view, "overlay", 0, opts); err != nil {
		fail("mount overlay at %s: %v", view, err)
	}
	// Bind mounts keep shared paths at their place in the view, and unlike
	// symlinks they cannot be replaced by a rename: that fails with EBUSY.
	for i := 4; i < len(args); i += 2 {
		if err := bindShare(view, args[i], args[i+1]); err != nil {
			fail("share %s: %v", args[i], err)
		}
	}
	_, _, _ = syscall.RawSyscall(syscall.SYS_PRCTL, prCapAmbient, prCapAmbientClearAll, 0)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	signal.Ignore(syscall.SIGHUP)
	fmt.Fprint(status, "ok")
	status.Close()
	<-sig
	os.Exit(0)
}

// bindShare bind mounts src over rel in view, first creating rel (in the
// upper layer) as a directory or empty file like src if the base lacks it.
func bindShare(view, rel, src string) error {
	sfi, err := os.Stat(src)
	if err != nil {
		return err
	}
	dst, err := backend.InView(view, rel)
	if err != nil {
		return err
	}
	dfi, err := os.Lstat(dst)
	switch {
	case os.IsNotExist(err):
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if sfi.IsDir() {
			err = os.Mkdir(dst, 0o755)
		} else {
			err = os.WriteFile(dst, nil, 0o644)
		}
		if err != nil {
			return err
		}
	case err != nil:
		return err
	case dfi.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("it is a symlink in the base")
	case dfi.IsDir() != sfi.IsDir():
		return fmt.Errorf("the base and the shared copy disagree on whether it is a directory")
	}
	return syscall.Mount(src, dst, "", syscall.MS_BIND|syscall.MS_REC, "")
}

// chdirExec runs inside the session's namespace: args are view, argv...
func chdirExec(args []string) {
	fail := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "tingly-fuse overlay: "+format+"\n", a...)
		os.Exit(127)
	}
	if len(args) < 2 {
		fail("bad arguments %q", args)
	}
	if err := os.Chdir(args[0]); err != nil {
		fail("%v", err)
	}
	path, err := exec.LookPath(args[1])
	if err != nil {
		fail("%v", err)
	}
	if err := syscall.Exec(path, args[1:], os.Environ()); err != nil {
		fail("exec %s: %v", args[1], err)
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
