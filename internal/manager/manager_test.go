package manager_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	_ "github.com/tingly-dev/tingly-fuse/internal/backend/fullcopy"
	"github.com/tingly-dev/tingly-fuse/internal/manager"
	"github.com/tingly-dev/tingly-fuse/internal/repo"
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

// setup returns a manager with an upstream bare repo registered as "demo".
func setup(t *testing.T) (*manager.Manager, string) {
	t.Helper()
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
	tmp := t.TempDir()

	src := filepath.Join(tmp, "src")
	git(t, tmp, "init", "--quiet", "--initial-branch=main", src)
	os.WriteFile(filepath.Join(src, "README.md"), []byte("hello\n"), 0o644)
	git(t, src, "add", ".")
	git(t, src, "commit", "--quiet", "-m", "init")
	upstream := filepath.Join(tmp, "upstream.git")
	git(t, tmp, "clone", "--quiet", "--bare", src, upstream)

	m := manager.New(filepath.Join(tmp, "home"))
	dropAll(t, m)
	if _, err := m.Git().Clone(context.Background(), upstream, "demo", repo.AddOptions{}); err != nil {
		t.Fatal(err)
	}
	return m, upstream
}

func execIn(t *testing.T, m *manager.Manager, id string, argv ...string) string {
	t.Helper()
	var out bytes.Buffer
	if err := m.Exec(context.Background(), id, argv, nil, &out, &out); err != nil {
		t.Fatalf("exec %v in %s: %v\n%s", argv, id, err, out.String())
	}
	return strings.TrimSpace(out.String())
}

func TestSessionsAreIsolatedAndPushBranches(t *testing.T) {
	m, upstream := setup(t)
	ctx := context.Background()

	a, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "demo", ID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "demo", ID: "b", Branch: "feature/b"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Base != b.Base {
		t.Fatalf("sessions from the same rev should share a base: %s vs %s", a.Base, b.Base)
	}

	execIn(t, m, "a", "sh", "-c", "echo from-a >> README.md && git commit --quiet -am a")
	execIn(t, m, "b", "sh", "-c", "echo from-b > NOTES.md && git add NOTES.md && git commit --quiet -m b")

	if got := execIn(t, m, "b", "cat", "README.md"); got != "hello" {
		t.Errorf("session b sees a's change: README.md = %q", got)
	}
	if got := execIn(t, m, "a", "git", "branch", "--show-current"); got != "agent/a" {
		t.Errorf("session a branch = %q", got)
	}
	if got := execIn(t, m, "a", "sh", "-c", `echo "$TINGLY_SESSION $TINGLY_REPO"`); got != "a demo" {
		t.Errorf("env = %q", got)
	}

	for _, id := range []string{"a", "b"} {
		var out bytes.Buffer
		if err := m.Exec(ctx, id, []string{"git", "push", "--quiet", "origin", "HEAD"}, nil, &out, &out); err != nil {
			t.Fatalf("push %s: %v\n%s", id, err, out.String())
		}
	}
	branches := git(t, upstream, "branch", "--format=%(refname:short)")
	for _, want := range []string{"agent/a", "feature/b", "main"} {
		if !strings.Contains(branches, want) {
			t.Errorf("upstream branches %q missing %q", branches, want)
		}
	}

	// The shared base must be untouched and hold no objects of its own.
	base := filepath.Join(m.Home, "repos", "demo", "bases", a.Base)
	if data, _ := os.ReadFile(filepath.Join(base, "README.md")); string(data) != "hello\n" {
		t.Errorf("base README.md modified: %q", data)
	}
	if packs, _ := filepath.Glob(filepath.Join(base, ".git", "objects", "pack", "*")); len(packs) != 0 {
		t.Errorf("base has its own packs: %v", packs)
	}

	if err := m.Drop(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.ViewPath); !os.IsNotExist(err) {
		t.Errorf("view still exists after drop: %v", err)
	}
	sessions, _ := m.Sessions()
	if len(sessions) != 1 || sessions[0].ID != "b" {
		t.Errorf("sessions after drop = %v", sessions)
	}
}

