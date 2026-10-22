package clone

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/tingly-dev/tingly-fuse/internal/backend"
	"github.com/tingly-dev/tingly-fuse/internal/manager"
	"github.com/tingly-dev/tingly-fuse/internal/repo"
)

func TestMain(m *testing.M) {
	// Outside darwin the backend is not registered; register it so the
	// manager tests below exercise the copy fallback through the same code.
	if _, err := backend.Get("clone"); err != nil {
		backend.Register(Backend{})
	}
	os.Exit(m.Run())
}

func TestRegisteredAsDarwinDefault(t *testing.T) {
	if runtime.GOOS == "darwin" && backend.Default != "clone" {
		t.Fatalf("default backend on darwin = %q, want clone", backend.Default)
	}
	if runtime.GOOS != "darwin" && backend.Default == "clone" {
		t.Fatalf("clone must not be the default on %s", runtime.GOOS)
	}
}

func makeBase(t *testing.T) string {
	t.Helper()
	base := filepath.Join(t.TempDir(), "base")
	if err := os.MkdirAll(filepath.Join(base, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(base, "a.txt"), []byte("base\n"), 0o644)
	os.WriteFile(filepath.Join(base, "sub", "run.sh"), []byte("#!/bin/sh\n"), 0o755)
	if err := os.Symlink("a.txt", filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	return base
}

func TestCreateIsolatesFromBase(t *testing.T) {
	ctx := context.Background()
	ws := backend.Workspace{ID: "s1", BaseDir: makeBase(t), StateDir: t.TempDir()}
	var b Backend
	view, err := b.Create(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(ws.StateDir, "root"); view != want {
		t.Fatalf("view = %s, want %s", view, want)
	}

	if target, err := os.Readlink(filepath.Join(view, "link")); err != nil || target != "a.txt" {
		t.Fatalf("symlink not preserved: %q %v", target, err)
	}
	if fi, err := os.Stat(filepath.Join(view, "sub", "run.sh")); err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("exec bit not preserved: %v %v", fi, err)
	}

	os.WriteFile(filepath.Join(view, "a.txt"), []byte("changed\n"), 0o644)
	os.WriteFile(filepath.Join(view, "new.txt"), []byte("x"), 0o644)
	if got, _ := os.ReadFile(filepath.Join(ws.BaseDir, "a.txt")); string(got) != "base\n" {
		t.Fatalf("write leaked into base: %q", got)
	}
	if _, err := os.Stat(filepath.Join(ws.BaseDir, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("new file leaked into base: %v", err)
	}

	ws.ViewPath = view
	cmd, err := b.Command(ctx, ws, []string{"pwd"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	gotDir, _ := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
	wantDir, _ := filepath.EvalSymlinks(view)
	if gotDir != wantDir {
		t.Fatalf("command ran in %s, want %s", gotDir, wantDir)
	}

	if err := b.Remove(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(view); !os.IsNotExist(err) {
		t.Fatalf("view still exists after Remove: %v", err)
	}
}

func TestRemovePartial(t *testing.T) {
	ctx := context.Background()
	var b Backend
	// Nothing created at all.
	if err := b.Remove(ctx, backend.Workspace{StateDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	// A half-written root.
	state := t.TempDir()
	os.MkdirAll(filepath.Join(state, "root", "x"), 0o755)
	if err := b.Remove(ctx, backend.Workspace{StateDir: state}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(state, "root")); !os.IsNotExist(err) {
		t.Fatalf("root survived Remove: %v", err)
	}
}

func TestCommandEmpty(t *testing.T) {
	if _, err := (Backend{}).Command(context.Background(), backend.Workspace{}, nil); err == nil {
		t.Fatal("expected error for empty argv")
	}
}

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

// TestManagerRoundTrip spawns two clone sessions from one base, commits in
// each and pushes both branches.
func TestManagerRoundTrip(t *testing.T) {
	ctx := context.Background()
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "test")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "test@example.com")
	}
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	git(t, tmp, "init", "--quiet", "--initial-branch=main", src)
	os.WriteFile(filepath.Join(src, "README.md"), []byte("hello\n"), 0o644)
	git(t, src, "add", ".")
	git(t, src, "commit", "--quiet", "-m", "init")
	upstream := filepath.Join(tmp, "upstream.git")
	git(t, tmp, "clone", "--quiet", "--bare", src, upstream)

	m := manager.New(filepath.Join(tmp, "home"))
	if _, err := m.Git().Clone(ctx, upstream, "demo", repo.AddOptions{}); err != nil {
		t.Fatal(err)
	}
	run := func(id string, argv ...string) string {
		t.Helper()
		var out bytes.Buffer
		if err := m.Exec(ctx, id, argv, nil, &out, &out); err != nil {
			t.Fatalf("exec %v in %s: %v\n%s", argv, id, err, out.String())
		}
		return strings.TrimSpace(out.String())
	}

	views := map[string]bool{}
	for _, id := range []string{"a", "b"} {
		sess, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "demo", ID: id, Backend: "clone"})
		if err != nil {
			t.Fatal(err)
		}
		views[sess.ViewPath] = true
		if ws := run(id, "sh", "-c", "echo $TINGLY_WORKSPACE"); ws != sess.ViewPath {
			t.Fatalf("TINGLY_WORKSPACE = %q, want %q", ws, sess.ViewPath)
		}
		run(id, "sh", "-c", "echo "+id+" > "+id+".txt && git add . && git commit --quiet -m "+id)
		if err := m.Git().Push(ctx, id, "", &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
	}
	if len(views) != 2 {
		t.Fatalf("sessions share a view path: %v", views)
	}
	for _, id := range []string{"a", "b"} {
		files := git(t, upstream, "ls-tree", "--name-only", "agent/"+id)
		if !strings.Contains(files, id+".txt") || strings.Contains(files, map[string]string{"a": "b", "b": "a"}[id]+".txt") {
			t.Fatalf("branch agent/%s has %q", id, files)
		}
	}
	if err := m.Drop(ctx, "a"); err != nil {
		t.Fatal(err)
	}
}
