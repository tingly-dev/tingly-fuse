package tinglyfuse_test

// Drives the whole lifecycle through the public package only, the way a
// program embedding tingly-fuse would, on every backend this host can run.

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tinglyfuse "github.com/tingly-dev/tingly-fuse"
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

func TestAPI(t *testing.T) {
	for _, b := range tinglyfuse.Backends() {
		t.Run(b, func(t *testing.T) {
			if err := tinglyfuse.CheckBackend(context.Background(), b); err != nil {
				t.Skipf("backend %s unavailable: %v", b, err)
			}
			testAPI(t, b)
		})
	}
}

func testAPI(t *testing.T, backend string) {
	for _, kv := range []string{"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com"} {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	tmp := t.TempDir()
	t.Setenv("TINGLY_FUSE_HOME", filepath.Join(tmp, "home"))
	src := filepath.Join(tmp, "src")
	git(t, tmp, "init", "--quiet", "--initial-branch=main", src)
	os.WriteFile(filepath.Join(src, "README.md"), []byte("hello\n"), 0o644)
	git(t, src, "add", ".")
	git(t, src, "commit", "--quiet", "-m", "init")
	upstream := filepath.Join(tmp, "upstream.git")
	git(t, tmp, "clone", "--quiet", "--bare", src, upstream)

	ctx := context.Background()
	m, err := tinglyfuse.Open("")
	if err != nil {
		t.Fatal(err)
	}
	if m.Home != filepath.Join(tmp, "home") {
		t.Errorf("Open(\"\") home = %s", m.Home)
	}
	t.Cleanup(func() {
		sessions, _ := m.Sessions()
		for _, s := range sessions {
			m.Drop(ctx, s.ID)
		}
	})
	r, err := m.Git().Clone(ctx, upstream, "demo", tinglyfuse.AddOptions{SharedGit: true, SharedPaths: []string{"cache", "out"}})
	if err != nil {
		t.Fatal(err)
	}
	if !r.SharedGit || strings.Join(r.SharedPaths, ",") != "cache,out" {
		t.Errorf("repo = %+v", r)
	}
	if _, err := m.SetShares("demo", []string{"logs"}, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Shares("demo"); strings.Join(got, ",") != "cache,logs,out" {
		t.Errorf("Shares = %v", got)
	}

	var sessions []*tinglyfuse.Session
	for _, id := range []string{"a", "b"} {
		s, err := m.Git().AddWorktree(ctx, tinglyfuse.WorktreeOptions{Repo: "demo", ID: id, Backend: backend, Share: []string{"tmp"}})
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, s)
	}
	run := func(id string, argv ...string) string {
		t.Helper()
		var out bytes.Buffer
		if err := m.Exec(ctx, id, argv, nil, &out, &out); err != nil {
			t.Fatalf("exec %v in %s: %v\n%s", argv, id, err, out.String())
		}
		return strings.TrimSpace(out.String())
	}
	run("a", "sh", "-c", "echo c > cache/c && echo t > tmp/t && git commit --quiet --allow-empty -m from-a")
	if got := run("b", "sh", "-c", "cat cache/c tmp/t; git log -1 --format=%s agent/a; echo $"+tinglyfuse.EnvSession); got != "c\nt\nfrom-a\nb" {
		t.Errorf("b sees %q", got)
	}
	if err := m.Exec(ctx, "a", []string{"git", "push", "--quiet", "origin", "HEAD"}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := git(t, upstream, "log", "-1", "--format=%s", "agent/a"); got != "from-a" {
		t.Errorf("upstream agent/a = %q", got)
	}
	bases, err := m.Bases(ctx, "demo")
	if err != nil || len(bases) != 1 || len(bases[0].Sessions) != 2 {
		t.Errorf("Bases = %+v, %v", bases, err)
	}
	health, err := m.Git().Fsck(ctx, "demo", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range health {
		if h.Err != nil || !h.Pinned {
			t.Errorf("base %+v unhealthy", h)
		}
	}
	for _, s := range sessions {
		if err := m.Drop(ctx, s.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.RemoveRepo(ctx, "demo", false); err != nil {
		t.Fatal(err)
	}

	// The generic layer on a plain directory.
	data := filepath.Join(tmp, "data")
	if err := os.MkdirAll(filepath.Join(data, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(data, "f"), []byte("v1\n"), 0o644)
	d, base, err := m.AddDir(ctx, "data", data, tinglyfuse.DirOptions{SharedPaths: []string{"cache"}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != tinglyfuse.KindDir || d.Current != base {
		t.Errorf("dir repo = %+v", d)
	}
	for _, id := range []string{"p", "q"} {
		if _, err := m.Spawn(ctx, tinglyfuse.SpawnOptions{Repo: "data", ID: id, Backend: backend}); err != nil {
			t.Fatal(err)
		}
	}
	run("p", "sh", "-c", "echo c > cache/c && echo mine > f")
	if got := run("q", "cat", "cache/c", "f"); got != "c\nv1" {
		t.Errorf("q sees %q", got)
	}
	if err := m.RemoveRepo(ctx, "data", true); err != nil {
		t.Fatal(err)
	}
}