func TestConcurrentSpawnSharesOneBase(t *testing.T) {
	m, _ := setup(t)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = m.Git().AddWorktree(context.Background(), manager.WorktreeOptions{Repo: "demo"})
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	bases, _ := os.ReadDir(filepath.Join(m.Home, "repos", "demo", "bases"))
	if len(bases) != 1 {
		names := []string{}
		for _, b := range bases {
			names = append(names, b.Name())
		}
		t.Errorf("want one base, got %v", names)
	}
}

func TestSpawnFailureCleansUp(t *testing.T) {
	m, _ := setup(t)
	ctx := context.Background()
	if _, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "demo", ID: "x", Branch: "bad..name"}); err == nil {
		t.Fatal("expected invalid branch name to fail")
	}
	if _, err := os.Stat(filepath.Join(m.Home, "sessions", "x")); !os.IsNotExist(err) {
		t.Errorf("failed spawn left state behind: %v", err)
	}
	if _, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "demo", Backend: "nope"}); err == nil {
		t.Error("expected unknown backend to fail")
	}
}

func TestSpawnChecksOutSubmodules(t *testing.T) {
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
	tmp := t.TempDir()
	ctx := context.Background()

	// lib.git <- nested.git: a submodule that itself has a submodule.
	mkrepo := func(name, file string) string {
		src := filepath.Join(tmp, name+"-src")
		git(t, tmp, "init", "--quiet", "--initial-branch=main", src)
		os.WriteFile(filepath.Join(src, file), []byte(name+"\n"), 0o644)
		git(t, src, "add", ".")
		git(t, src, "commit", "--quiet", "-m", name)
		return src
	}
	nestedSrc := mkrepo("nested", "nested.txt")
	nested := filepath.Join(tmp, "nested.git")
	git(t, tmp, "clone", "--quiet", "--bare", nestedSrc, nested)

	libSrc := mkrepo("lib", "lib.txt")
	git(t, libSrc, "-c", "protocol.file.allow=always", "submodule", "--quiet", "add", nested, "deps/nested")
	git(t, libSrc, "commit", "--quiet", "-m", "add nested")
	lib := filepath.Join(tmp, "lib.git")
	git(t, tmp, "clone", "--quiet", "--bare", libSrc, lib)

	src := mkrepo("app", "README.md")
	git(t, src, "-c", "protocol.file.allow=always", "submodule", "--quiet", "add", lib, "libs/lib")
	git(t, src, "commit", "--quiet", "-m", "add lib")
	upstream := filepath.Join(tmp, "upstream.git")
	git(t, tmp, "clone", "--quiet", "--bare", src, upstream)

	m := manager.New(filepath.Join(tmp, "home"))
	if _, err := m.Git().Clone(ctx, upstream, "demo", repo.AddOptions{}); err != nil {
		t.Fatal(err)
	}
	sess, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "demo", ID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if got := execIn(t, m, "s", "cat", "libs/lib/lib.txt", "libs/lib/deps/nested/nested.txt"); got != "lib\nnested" {
		t.Errorf("submodule contents = %q", got)
	}
	if got := execIn(t, m, "s", "git", "status", "--porcelain", "--ignore-submodules=none"); got != "" {
		t.Errorf("view not clean: %q", got)
	}
	if got := execIn(t, m, "s", "git", "-C", "libs/lib", "remote", "get-url", "origin"); got != lib {
		t.Errorf("submodule origin = %q, want %q", got, lib)
	}

	// Submodule repos in the base hold no objects of their own.
	base := filepath.Join(m.Home, "repos", "demo", "bases", sess.Base)
	packs, _ := filepath.Glob(filepath.Join(base, ".git", "modules", "*", "objects", "pack", "*.pack"))
	nestedPacks, _ := filepath.Glob(filepath.Join(base, ".git", "modules", "*", "modules", "*", "objects", "pack", "*.pack"))
	if len(packs)+len(nestedPacks) != 0 {
		t.Errorf("base submodules have their own packs: %v %v", packs, nestedPacks)
	}
	loose, _ := filepath.Glob(filepath.Join(base, ".git", "modules", "*", "objects", "??", "*"))
	if len(loose) != 0 {
		t.Errorf("base submodules have loose objects: %v", loose)
	}

	// A second base reuses the submodule mirrors instead of cloning again.
	mirrors, _ := filepath.Glob(filepath.Join(m.Home, "repos", "demo", "modules", "*.git"))
	if len(mirrors) != 2 {
		t.Errorf("want 2 submodule mirrors (lib, nested), got %v", mirrors)
	}
}

