package overlay_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tingly-dev/tingly-fuse/internal/backend"
	"github.com/tingly-dev/tingly-fuse/internal/backend/overlay"
	"github.com/tingly-dev/tingly-fuse/internal/manager"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// setup returns a manager with an upstream bare repo registered as "demo"
// and views mounted under a temp dir. It skips when this host cannot mount
// overlayfs in a new mount namespace.
func setup(t *testing.T) (*manager.Manager, string) {
	t.Helper()
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
	tmp := t.TempDir()
	t.Setenv(overlay.EnvViewRoot, filepath.Join(tmp, "workspace"))
	probe(t, tmp)

	src := filepath.Join(tmp, "src")
	git(t, tmp, "init", "--quiet", "--initial-branch=main", src)
	os.WriteFile(filepath.Join(src, "README.md"), []byte("hello\n"), 0o644)
	git(t, src, "add", ".")
	git(t, src, "commit", "--quiet", "-m", "init")
	upstream := filepath.Join(tmp, "upstream.git")
	git(t, tmp, "clone", "--quiet", "--bare", src, upstream)

	m := manager.New(filepath.Join(tmp, "home"))
	if _, err := m.AddRepo(context.Background(), "demo", upstream); err != nil {
		t.Fatal(err)
	}
	return m, upstream
}

func probe(t *testing.T, tmp string) {
	t.Helper()
	ws := backend.Workspace{ID: "probe", Repo: "probe", BaseDir: filepath.Join(tmp, "probe-base"), StateDir: filepath.Join(tmp, "probe-state")}
	os.MkdirAll(ws.BaseDir, 0o755)
	b := overlay.Backend{}
	var err error
	if ws.ViewPath, err = b.Create(context.Background(), ws); err != nil {
		t.Skipf("overlay backend unavailable: %v", err)
	}
	defer b.Remove(context.Background(), ws)
	cmd, _ := b.Command(context.Background(), ws, []string{"true"})
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("overlay backend unavailable: %v: %s", err, out)
	}
}

func execIn(t *testing.T, m *manager.Manager, id string, argv ...string) string {
	t.Helper()
	var out bytes.Buffer
	if err := m.Exec(context.Background(), id, argv, nil, &out, &out); err != nil {
		t.Fatalf("exec %v in %s: %v\n%s", argv, id, err, out.String())
	}
	return strings.TrimSpace(out.String())
}

func TestDefaultOnLinux(t *testing.T) {
	if backend.Default != "overlay" {
		t.Errorf("default backend = %q", backend.Default)
	}
}

func TestSamePathIsolatedViews(t *testing.T) {
	m, upstream := setup(t)
	ctx := context.Background()

	a, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "demo", ID: "a", Backend: "overlay"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "demo", ID: "b", Backend: "overlay"})
	if err != nil {
		t.Fatal(err)
	}
	if a.ViewPath != b.ViewPath || filepath.Base(a.ViewPath) != "demo" {
		t.Fatalf("want one shared view path ending in demo, got %q and %q", a.ViewPath, b.ViewPath)
	}

	execIn(t, m, "a", "sh", "-c", "echo from-a >> README.md && git commit --quiet -am a")
	execIn(t, m, "b", "sh", "-c", "echo from-b > NOTES.md && rm README.md && git add -A && git commit --quiet -m b")

	if got := execIn(t, m, "a", "pwd"); got != a.ViewPath {
		t.Errorf("cwd = %q, want %q", got, a.ViewPath)
	}
	if got := execIn(t, m, "a", "sh", "-c", `echo "$PWD $TINGLY_WORKSPACE"`); got != a.ViewPath+" "+a.ViewPath {
		t.Errorf("PWD/TINGLY_WORKSPACE = %q", got)
	}
	if got := execIn(t, m, "a", "cat", "README.md"); got != "hello\nfrom-a" {
		t.Errorf("a README.md = %q", got)
	}
	if got := execIn(t, m, "a", "ls"); got != "README.md" {
		t.Errorf("a sees b's files: %q", got)
	}
	if got := execIn(t, m, "b", "ls"); got != "NOTES.md" {
		t.Errorf("b ls = %q", got)
	}
	if got := execIn(t, m, "b", "git", "branch", "--show-current"); got != "agent/b" {
		t.Errorf("b branch = %q", got)
	}

	// Exit status of the inner command propagates.
	err = m.Exec(ctx, "a", []string{"sh", "-c", "exit 7"}, nil, nil, nil)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Errorf("exit status: %v", err)
	}

	// The mount is invisible from the host.
	if entries, _ := os.ReadDir(a.ViewPath); len(entries) != 0 {
		t.Errorf("host sees files at the view path: %v", entries)
	}

	for _, id := range []string{"a", "b"} {
		var out bytes.Buffer
		if err := m.Push(ctx, id, "", &out, &out); err != nil {
			t.Fatalf("push %s: %v\n%s", id, err, out.String())
		}
	}
	branches := git(t, upstream, "branch", "--format=%(refname:short)")
	for _, want := range []string{"agent/a", "agent/b", "main"} {
		if !strings.Contains(branches, want) {
			t.Errorf("upstream branches %q missing %q", branches, want)
		}
	}

	// Writes land in the session's upper dir, never in the base.
	base := filepath.Join(m.Home, "repos", "demo", "bases", a.Base)
	if data, _ := os.ReadFile(filepath.Join(base, "README.md")); string(data) != "hello\n" {
		t.Errorf("base README.md modified: %q", data)
	}
	if _, err := os.Stat(filepath.Join(base, "NOTES.md")); !os.IsNotExist(err) {
		t.Errorf("base got b's file: %v", err)
	}
	upperA := filepath.Join(m.Home, "sessions", "a", "upper")
	if data, _ := os.ReadFile(filepath.Join(upperA, "README.md")); string(data) != "hello\nfrom-a\n" {
		t.Errorf("a upper README.md = %q", data)
	}
	if packs, _ := filepath.Glob(filepath.Join(upperA, ".git", "objects", "pack", "*")); len(packs) != 0 {
		t.Errorf("pack files copied up into a's upper: %v", packs)
	}

	if err := m.Drop(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(m.Home, "sessions", "a")); !os.IsNotExist(err) {
		t.Errorf("session state left after drop: %v", err)
	}
	if got := execIn(t, m, "b", "cat", "NOTES.md"); got != "from-b" {
		t.Errorf("b after dropping a: %q", got)
	}
}

func TestRemovePartial(t *testing.T) {
	tmp := t.TempDir()
	ws := backend.Workspace{ID: "x", Repo: "x", StateDir: filepath.Join(tmp, "state")}
	if err := (overlay.Backend{}).Remove(context.Background(), ws); err != nil {
		t.Fatalf("remove of never-created workspace: %v", err)
	}
	os.MkdirAll(filepath.Join(ws.StateDir, "work", "work"), 0o755)
	os.Chmod(filepath.Join(ws.StateDir, "work", "work"), 0)
	if err := (overlay.Backend{}).Remove(context.Background(), ws); err != nil {
		t.Fatalf("remove: %v", err)
	}
}
