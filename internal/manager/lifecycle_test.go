package manager_test

// Base lifecycle harness: what happens to bases and running sessions when
// upstream moves on, rewrites history, and the mirrors are garbage
// collected. Each scenario ends with git fsck inside the views, which is the
// check that matters: a view whose objects were collected looks fine until
// git touches the missing object.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tingly-dev/tingly-fuse/internal/manager"
)

// commitTo makes a commit on branch in upstream through a throwaway clone
// and pushes it, with --force when force is set.
func commitTo(t *testing.T, upstream, branch, file, content string, force bool) string {
	t.Helper()
	w := filepath.Join(t.TempDir(), "w")
	git(t, filepath.Dir(w), "clone", "--quiet", upstream, w)
	if git(t, w, "branch", "--show-current") != branch {
		git(t, w, "switch", "--quiet", "-C", branch)
	}
	os.WriteFile(filepath.Join(w, file), []byte(content), 0o644)
	git(t, w, "add", ".")
	git(t, w, "commit", "--quiet", "-m", file+": "+strings.TrimSpace(content))
	args := []string{"push", "--quiet", "origin", "HEAD:refs/heads/" + branch}
	if force {
		args = append(args, "--force")
	}
	git(t, w, args...)
	return git(t, w, "rev-parse", "HEAD")
}

// mirrors returns the repository's object mirror and its submodule mirrors.
func mirrors(t *testing.T, m *manager.Manager, repo string) []string {
	t.Helper()
	dir := filepath.Join(m.Home, "repos", repo)
	mods, _ := filepath.Glob(filepath.Join(dir, "modules", "*.git"))
	return append([]string{filepath.Join(dir, "objects.git")}, mods...)
}

// fetchAndCollect does what tingly-fuse and git do to a mirror over time,
// all at once: fetch with prune, then the most aggressive gc there is.
func fetchAndCollect(t *testing.T, m *manager.Manager, repo string) {
	t.Helper()
	for _, mir := range mirrors(t, m, repo) {
		git(t, mir, "fetch", "--quiet", "--prune", "origin")
		git(t, mir, "reflog", "expire", "--expire=now", "--all")
		git(t, mir, "-c", "gc.pruneExpire=now", "gc", "--quiet", "--prune=now")
	}
}

// fsckView fails the test if the view of session id is missing any object
// its refs, index or reflogs need, in the superproject or any submodule.
func fsckView(t *testing.T, m *manager.Manager, id string) {
	t.Helper()
	var out strings.Builder
	cmd := []string{"sh", "-c", `git fsck --no-progress --no-dangling 2>&1 && git submodule --quiet foreach --recursive 'git fsck --no-progress --no-dangling 2>&1'`}
	if err := m.Exec(context.Background(), id, cmd, nil, &out, &out); err != nil {
		t.Errorf("fsck in session %s: %v\n%s", id, err, out.String())
	}
}

func TestBaseSurvivesUpstreamRewriteAndGC(t *testing.T) {
	m, upstream := setup(t)
	ctx := context.Background()
	topic := commitTo(t, upstream, "topic", "topic.txt", "topic\n", false)

	if _, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "demo", ID: "main", From: "main"}); err != nil {
		t.Fatal(err)
	}
	tp, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "demo", ID: "topic", From: "topic"})
	if err != nil {
		t.Fatal(err)
	}
	if tp.Base != topic {
		t.Fatalf("topic base = %s, want %s", tp.Base, topic)
	}

	// Upstream deletes topic and rewrites main to an unrelated history.
	git(t, upstream, "branch", "-D", "topic")
	w := filepath.Join(t.TempDir(), "w")
	git(t, filepath.Dir(w), "init", "--quiet", "--initial-branch=main", w)
	os.WriteFile(filepath.Join(w, "new.txt"), []byte("rewritten\n"), 0o644)
	git(t, w, "add", ".")
	git(t, w, "commit", "--quiet", "-m", "rewrite")
	git(t, w, "push", "--quiet", "--force", upstream, "main")

	fetchAndCollect(t, m, "demo")

	for _, id := range []string{"main", "topic"} {
		fsckView(t, m, id)
		execIn(t, m, id, "git", "status", "--porcelain")
	}
	if got := execIn(t, m, "topic", "cat", "topic.txt"); got != "topic" {
		t.Errorf("topic.txt = %q", got)
	}
	// The old base still serves new sessions.
	if _, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "demo", ID: "again", Base: tp.Base}); err != nil {
		t.Fatal(err)
	}
	fsckView(t, m, "again")
}