func TestRemoveRepo(t *testing.T) {
	m, _ := setup(t)
	ctx := context.Background()
	if _, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "demo", ID: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveRepo(ctx, "demo", false); err == nil || !strings.Contains(err.Error(), "a") {
		t.Fatalf("remove with live session: err = %v, want refusal naming the session", err)
	}
	repos, _ := m.Repos()
	if len(repos) != 1 {
		t.Fatalf("refused remove still deleted the repo: %v", repos)
	}
	if b, _ := repos[0].Bases(); len(b) != 1 {
		t.Fatalf("bases = %v, want one", b)
	}

	if err := m.RemoveRepo(ctx, "demo", true); err != nil {
		t.Fatal(err)
	}
	if repos, _ := m.Repos(); len(repos) != 0 {
		t.Errorf("repos after remove = %v", repos)
	}
	if sessions, _ := m.Sessions(); len(sessions) != 0 {
		t.Errorf("sessions after forced remove = %v", sessions)
	}
	if _, err := os.Stat(filepath.Join(m.Home, "repos", "demo")); !os.IsNotExist(err) {
		t.Errorf("repo dir survived: %v", err)
	}
	if err := m.RemoveRepo(ctx, "demo", false); err == nil {
		t.Error("removing an unregistered repo should fail")
	}
}

func TestSpawnFromDir(t *testing.T) {
	m, upstream := setup(t)
	ctx := context.Background()
	tmp := t.TempDir()

	// A prepared directory: a --shared clone (objects only through
	// alternates), with an ignored build output, an unpushed commit and a
	// staged-but-uncommitted file.
	prep := filepath.Join(tmp, "prep")
	git(t, tmp, "clone", "--quiet", "--shared", upstream, prep)
	os.WriteFile(filepath.Join(prep, ".gitignore"), []byte("node_modules/\n"), 0o644)
	git(t, prep, "add", ".")
	git(t, prep, "commit", "--quiet", "-m", "local, unpushed")
	os.MkdirAll(filepath.Join(prep, "node_modules", "dep"), 0o755)
	os.WriteFile(filepath.Join(prep, "node_modules", "dep", "index.js"), []byte("installed\n"), 0o644)
	os.WriteFile(filepath.Join(prep, "staged.txt"), []byte("staged\n"), 0o644)
	git(t, prep, "add", "staged.txt")
	head := git(t, prep, "rev-parse", "HEAD")

	sess, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "demo", ID: "a", FromDir: prep})
	if err != nil {
		t.Fatal(err)
	}
	if sess.Head != head || sess.Source != prep || !strings.HasPrefix(sess.Base, "dir-"+head[:12]+"-") {
		t.Errorf("session = %+v", sess)
	}
	if got := execIn(t, m, "a", "cat", "node_modules/dep/index.js"); got != "installed" {
		t.Errorf("ignored file = %q", got)
	}
	if got := execIn(t, m, "a", "git", "log", "-1", "--format=%s"); got != "local, unpushed" {
		t.Errorf("HEAD = %q", got)
	}
	if got := execIn(t, m, "a", "git", "show", ":staged.txt"); got != "staged" {
		t.Errorf("staged blob = %q", got)
	}
	if got := execIn(t, m, "a", "git", "remote", "get-url", "origin"); got != upstream {
		t.Errorf("origin = %q, want %q", got, upstream)
	}

	// The base holds no objects of its own; they live in the mirror.
	base := filepath.Join(m.Home, "repos", "demo", "bases", sess.Base)
	if p, _ := filepath.Glob(filepath.Join(base, ".git", "objects", "*", "*")); len(p) != 1 { // only info/alternates
		t.Errorf("base has own objects: %v", p)
	}

	// Changing or deleting the source afterwards does not affect the session.
	os.WriteFile(filepath.Join(prep, "node_modules", "dep", "index.js"), []byte("changed\n"), 0o644)
	if err := os.RemoveAll(prep); err != nil {
		t.Fatal(err)
	}
	if got := execIn(t, m, "a", "sh", "-c", "cat node_modules/dep/index.js && git fsck --no-progress 2>&1 | grep -c missing || true"); !strings.HasPrefix(got, "installed") || !strings.HasSuffix(got, "0") {
		t.Errorf("after source removed: %q", got)
	}

	// A second session reuses the snapshot, and can push.
	if _, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "demo", ID: "b", Base: sess.Base}); err != nil {
		t.Fatal(err)
	}
	execIn(t, m, "b", "sh", "-c", "echo b > b.txt && git add b.txt && git commit -qm b")
	if err := m.Exec(ctx, "b", []string{"git", "push", "--quiet", "origin", "HEAD"}, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "demo", ID: "c", Base: "nope"}); err == nil {
		t.Error("unknown base accepted")
	}
	if _, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "demo", ID: "d", Base: sess.Base, Commit: "main"}); err == nil {
		t.Error("--base with --from accepted")
	}
}

