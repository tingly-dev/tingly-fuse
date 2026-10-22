package manager_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/tingly-dev/tingly-fuse/internal/manager"
	"github.com/tingly-dev/tingly-fuse/internal/repo"
)

// newDir makes a plain directory to register, with a.txt, sub/b and an
// empty cache/.
func newDir(t *testing.T) (m *manager.Manager, src string) {
	t.Helper()
	tmp := t.TempDir()
	src = filepath.Join(tmp, "src")
	for _, d := range []string{"sub", "cache"} {
		if err := os.MkdirAll(filepath.Join(src, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("one\n"), 0o644)
	os.WriteFile(filepath.Join(src, "sub", "b"), []byte("b\n"), 0o644)
	m = manager.New(filepath.Join(tmp, "home"))
	dropAll(t, m)
	return m, src
}

func TestDirRepo(t *testing.T) { eachBackend(t, testDirRepo) }

// A plain directory works like a git repository minus git: sessions get
// private views of a snapshot, share what they are told to, and a new
// snapshot only affects sessions spawned after it.
func testDirRepo(t *testing.T) {
	m, src := newDir(t)
	ctx := context.Background()
	r, first, err := m.AddDir(ctx, "data", src, repo.DirOptions{SharedPaths: []string{"cache"}})
	if err != nil {
		t.Fatal(err)
	}
	if r.IsGit() || r.Kind != repo.KindDir || r.Current != first || r.Source != src {
		t.Errorf("repo = %+v", r)
	}
	for _, id := range []string{"a", "b"} {
		s, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "data", ID: id})
		if err != nil {
			t.Fatal(err)
		}
		if s.Base != first || s.Branch != "" || !slices.Equal(s.Shared, []string{"cache"}) {
			t.Errorf("session %s = %+v", id, s)
		}
	}
	execIn(t, m, "a", "sh", "-c", "echo mine > a.txt && rm sub/b && echo c > cache/c")
	if got := execIn(t, m, "b", "sh", "-c", "cat a.txt sub/b cache/c; echo $TINGLY_SESSION"); got != "one\nb\nc\nb" {
		t.Errorf("b sees %q", got)
	}
	// The snapshot is a copy: changing the source later changes no view.
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("two\n"), 0o644)
	if got := execIn(t, m, "b", "cat", "a.txt"); got != "one" {
		t.Errorf("b's a.txt after the source changed = %q", got)
	}

	_, second, err := m.AddDir(ctx, "data", src, repo.DirOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatalf("second snapshot reused id %s", first)
	}
	if _, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "data", ID: "c"}); err != nil {
		t.Fatal(err)
	}
	if got := execIn(t, m, "c", "cat", "a.txt", "cache/c"); got != "two\nc" {
		t.Errorf("c sees %q", got)
	}
	if got := execIn(t, m, "b", "cat", "a.txt"); got != "one" {
		t.Errorf("b's a.txt after a new snapshot = %q", got)
	}
	if _, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "data", ID: "d", Base: first}); err != nil {
		t.Fatal(err)
	}
	if got := execIn(t, m, "d", "cat", "a.txt"); got != "one" {
		t.Errorf("d, spawned from the first snapshot, sees %q", got)
	}
	if _, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "data", ID: "e", Base: "nope"}); err == nil || !strings.Contains(err.Error(), "no base") {
		t.Errorf("spawn from a missing base: %v", err)
	}

	// The current snapshot is never pruned or removed, even unused.
	for _, id := range []string{"a", "b", "c", "d"} {
		if err := m.Drop(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	bases, err := m.Bases(ctx, "data")
	if err != nil || len(bases) != 2 {
		t.Fatalf("bases = %+v, %v", bases, err)
	}
	for _, b := range bases {
		if b.Current != (b.ID == second) || b.Head != "" {
			t.Errorf("base %+v", b)
		}
	}
	if err := m.RemoveBase(ctx, "data", second); err == nil || !strings.Contains(err.Error(), "new sessions of data start from") {
		t.Errorf("remove the current base: %v", err)
	}
	pruned, err := m.PruneBases(ctx, "data", false)
	if err != nil || !slices.Equal(pruned, []string{first}) {
		t.Errorf("prune = %v, %v", pruned, err)
	}
	if _, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "data", ID: "f"}); err != nil {
		t.Fatalf("spawn after prune: %v", err)
	}
	if err := m.RemoveRepo(ctx, "data", true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Repo("data"); err == nil {
		t.Error("data still registered after remove")
	}
}

func TestDirRepoWithGitInside(t *testing.T) { eachBackend(t, testDirRepoWithGitInside) }

