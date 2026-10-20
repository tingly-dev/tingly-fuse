package manager_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tingly-dev/tingly-fuse/internal/manager"
	"github.com/tingly-dev/tingly-fuse/internal/repo"
)

// setupShared registers an upstream with a submodule (libs/lib) as "demo"
// with shared git on.
func setupShared(t *testing.T) (*manager.Manager, string) {
	t.Helper()
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
	tmp := t.TempDir()
	mkrepo := func(name, file string) string {
		src := filepath.Join(tmp, name+"-src")
		git(t, tmp, "init", "--quiet", "--initial-branch=main", src)
		os.WriteFile(filepath.Join(src, file), []byte(name+"\n"), 0o644)
		git(t, src, "add", ".")
		git(t, src, "commit", "--quiet", "-m", name)
		return src
	}
	lib := filepath.Join(tmp, "lib.git")
	git(t, tmp, "clone", "--quiet", "--bare", mkrepo("lib", "lib.txt"), lib)
	src := mkrepo("app", "README.md")
	git(t, src, "-c", "protocol.file.allow=always", "submodule", "--quiet", "add", lib, "libs/lib")
	git(t, src, "commit", "--quiet", "-m", "add lib")
	upstream := filepath.Join(tmp, "upstream.git")
	git(t, tmp, "clone", "--quiet", "--bare", src, upstream)

	m := manager.New(filepath.Join(tmp, "home"))
	dropAll(t, m)
	if _, err := m.AddRepoWith(context.Background(), "demo", upstream, repo.AddOptions{SharedGit: true}); err != nil {
		t.Fatal(err)
	}
	return m, upstream
}

func TestSharedGit(t *testing.T) { eachBackend(t, testSharedGit) }

// Sessions of a shared-git repository see each other's branches at once,
// keep their own HEAD, index and submodules, and one session's gc never
// takes what another session still needs.
func testSharedGit(t *testing.T) {
	m, upstream := setupShared(t)
	ctx := context.Background()
	for _, id := range []string{"a", "b"} {
		if _, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "demo", ID: id}); err != nil {
			t.Fatal(err)
		}
		if got := execIn(t, m, id, "git", "status", "--porcelain", "--ignore-submodules=none"); got != "" {
			t.Errorf("session %s not clean after spawn: %q", id, got)
		}
		if got := execIn(t, m, id, "cat", "libs/lib/lib.txt"); got != "lib" {
			t.Errorf("session %s submodule contents = %q", id, got)
		}
	}

	// A commit in a is visible in b without pushing.
	execIn(t, m, "a", "sh", "-c", "echo a > a.txt && git add a.txt && git commit --quiet -m from-a")
	if got := execIn(t, m, "b", "git", "log", "-1", "--format=%s", "agent/a"); got != "from-a" {
		t.Errorf("b sees agent/a at %q, want from-a", got)
	}
	// Git itself keeps two sessions off one branch.
	var out strings.Builder
	if err := m.Exec(ctx, "b", []string{"git", "switch", "agent/a"}, nil, &out, &out); err == nil {
		t.Errorf("b could check out a's branch:\n%s", out.String())
	}
	// HEAD, index and submodules stay per session.
	execIn(t, m, "a", "sh", "-c", "cd libs/lib && echo x > x.txt && git add x.txt && git commit --quiet -m sub-a")
	if got := execIn(t, m, "b", "git", "status", "--porcelain", "--ignore-submodules=none"); got != "" {
		t.Errorf("a's work leaked into b's status: %q", got)
	}
	if got := execIn(t, m, "b", "git", "branch", "--show-current"); got != "agent/b" {
		t.Errorf("b is on %q", got)
	}

	// The most aggressive gc in one session keeps what the other has staged.
	execIn(t, m, "b", "sh", "-c", "echo staged > st.txt && git add st.txt")
	blob := execIn(t, m, "b", "git", "rev-parse", ":st.txt")
	execIn(t, m, "a", "sh", "-c", "git reflog expire --expire=now --all && git gc --quiet --prune=now && git worktree prune")
	execIn(t, m, "b", "git", "cat-file", "-e", blob)
	fsckView(t, m, "a")
	fsckView(t, m, "b")

	if err := m.Push(ctx, "a", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := git(t, upstream, "log", "-1", "--format=%s", "agent/a"); got != "from-a" {
		t.Errorf("upstream agent/a = %q", got)
	}

	// Dropping a keeps its branch for the others, and only its own entry
	// leaves shared.git.
	if err := m.Drop(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(m.Home, "repos", "demo", "shared.git", "worktrees", "a")); !os.IsNotExist(err) {
		t.Errorf("a's worktree entry survived drop: %v", err)
	}
	if got := execIn(t, m, "b", "git", "log", "-1", "--format=%s", "agent/a"); got != "from-a" {
		t.Errorf("after drop, b sees agent/a at %q", got)
	}
	execIn(t, m, "b", "git", "worktree", "prune")
	if got := execIn(t, m, "b", "git", "status", "--porcelain"); got != "A  st.txt" {
		t.Errorf("b after prune: %q", got)
	}
	fsckView(t, m, "b")
}

func TestSharedRefsSurviveMirrorGC(t *testing.T) { eachBackend(t, testSharedRefsSurviveMirrorGC) }

// A session fetches a branch whose objects the mirror already has, so
// shared.git stores none of them; upstream then deletes the branch. The
// mirror must still keep those objects, or every gc in shared.git fails.
func testSharedRefsSurviveMirrorGC(t *testing.T) {
	m, upstream := setupShared(t)
	ctx := context.Background()
	if _, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "demo", ID: "s"}); err != nil {
		t.Fatal(err)
	}
	commitTo(t, upstream, "feature", "f.txt", "feature\n", false)
	if err := m.FetchRepo(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	execIn(t, m, "s", "git", "fetch", "--quiet", "origin")
	git(t, upstream, "branch", "-D", "feature")
	commitTo(t, upstream, "main", "g.txt", "rewrite\n", true)

	if err := m.FetchRepo(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	fetchAndCollect(t, m, "demo")
	fsckView(t, m, "s")
	execIn(t, m, "s", "git", "gc", "--quiet")
}

func TestSharedGitFromDir(t *testing.T) { eachBackend(t, testSharedGitFromDir) }

// A --from-dir base keeps its untracked files and submodules in a shared
// session, which starts detached at the directory's HEAD.
func testSharedGitFromDir(t *testing.T) {
	m, upstream := setupShared(t)
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "prepared")
	git(t, filepath.Dir(dir), "-c", "protocol.file.allow=always", "clone", "--quiet", "--recurse-submodules", upstream, dir)
	os.WriteFile(filepath.Join(dir, "built.out"), []byte("artifact\n"), 0o644)
	head := git(t, dir, "rev-parse", "HEAD")
	if _, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "demo", FromDir: dir, ID: "s"}); err != nil {
		t.Fatal(err)
	}
	if got := execIn(t, m, "s", "git", "status", "--porcelain", "--ignore-submodules=none"); got != "?? built.out" {
		t.Errorf("status = %q", got)
	}
	if got := execIn(t, m, "s", "git", "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD = %s, want %s", got, head)
	}
	fsckView(t, m, "s")
}