func TestSpawnFromDirWithSubmodule(t *testing.T) {
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
	tmp := t.TempDir()
	ctx := context.Background()

	libSrc := filepath.Join(tmp, "lib-src")
	git(t, tmp, "init", "--quiet", "--initial-branch=main", libSrc)
	os.WriteFile(filepath.Join(libSrc, "lib.txt"), []byte("lib\n"), 0o644)
	git(t, libSrc, "add", ".")
	git(t, libSrc, "commit", "--quiet", "-m", "lib")
	lib := filepath.Join(tmp, "lib.git")
	git(t, tmp, "clone", "--quiet", "--bare", libSrc, lib)

	src := filepath.Join(tmp, "app-src")
	git(t, tmp, "init", "--quiet", "--initial-branch=main", src)
	os.WriteFile(filepath.Join(src, "README.md"), []byte("app\n"), 0o644)
	git(t, src, "add", ".")
	git(t, src, "commit", "--quiet", "-m", "app")
	git(t, src, "-c", "protocol.file.allow=always", "submodule", "--quiet", "add", lib, "libs/lib")
	git(t, src, "commit", "--quiet", "-m", "add lib")
	upstream := filepath.Join(tmp, "upstream.git")
	git(t, tmp, "clone", "--quiet", "--bare", src, upstream)

	// src is itself the prepared directory; the submodule gitdir under
	// .git/modules has its own objects.
	m := manager.New(filepath.Join(tmp, "home"))
	if _, err := m.Git().Clone(ctx, upstream, "demo", repo.AddOptions{}); err != nil {
		t.Fatal(err)
	}
	sess, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "demo", ID: "s", FromDir: src})
	if err != nil {
		t.Fatal(err)
	}
	if got := execIn(t, m, "s", "cat", "libs/lib/lib.txt"); got != "lib" {
		t.Errorf("submodule contents = %q", got)
	}
	if got := execIn(t, m, "s", "git", "-C", "libs/lib", "remote", "get-url", "origin"); got != lib {
		t.Errorf("submodule origin = %q, want %q", got, lib)
	}
	base := filepath.Join(m.Home, "repos", "demo", "bases", sess.Base)
	packs, _ := filepath.Glob(filepath.Join(base, ".git", "modules", "*", "objects", "pack", "*.pack"))
	loose, _ := filepath.Glob(filepath.Join(base, ".git", "modules", "*", "objects", "??", "*"))
	if len(packs)+len(loose) != 0 {
		t.Errorf("base submodule has own objects: %v %v", packs, loose)
	}
	mirrors, _ := filepath.Glob(filepath.Join(m.Home, "repos", "demo", "modules", "*.git"))
	if len(mirrors) != 1 {
		t.Errorf("want 1 submodule mirror, got %v", mirrors)
	}
	// The seeded mirror still fetches from the real URL when asked for more.
	if out, err := exec.Command("git", "-C", mirrors[0], "fsck", "--no-progress").CombinedOutput(); err != nil {
		t.Errorf("mirror fsck: %v\n%s", err, out)
	}
}