func TestFromDirObjectsSurviveGC(t *testing.T) {
	m, upstream := setup(t)
	ctx := context.Background()

	// Everything a prepared directory can hold that upstream never saw:
	// an unpushed commit, a commit only in the reflog (amended away), a
	// stash, a local branch and a staged blob.
	prep := filepath.Join(t.TempDir(), "prep")
	git(t, filepath.Dir(prep), "clone", "--quiet", upstream, prep)
	os.WriteFile(filepath.Join(prep, "a.txt"), []byte("first\n"), 0o644)
	git(t, prep, "add", ".")
	git(t, prep, "commit", "--quiet", "-m", "unpushed")
	os.WriteFile(filepath.Join(prep, "a.txt"), []byte("amended\n"), 0o644)
	git(t, prep, "commit", "--quiet", "--amend", "-am", "unpushed, amended")
	git(t, prep, "branch", "local-only")
	os.WriteFile(filepath.Join(prep, "a.txt"), []byte("stashed\n"), 0o644)
	git(t, prep, "stash", "--quiet")
	os.WriteFile(filepath.Join(prep, "staged.txt"), []byte("staged\n"), 0o644)
	git(t, prep, "add", "staged.txt")

	sess, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "demo", ID: "a", FromDir: prep})
	if err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(prep)
	fetchAndCollect(t, m, "demo")

	fsckView(t, m, "a")
	if got := execIn(t, m, "a", "git", "show", ":staged.txt"); got != "staged" {
		t.Errorf("staged blob = %q", got)
	}
	if got := execIn(t, m, "a", "git", "stash", "list", "--format=%s"); !strings.Contains(got, "unpushed, amended") {
		t.Errorf("stash = %q", got)
	}
	if got := execIn(t, m, "a", "git", "log", "-1", "--format=%s", "local-only"); got != "unpushed, amended" {
		t.Errorf("local-only = %q", got)
	}
	if _, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "demo", ID: "b", Base: sess.Base}); err != nil {
		t.Fatal(err)
	}
	fsckView(t, m, "b")
}

func TestSubmoduleObjectsSurviveUpstreamRewriteAndGC(t *testing.T) {
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
	tmp := t.TempDir()
	ctx := context.Background()

	// lib's pinned commit lives only on a branch that is later deleted.
	libSrc := filepath.Join(tmp, "lib-src")
	git(t, tmp, "init", "--quiet", "--initial-branch=main", libSrc)
	os.WriteFile(filepath.Join(libSrc, "lib.txt"), []byte("v1\n"), 0o644)
	git(t, libSrc, "add", ".")
	git(t, libSrc, "commit", "--quiet", "-m", "v1")
	git(t, libSrc, "switch", "--quiet", "-c", "feature")
	os.WriteFile(filepath.Join(libSrc, "lib.txt"), []byte("feature\n"), 0o644)
	git(t, libSrc, "commit", "--quiet", "-am", "feature")
	lib := filepath.Join(tmp, "lib.git")
	git(t, tmp, "clone", "--quiet", "--mirror", libSrc, lib)

	src := filepath.Join(tmp, "app-src")
	git(t, tmp, "init", "--quiet", "--initial-branch=main", src)
	os.WriteFile(filepath.Join(src, "README.md"), []byte("app\n"), 0o644)
	git(t, src, "add", ".")
	git(t, src, "commit", "--quiet", "-m", "app")
	git(t, src, "-c", "protocol.file.allow=always", "submodule", "--quiet", "add", "-b", "feature", lib, "libs/lib")
	git(t, src, "commit", "--quiet", "-m", "add lib at feature")
	upstream := filepath.Join(tmp, "upstream.git")
	git(t, tmp, "clone", "--quiet", "--bare", src, upstream)

	m := manager.New(filepath.Join(tmp, "home"))
	if _, err := m.AddRepo(ctx, "demo", upstream); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "demo", ID: "s"}); err != nil {
		t.Fatal(err)
	}
	// The prepared-directory path seeds module mirrors differently; cover it too.
	if _, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "demo", ID: "d", FromDir: src}); err != nil {
		t.Fatal(err)
	}

	git(t, lib, "branch", "-D", "feature")
	fetchAndCollect(t, m, "demo")

	for _, id := range []string{"s", "d"} {
		fsckView(t, m, id)
		if got := execIn(t, m, id, "cat", "libs/lib/lib.txt"); got != "feature" {
			t.Errorf("%s: lib.txt = %q", id, got)
		}
	}
}

