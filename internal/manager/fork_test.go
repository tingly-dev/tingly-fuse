package manager_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tingly-dev/tingly-fuse/internal/backend"
	"github.com/tingly-dev/tingly-fuse/internal/manager"
	"github.com/tingly-dev/tingly-fuse/internal/repo"
)

func TestForkCopiesTheTree(t *testing.T) { eachBackend(t, testForkCopiesTheTree) }

// A fork sees the parent's tree as it is: committed, staged, untracked and
// ignored-by-nothing files alike. After that the two diverge.
func testForkCopiesTheTree(t *testing.T) {
	m, _ := setup(t)
	ctx := context.Background()
	if _, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "demo", ID: "a"}); err != nil {
		t.Fatal(err)
	}
	if backend.Default == "overlay-ns" {
		// Its view is not a directory the host can read.
		if _, err := m.Fork(ctx, "a", manager.ForkOptions{ID: "b"}); err == nil || !strings.Contains(err.Error(), "only commands run by tingly-fuse") {
			t.Errorf("fork of a namespace-only view: %v", err)
		}
		if sessions, _ := m.Sessions(); len(sessions) != 1 {
			t.Errorf("failed fork left sessions: %v", sessions)
		}
		return
	}
	execIn(t, m, "a", "sh", "-c", `echo changed > README.md && git commit --quiet -am "a work" &&
		mkdir -p deps && echo staged > deps/s && git add deps/s && echo loose > NEW.md && echo dirty >> README.md`)

	b, err := m.Fork(ctx, "a", manager.ForkOptions{ID: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if b.Backend != backend.Default || b.Branch != "agent/b" || b.ViewPath == "" {
		t.Errorf("forked session: %+v", b)
	}
	if got := execIn(t, m, "b", "git", "branch", "--show-current"); got != "agent/b" {
		t.Errorf("b's branch = %q", got)
	}
	if got := execIn(t, m, "b", "git", "log", "-1", "--format=%s"); got != "a work" {
		t.Errorf("b's HEAD = %q", got)
	}
	if got := execIn(t, m, "b", "sh", "-c", "cat README.md NEW.md deps/s"); got != "changed\ndirty\nloose\nstaged" {
		t.Errorf("b's files = %q", got)
	}
	if got := execIn(t, m, "b", "git", "status", "--porcelain"); got != "M README.md\nA  deps/s\n?? NEW.md" { // execIn trims the leading space of " M"
		t.Errorf("b's status = %q", got)
	}
	fsckView(t, m, "b")

	// They diverge from here.
	execIn(t, m, "a", "sh", "-c", "echo A > A.txt && echo again >> README.md")
	execIn(t, m, "b", "sh", "-c", "echo B > B.txt")
	if got := execIn(t, m, "b", "sh", "-c", "test ! -e A.txt && cat README.md | tail -1"); got != "dirty" {
		t.Errorf("b sees a's later changes: %q", got)
	}
	if got := execIn(t, m, "a", "sh", "-c", "test ! -e B.txt && echo ok"); got != "ok" {
		t.Errorf("a sees b's file: %q", got)
	}

	// The checkpoint is a base the new session uses, so base prune keeps it
	// until b is dropped, and then takes it.
	bases, err := m.Bases(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	var ckpt string
	for _, bi := range bases {
		if strings.HasPrefix(bi.ID, "dir-") {
			ckpt = bi.ID
			if len(bi.Sessions) != 1 || bi.Sessions[0] != "b" {
				t.Errorf("checkpoint base used by %v, want [b]", bi.Sessions)
			}
		}
	}
	if ckpt == "" || b.Base != ckpt {
		t.Fatalf("no checkpoint base (b's base %q): %+v", b.Base, bases)
	}
	if pruned, err := m.PruneBases(ctx, "demo", false); err != nil || contains(pruned, ckpt) {
		t.Errorf("prune with b alive: %v, %v", pruned, err)
	}
	if err := m.Drop(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	if pruned, err := m.PruneBases(ctx, "demo", false); err != nil || !contains(pruned, ckpt) {
		t.Errorf("prune after dropping b: %v, %v", pruned, err)
	}
	// The parent is not affected by any of it.
	if got := execIn(t, m, "a", "sh", "-c", "cat A.txt"); got != "A" {
		t.Errorf("a after dropping its fork: %q", got)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestCheckpointStartsMoreSessions(t *testing.T) { eachBackend(t, testCheckpointStartsMoreSessions) }

func testCheckpointStartsMoreSessions(t *testing.T) {
	if backend.Default == "overlay-ns" {
		t.Skip("covered by the fork test")
	}
	m, _ := setup(t)
	ctx := context.Background()
	if _, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "demo", ID: "a"}); err != nil {
		t.Fatal(err)
	}
	execIn(t, m, "a", "sh", "-c", "echo built > OUT")
	base, err := m.Checkpoint(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	// Any number of sessions start from it, with their own branches.
	for _, id := range []string{"c", "d"} {
		if _, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "demo", ID: id, Base: base}); err != nil {
			t.Fatal(err)
		}
		if got := execIn(t, m, id, "cat", "OUT"); got != "built" {
			t.Errorf("%s OUT = %q", id, got)
		}
	}
	execIn(t, m, "c", "sh", "-c", "echo c > C")
	if got := execIn(t, m, "d", "sh", "-c", "test ! -e C && echo ok"); got != "ok" {
		t.Errorf("d sees c's file: %q", got)
	}
	if _, err := m.Checkpoint(ctx, "nope"); err == nil {
		t.Error("checkpoint of an unknown session succeeded")
	}
}

// Paths the session shares stay shared, and stay out of the checkpoint.
func TestForkKeepsSharesShared(t *testing.T) { eachBackend(t, testForkKeepsSharesShared) }

func testForkKeepsSharesShared(t *testing.T) {
	if backend.Default == "overlay-ns" {
		t.Skip("covered by the fork test")
	}
	m, _ := setup(t)
	ctx := context.Background()
	if _, err := m.SetShares("demo", []string{"cache"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "demo", ID: "a"}); err != nil {
		t.Fatal(err)
	}
	execIn(t, m, "a", "sh", "-c", "echo shared > cache/x && echo private > P")
	b, err := m.Fork(ctx, "a", manager.ForkOptions{ID: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(m.Home, "repos", "demo", "bases", b.Base, "cache", "x")); err == nil {
		t.Errorf("shared file copied into the checkpoint")
	}
	if got := execIn(t, m, "b", "sh", "-c", "cat cache/x P"); got != "shared\nprivate" {
		t.Errorf("b sees %q", got)
	}
	execIn(t, m, "b", "sh", "-c", "echo more > cache/y")
	if got := execIn(t, m, "a", "cat", "cache/y"); got != "more" {
		t.Errorf("a does not see b's write to the shared path: %q", got)
	}
}

func TestCheckpointRefusesSharedGit(t *testing.T) {
	m, _ := setupShared(t)
	ctx := context.Background()
	if _, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "demo", ID: "a", Backend: "copy"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Checkpoint(ctx, "a"); err == nil || !strings.Contains(err.Error(), "shares git") {
		t.Errorf("checkpoint with shared git: %v", err)
	}
}

func TestForkOfDirRepo(t *testing.T) { eachBackend(t, testForkOfDirRepo) }

func testForkOfDirRepo(t *testing.T) {
	if backend.Default == "overlay-ns" {
		t.Skip("covered by the fork test")
	}
	m, _ := setup(t)
	ctx := context.Background()
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "data.txt"), []byte("one\n"), 0o644)
	if _, _, err := m.AddDir(ctx, "files", src, repo.DirOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "files", ID: "a"}); err != nil {
		t.Fatal(err)
	}
	execIn(t, m, "a", "sh", "-c", "echo two >> data.txt && echo made > out")
	b, err := m.Fork(ctx, "a", manager.ForkOptions{ID: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if b.Branch != "" || !strings.HasPrefix(b.Base, "snap-") {
		t.Errorf("forked session of a dir repo: %+v", b)
	}
	if got := execIn(t, m, "b", "sh", "-c", "cat data.txt out"); got != "one\ntwo\nmade" {
		t.Errorf("b sees %q", got)
	}
	execIn(t, m, "b", "sh", "-c", "echo b > only-b")
	if got := execIn(t, m, "a", "sh", "-c", "test ! -e only-b && echo ok"); got != "ok" {
		t.Errorf("a sees b's file: %q", got)
	}
}

// Two checkpoints of one commit in one second get different bases.
func TestCheckpointsInOneSecondDoNotCollide(t *testing.T) {
	m, _ := setup(t)
	ctx := context.Background()
	if _, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "demo", ID: "a", Backend: "copy"}); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		base, err := m.Checkpoint(ctx, "a")
		if err != nil {
			t.Fatal(err)
		}
		if seen[base] {
			t.Fatalf("base %s made twice", base)
		}
		seen[base] = true
	}
}
