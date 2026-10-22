package manager_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tingly-dev/tingly-fuse/internal/backend"
	"github.com/tingly-dev/tingly-fuse/internal/manager"
	"github.com/tingly-dev/tingly-fuse/internal/repo"
)

func TestSharedPaths(t *testing.T) { eachBackend(t, testSharedPaths) }

// A shared directory is one directory for every session spawned with it:
// writes, deletes and new files show up in the others at once, and dropping
// a session leaves it alone.
func testSharedPaths(t *testing.T) {
	m, _ := setup(t)
	ctx := context.Background()
	if _, err := m.SetShares("demo", []string{"cache", "build/out"}, false); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if _, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "demo", ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	execIn(t, m, "a", "sh", "-c", "echo hi > cache/x && echo o > build/out/o")
	if got := execIn(t, m, "b", "cat", "cache/x", "build/out/o"); got != "hi\no" {
		t.Errorf("b reads %q", got)
	}
	// Shared content never gets committed by accident.
	if got := execIn(t, m, "b", "sh", "-c", "git add -A && git status --porcelain"); got != "" {
		t.Errorf("shared paths are not excluded from git: %q", got)
	}
	execIn(t, m, "b", "sh", "-c", "rm cache/x && mkdir cache/d && echo y > cache/d/y")
	if got := execIn(t, m, "a", "sh", "-c", "ls cache; cat cache/d/y"); got != "d\ny" {
		t.Errorf("a sees %q", got)
	}
	// The rest of the tree stays private.
	execIn(t, m, "a", "sh", "-c", "echo private > README.md")
	if got := execIn(t, m, "b", "cat", "README.md"); got != "hello" {
		t.Errorf("b's README.md = %q", got)
	}
	if err := m.Drop(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if backend.Default == "overlay" {
		// A holder started again after a crash binds the shares again.
		var st struct{ PID int }
		data, _ := os.ReadFile(filepath.Join(m.Home, "sessions", "b", "holder.json"))
		if err := json.Unmarshal(data, &st); err != nil || st.PID == 0 {
			t.Fatalf("holder.json: %v %s", err, data)
		}
		p, _ := os.FindProcess(st.PID)
		p.Kill()
		for i := 0; i < 100 && p.Signal(syscall.Signal(0)) == nil; i++ {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if got := execIn(t, m, "b", "cat", "cache/d/y"); got != "y" {
		t.Errorf("after drop, b reads %q", got)
	}

	// Shares are fixed at spawn: unsharing affects new sessions only.
	if _, err := m.SetShares("demo", []string{"cache"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "demo", ID: "c"}); err != nil {
		t.Fatal(err)
	}
	execIn(t, m, "c", "sh", "-c", "mkdir -p cache && echo c > cache/c")
	if got := execIn(t, m, "b", "ls", "cache"); got != "d" {
		t.Errorf("c's private cache leaked into b: %q", got)
	}
	if got := execIn(t, m, "c", "cat", "build/out/o"); got != "o" {
		t.Errorf("c's build/out = %q", got)
	}
	fsckView(t, m, "b")
}

func TestSharedTrackedPath(t *testing.T) { eachBackend(t, testSharedTrackedPath) }

// A tracked path can be shared where the backend bind mounts it; backends
// that use symlinks refuse, since git would see the tracked file replaced.
func testSharedTrackedPath(t *testing.T) {
	m, _ := setup(t)
	ctx := context.Background()
	if _, err := m.SetShares("demo", []string{"README.md"}, false); err != nil {
		t.Fatal(err)
	}
	_, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "demo", ID: "a"})
	if backend.Default != "overlay" {
		if err == nil || !strings.Contains(err.Error(), "tracked by git") {
			t.Fatalf("spawn sharing a tracked path on %s: %v", backend.Default, err)
		}
		if sessions, _ := m.Sessions(); len(sessions) != 0 {
			t.Errorf("failed spawn left sessions: %v", sessions)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "demo", ID: "b"}); err != nil {
		t.Fatal(err)
	}
	execIn(t, m, "a", "sh", "-c", "echo shared > README.md")
	if got := execIn(t, m, "b", "git", "status", "--porcelain"); got != "M README.md" {
		t.Errorf("b's status = %q", got)
	}
	// A rename onto a bind mount fails instead of quietly unsharing.
	var out strings.Builder
	if err := m.Exec(ctx, "b", []string{"sh", "-c", "echo x > tmp && mv tmp README.md"}, nil, &out, &out); err == nil {
		t.Errorf("rename over a shared file succeeded:\n%s", out.String())
	}
}

func TestSharedFileFromDir(t *testing.T) { eachBackend(t, testSharedFileFromDir) }

// An untracked file in a prepared directory, such as .env, is shared as a
// file and seeded from the first base spawned with it.
func testSharedFileFromDir(t *testing.T) {
	m, upstream := setup(t)
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "prepared")
	git(t, filepath.Dir(dir), "clone", "--quiet", upstream, dir)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("KEY=1\n"), 0o644)
	if _, err := m.SetShares("demo", []string{"./.env"}, false); err != nil {
		t.Fatal(err)
	}
	s, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "demo", FromDir: dir, ID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "demo", Base: s.Base, ID: "b"}); err != nil {
		t.Fatal(err)
	}
	if got := execIn(t, m, "b", "cat", ".env"); got != "KEY=1" {
		t.Errorf("b's .env = %q", got)
	}
	execIn(t, m, "a", "sh", "-c", "echo KEY=2 > .env")
	if got := execIn(t, m, "b", "cat", ".env"); got != "KEY=2" {
		t.Errorf("after a's write, b's .env = %q", got)
	}
	// A plain commit base without .env still gets the shared file.
	if _, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "demo", ID: "c"}); err != nil {
		t.Fatal(err)
	}
	if got := execIn(t, m, "c", "cat", ".env"); got != "KEY=2" {
		t.Errorf("c's .env = %q", got)
	}
}