// The generic layer does not interpret a .git in the directory: each
// session gets its own copy, so commits stay in the session, and a tracked
// path can be shared like any other.
func testDirRepoWithGitInside(t *testing.T) {
	_, upstream := setup(t)
	m, _ := newDir(t)
	ctx := context.Background()
	co := filepath.Join(t.TempDir(), "co")
	git(t, filepath.Dir(co), "clone", "--quiet", upstream, co)
	if _, _, err := m.AddDir(ctx, "plain", co, repo.DirOptions{SharedPaths: []string{"README.md"}}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if _, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "plain", ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	execIn(t, m, "a", "sh", "-c", "git commit --quiet --allow-empty -m in-a")
	if got := execIn(t, m, "b", "git", "log", "-1", "--format=%s"); got != "init" {
		t.Errorf("b's HEAD = %q, want the clone's", got)
	}
	if got := git(t, co, "log", "-1", "--format=%s"); got != "init" {
		t.Errorf("source HEAD = %q", got)
	}
	execIn(t, m, "a", "sh", "-c", "echo shared > README.md")
	if got := execIn(t, m, "b", "cat", "README.md"); got != "shared" {
		t.Errorf("b's README.md = %q", got)
	}
}

// Each layer refuses the other's repositories, and a failed registration
// leaves nothing behind.
func TestLayerRefusals(t *testing.T) {
	m, src := newDir(t)
	ctx := context.Background()
	_, upstream := setup(t)
	if _, err := m.Git().Clone(ctx, upstream, "g", repo.AddOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.AddDir(ctx, "d", src, repo.DirOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "d", ID: "s"}); err != nil {
		t.Fatal(err)
	}
	notGit := "plain directory, not a git repository"
	for name, err := range map[string]error{
		"git worktree add": func() error { _, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "d"}); return err }(),
		"git fetch":        m.Git().Fetch(ctx, "d"),
		"git fsck":         func() error { _, err := m.Git().Fsck(ctx, "d", false); return err }(),
		"git push":         m.Git().Push(ctx, "s", "", nil, nil),
	} {
		if err == nil || !strings.Contains(err.Error(), notGit) {
			t.Errorf("%s on a dir repo: %v", name, err)
		}
	}
	if _, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "g"}); err == nil || !strings.Contains(err.Error(), "git worktree add") {
		t.Errorf("spawn on a git repo: %v", err)
	}
	if _, _, err := m.AddDir(ctx, "g", src, repo.DirOptions{}); err == nil || !strings.Contains(err.Error(), "--from-dir") {
		t.Errorf("base add on a git repo: %v", err)
	}
	if _, _, err := m.AddDir(ctx, "d", src, repo.DirOptions{SharedPaths: []string{"x"}}); err == nil || !strings.Contains(err.Error(), "share and unshare") {
		t.Errorf("base add with shares on a registered repo: %v", err)
	}
	for _, c := range []struct{ name, src string }{
		{"missing", filepath.Join(src, "nope")},
		{"file", filepath.Join(src, "a.txt")},
		{"bad/name", src},
	} {
		if _, _, err := m.AddDir(ctx, c.name, c.src, repo.DirOptions{}); err == nil {
			t.Errorf("base add %s %s succeeded", c.name, c.src)
		}
		if _, err := os.Stat(filepath.Join(m.Home, "repos", c.name)); err == nil {
			t.Errorf("failed base add left repos/%s", c.name)
		}
	}
}

// base add, spawn and prune at the same time: every spawn finds a base,
// and the current snapshot always survives the prunes.
func TestDirSnapshotsDoNotRacePrune(t *testing.T) {
	m, src := newDir(t)
	ctx := context.Background()
	if _, _, err := m.AddDir(ctx, "data", src, repo.DirOptions{}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 30)
	for i := 0; i < 10; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			_, _, err := m.AddDir(ctx, "data", src, repo.DirOptions{})
			errs <- err
		}()
		go func() {
			defer wg.Done()
			_, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "data", Backend: "copy"})
			errs <- err
		}()
		go func() {
			defer wg.Done()
			_, err := m.PruneBases(ctx, "data", false)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	r, err := m.Repo("data")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.BaseDir(r.Current)); err != nil {
		t.Errorf("current snapshot %s is gone: %v", r.Current, err)
	}
	sessions, _ := m.Sessions()
	for _, s := range sessions {
		if _, err := os.Stat(r.BaseDir(s.Base)); err != nil {
			t.Errorf("session %s lost its base: %v", s.ID, err)
		}
	}
	if len(sessions) != 10 {
		t.Errorf("%d of 10 spawns succeeded", len(sessions))
	}
}
