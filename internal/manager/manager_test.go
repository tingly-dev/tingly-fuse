package manager_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	_ "github.com/tingly-dev/tingly-fuse/internal/backend/fullcopy"
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
	if _, err := m.AddRepo(context.Background(), "demo", upstream); err != nil {
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

	a, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "demo", ID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "demo", ID: "b", Branch: "feature/b"})
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
		if err := m.Push(ctx, id, "", &out, &out); err != nil {
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
			_, errs[i] = m.Spawn(context.Background(), manager.SpawnOptions{Repo: "demo"})
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
	if _, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "demo", ID: "x", Branch: "bad..name"}); err == nil {
		t.Fatal("expected invalid branch name to fail")
	}
	if _, err := os.Stat(filepath.Join(m.Home, "sessions", "x")); !os.IsNotExist(err) {
		t.Errorf("failed spawn left state behind: %v", err)
	}
	if _, err := m.Spawn(ctx, manager.SpawnOptions{Repo: "demo", Backend: "nope"}); err == nil {
		t.Error("expected unknown backend to fail")
	}
}
