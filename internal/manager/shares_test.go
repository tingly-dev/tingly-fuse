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
		if _, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "demo", ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	execIn(t, m, "a", "sh", "-c", "echo hi > cache/x && echo o > build/out/o")
	if got := execIn(t, m, "b", "cat", "cache/x", "build/out/o"); got != "hi\no" {
		t.Errorf("b reads %q", got)
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
	if _, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "demo", ID: "c"}); err != nil {
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
	_, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "demo", ID: "a"})
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
	if _, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "demo", ID: "b"}); err != nil {
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
	s, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "demo", FromDir: dir, ID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "demo", Base: s.Base, ID: "b"}); err != nil {
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
	if _, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "demo", ID: "c"}); err != nil {
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