func TestSharePathValidation(t *testing.T) {
	m, _ := setup(t)
	for _, tc := range []struct {
		paths []string
		want  string
	}{
		{[]string{".git"}, "--shared-git"},
		{[]string{".git/hooks"}, "--shared-git"},
		{[]string{"../x"}, "inside the tree"},
		{[]string{"/abs"}, "inside the tree"},
		{[]string{"."}, "inside the tree"},
		{[]string{"a", "a/b"}, "overlap"},
	} {
		if _, err := m.SetShares("demo", tc.paths, false); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("share %v: err = %v, want %q", tc.paths, err, tc.want)
		}
	}
	if _, err := m.SetShares("demo", []string{"nope"}, true); err == nil {
		t.Error("unsharing a path that is not shared succeeded")
	}
	r, err := m.SetShares("demo", []string{"b/", "a"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(r.SharedPaths, ",") != "a,b" {
		t.Errorf("shared paths = %v", r.SharedPaths)
	}
}

func TestSpawnShares(t *testing.T) { eachBackend(t, testSpawnShares) }

// Shares can be set when registering the repository and per spawn; a
// per-spawn path is shared among the sessions spawned with it only.
func testSpawnShares(t *testing.T) {
	m, upstream := setup(t)
	ctx := context.Background()
	if _, err := m.Git().Clone(ctx, upstream, "multi", repo.AddOptions{SharedPaths: []string{"cache", "../x"}}); err == nil {
		t.Error("add accepted the path ../x")
	}
	r, err := m.Git().Clone(ctx, upstream, "multi", repo.AddOptions{SharedPaths: []string{"out", "cache", "./cache"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.SharedPaths, ","); got != "cache,out" {
		t.Errorf("shared paths at add = %s", got)
	}
	if _, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "multi", ID: "a", Share: []string{"tmp", "logs"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "multi", ID: "b", Share: []string{"tmp"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "multi", ID: "c"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "multi", ID: "d", Share: []string{"cache/sub"}}); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Errorf("spawn sharing a path inside a shared one: %v", err)
	}
	// Shared copies nest on disk, so a path inside (or around) one that
	// already has a copy is refused, even when no session shares both.
	for _, p := range []string{"logs/x", "tmp/y/z"} {
		if _, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "multi", ID: "e", Share: []string{p}}); err == nil || !strings.Contains(err.Error(), "already has a shared copy") {
			t.Errorf("spawn sharing %s inside an existing copy: %v", p, err)
		}
	}
	if s, _ := m.Session("a"); strings.Join(s.Shared, ",") != "cache,logs,out,tmp" {
		t.Errorf("a shares %v", s.Shared)
	}
	execIn(t, m, "a", "sh", "-c", "echo t > tmp/t && echo l > logs/l && echo c > cache/c && echo o > out/o")
	if got := execIn(t, m, "b", "sh", "-c", "cat tmp/t cache/c out/o; ls logs 2>/dev/null | wc -l | tr -d ' '"); got != "t\nc\no\n0" {
		t.Errorf("b sees %q", got)
	}
	if got := execIn(t, m, "c", "sh", "-c", "cat cache/c out/o; ls tmp logs 2>/dev/null | wc -l | tr -d ' '"); got != "c\no\n0" {
		t.Errorf("c sees %q", got)
	}
}

func TestShareKindMismatch(t *testing.T) { eachBackend(t, testShareKindMismatch) }

// A share seeded as a directory (the first base had nothing there) stays
// one: a base that has a file at that path fails to spawn instead of
// having its file replaced.
func testShareKindMismatch(t *testing.T) {
	m, upstream := setup(t)
	ctx := context.Background()
	if _, err := m.SetShares("demo", []string{".env"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "demo", ID: "a"}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "prepared")
	git(t, filepath.Dir(dir), "clone", "--quiet", upstream, dir)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("KEY=1\n"), 0o644)
	_, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "demo", FromDir: dir, ID: "b"})
	if err == nil || !strings.Contains(err.Error(), "disagree on whether it is a directory") {
		t.Fatalf("spawn with a file where the share is a directory: %v", err)
	}
	if sessions, _ := m.Sessions(); len(sessions) != 1 {
		t.Errorf("failed spawn left sessions: %d", len(sessions))
	}
}