func TestSpawnFollowsUpstream(t *testing.T) {
	m, upstream := setup(t)
	ctx := context.Background()
	old, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "demo", ID: "old"})
	if err != nil {
		t.Fatal(err)
	}
	next := commitTo(t, upstream, "main", "next.txt", "next\n", false)

	stale, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "demo", ID: "stale", From: "main", NoFetch: true})
	if err != nil {
		t.Fatal(err)
	}
	if stale.Base != old.Base {
		t.Errorf("--no-fetch base = %s, want the mirror's %s", stale.Base, old.Base)
	}
	for _, from := range []string{"", "main"} {
		s, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "demo", From: from})
		if err != nil {
			t.Fatal(err)
		}
		if s.Base != next {
			t.Errorf("spawn --from %q: base = %s, want upstream's %s", from, s.Base, next)
		}
	}
	// A full commit id the mirror has needs no fetch, so works offline.
	if err := os.Rename(upstream, upstream+".away"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "demo", From: old.Base}); err != nil {
		t.Errorf("spawn by sha with upstream gone: %v", err)
	}
	if _, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "demo", From: "main"}); err == nil || !strings.Contains(err.Error(), "--no-fetch") {
		t.Errorf("spawn --from main with upstream gone: err = %v, want a hint at --no-fetch", err)
	}
	// The old session still sees its own base.
	if got := execIn(t, m, "old", "sh", "-c", "test -e next.txt && echo yes || echo no"); got != "no" {
		t.Errorf("old session sees next.txt: %s", got)
	}
}

func TestCheckRepoFindsAndRepairsUnpinnedBases(t *testing.T) {
	m, _ := setup(t)
	ctx := context.Background()
	s, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "demo", ID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	health, err := m.CheckRepo(ctx, "demo", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(health) != 1 || !health[0].Pinned || health[0].Err != nil || health[0].Sessions != 1 {
		t.Fatalf("health = %+v", health)
	}

	// A base made before pinning existed: no refs/tingly in the mirror.
	mirror := mirrors(t, m, "demo")[0]
	for _, ref := range strings.Fields(git(t, mirror, "for-each-ref", "--format=%(refname)", "refs/tingly/")) {
		git(t, mirror, "update-ref", "-d", ref)
	}
	if health, _ = m.CheckRepo(ctx, "demo", false); health[0].Pinned {
		t.Fatal("unpinned base reported as pinned")
	}
	if health, _ = m.CheckRepo(ctx, "demo", true); !health[0].Pinned || health[0].Err != nil {
		t.Fatalf("after --fix: %+v", health[0])
	}

	// Spawning on an unpinned base pins it too.
	for _, ref := range strings.Fields(git(t, mirror, "for-each-ref", "--format=%(refname)", "refs/tingly/")) {
		git(t, mirror, "update-ref", "-d", ref)
	}
	if _, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "demo", ID: "b", Base: s.Base}); err != nil {
		t.Fatal(err)
	}
	if health, _ = m.CheckRepo(ctx, "demo", false); !health[0].Pinned {
		t.Fatal("spawn --base did not pin")
	}

	// Objects lost anyway (here: deleted by hand) are reported, not hidden.
	git(t, mirror, "update-ref", "-d", "refs/heads/main")
	for _, ref := range strings.Fields(git(t, mirror, "for-each-ref", "--format=%(refname)", "refs/tingly/")) {
		git(t, mirror, "update-ref", "-d", ref)
	}
	git(t, mirror, "-c", "gc.pruneExpire=now", "gc", "--quiet", "--prune=now")
	if health, _ = m.CheckRepo(ctx, "demo", false); health[0].Err == nil {
		t.Error("missing objects not reported")
	}
}
