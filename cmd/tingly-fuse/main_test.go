package main

// CLI harness: runs this test binary as the tingly-fuse command, so flag
// parsing, argument handling, exit codes and output are covered end to end.
// The manager tests cover the behaviour behind each command.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const envRunMain = "TINGLY_FUSE_TEST_RUN_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(envRunMain) == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type cli struct {
	t   *testing.T
	env []string
}

// run executes tingly-fuse with args and returns its combined output and
// exit code.
func (c cli) run(args ...string) (string, int) {
	c.t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), append(c.env, envRunMain+"=1")...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		c.t.Fatalf("tingly-fuse %v: %v", args, err)
	}
	return strings.TrimSpace(string(out)), code
}

// ok runs tingly-fuse and fails the test unless it exits 0.
func (c cli) ok(args ...string) string {
	c.t.Helper()
	out, code := c.run(args...)
	if code != 0 {
		c.t.Fatalf("tingly-fuse %v: exit %d\n%s", args, code, out)
	}
	return out
}

// fails runs tingly-fuse and fails the test unless it exits with want and
// prints something containing each of msgs.
func (c cli) fails(want int, msgs []string, args ...string) {
	c.t.Helper()
	out, code := c.run(args...)
	if code != want {
		c.t.Errorf("tingly-fuse %v: exit %d, want %d\n%s", args, code, want, out)
	}
	for _, m := range msgs {
		if !strings.Contains(out, m) {
			c.t.Errorf("tingly-fuse %v: output lacks %q\n%s", args, m, out)
		}
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

// Sessions use the copy backend so the test runs anywhere; the manager tests
// cover the other backends.
func TestCommands(t *testing.T) {
	tmp := t.TempDir()
	c := cli{t: t, env: []string{
		"TINGLY_FUSE_HOME=" + filepath.Join(tmp, "home"),
		"TINGLY_FUSE_VIEW_ROOT=" + filepath.Join(tmp, "workspace"),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	}}
	for _, kv := range c.env {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	src := filepath.Join(tmp, "src")
	git(t, tmp, "init", "--quiet", "--initial-branch=main", src)
	os.WriteFile(filepath.Join(src, "README.md"), []byte("hello\n"), 0o644)
	git(t, src, "add", ".")
	git(t, src, "commit", "--quiet", "-m", "init")
	upstream := filepath.Join(tmp, "upstream.git")
	git(t, tmp, "clone", "--quiet", "--bare", src, upstream)

	// Usage errors: every command says how to call it.
	c.fails(2, []string{"usage:"})
	c.fails(1, []string{"usage:"}, "nope")
	c.fails(1, []string{"usage: tingly-fuse repo"}, "repo")
	c.fails(1, []string{"usage: tingly-fuse base"}, "base")
	c.fails(1, []string{"usage: tingly-fuse base"}, "base", "rm", "demo")
	c.fails(1, []string{"usage: tingly-fuse spawn"}, "spawn")
	c.fails(1, []string{"usage: tingly-fuse exec"}, "exec", "x")
	c.fails(1, []string{"usage: tingly-fuse drop"}, "drop")
	c.fails(1, []string{"not registered"}, "spawn", "demo")
	if out := c.ok("help"); !strings.Contains(out, "base prune") || !strings.Contains(out, "repo check") {
		t.Errorf("help lacks the base and check commands:\n%s", out)
	}

	c.ok("repo", "add", "demo", upstream)
	c.fails(1, []string{"already registered"}, "repo", "add", "demo", upstream)
	if out := c.ok("repo", "list", "-v"); !strings.Contains(out, "demo") || !strings.Contains(out, "SESSIONS") {
		t.Errorf("repo list -v:\n%s", out)
	}
	c.ok("repo", "fetch", "demo")
	c.fails(1, []string{"usage: tingly-fuse repo add"}, "repo", "add", "shared", upstream, "extra")
	c.ok("repo", "add", "shared", upstream, "--shared-git")
	if out := c.ok("repo", "list", "-v"); !strings.Contains(out, "shared") || !strings.Contains(out, "private") {
		t.Errorf("repo list -v does not tell shared and private git apart:\n%s", out)
	}
	c.ok("spawn", "shared", "--id", "s1", "--backend", "copy")
	c.ok("spawn", "shared", "--id", "s2", "--backend", "copy")
	c.ok("exec", "s1", "--", "git", "commit", "--quiet", "--allow-empty", "-m", "from-s1")
	if out := c.ok("exec", "s2", "--", "git", "log", "-1", "--format=%s", "agent/s1"); strings.TrimSpace(out) != "from-s1" {
		t.Errorf("s2 does not see s1's branch: %q", out)
	}
	c.ok("drop", "s1")
	c.ok("drop", "s2")
	c.ok("repo", "remove", "shared")

	c.fails(1, []string{"--shared-git"}, "repo", "share", "demo", ".git")
	c.fails(1, []string{"usage: tingly-fuse repo unshare"}, "repo", "unshare", "demo")
	if out := c.ok("repo", "share", "demo", "cache", "tmp"); out != "cache\ntmp" {
		t.Errorf("repo share output: %q", out)
	}
	if out := c.ok("repo", "unshare", "demo", "tmp"); out != "cache" {
		t.Errorf("repo unshare output: %q", out)
	}
	c.ok("spawn", "demo", "--id", "c1", "--backend", "copy")
	c.ok("spawn", "demo", "--id", "c2", "--backend", "copy")
	c.ok("exec", "c1", "--", "sh", "-c", "echo hi > cache/f")
	if out := c.ok("exec", "c2", "--", "cat", "cache/f"); strings.TrimSpace(out) != "hi" {
		t.Errorf("c2 does not see c1's shared cache: %q", out)
	}
	c.ok("drop", "c1")
	c.ok("drop", "c2")
	c.ok("repo", "unshare", "demo", "cache")
	if out := c.ok("repo", "share", "demo"); out != "" {
		t.Errorf("shares left: %q", out)
	}

	// Shares set at add (repeatable and comma-separated) and per spawn.
	c.ok("repo", "add", "multi", upstream, "--share", "cache,out", "--share", "logs")
	if out := c.ok("repo", "share", "multi"); out != "cache\nlogs\nout" {
		t.Errorf("shares from repo add: %q", out)
	}
	c.fails(1, []string{"overlap"}, "spawn", "multi", "--id", "m0", "--backend", "copy", "--share", "cache/x")
	c.ok("spawn", "multi", "--id", "m1", "--backend", "copy", "--share", "tmp")
	c.ok("spawn", "multi", "--id", "m2", "--backend", "copy", "--share", "tmp")
	c.ok("exec", "m1", "--", "sh", "-c", "echo t > tmp/t && echo l > logs/l")
	if out := c.ok("exec", "m2", "--", "cat", "tmp/t", "logs/l"); strings.TrimSpace(out) != "t\nl" {
		t.Errorf("m2 sees %q", out)
	}
	c.ok("repo", "remove", "multi", "--force")

	if out := c.ok("spawn", "demo", "--id", "a", "--branch", "feature/a", "--backend", "copy"); !strings.HasPrefix(out, "a\n") || !strings.Contains(out, "branch: feature/a") {
		t.Errorf("spawn output:\n%s", out)
	}
	c.ok("spawn", "demo", "--id", "b", "--from", "main", "--no-fetch", "--backend", "copy")
	c.fails(1, []string{"mutually exclusive"}, "spawn", "demo", "--from", "main", "--base", "x")
	c.fails(1, []string{"no base"}, "spawn", "demo", "--base", "nope")

	// exec passes stdout through and propagates the exit code.
	if out := c.ok("exec", "a", "--", "sh", "-c", `echo "$TINGLY_SESSION"; cat README.md`); out != "a\nhello" {
		t.Errorf("exec output = %q", out)
	}
	c.fails(7, nil, "exec", "a", "--", "sh", "-c", "exit 7")
	c.ok("exec", "a", "--", "sh", "-c", "echo a > a.txt && git add a.txt && git commit -qm a")
	c.ok("push", "a")
	if got := git(t, upstream, "branch", "--list", "feature/a", "--format=%(refname:short)"); got != "feature/a" {
		t.Errorf("pushed branches = %q", got)
	}

	// A prepared directory becomes a second base.
	prep := filepath.Join(tmp, "prep")
	git(t, tmp, "clone", "--quiet", upstream, prep)
	os.WriteFile(filepath.Join(prep, "prepared.txt"), []byte("prepared\n"), 0o644)
	out := c.ok("spawn", "demo", "--id", "d", "--from-dir", prep, "--backend", "copy")
	var dirBase string
	for _, line := range strings.Split(out, "\n") {
		if b, ok := strings.CutPrefix(line, "base: "); ok {
			dirBase = b
		}
	}
	if !strings.HasPrefix(dirBase, "dir-") {
		t.Fatalf("spawn --from-dir output:\n%s", out)
	}
	c.ok("spawn", "demo", "--id", "e", "--base", dirBase, "--backend", "copy")
	if out := c.ok("list"); strings.Count(out, "\n") != 4 {
		t.Errorf("list:\n%s", out)
	}
	if out := c.ok("base", "list", "demo"); !strings.Contains(out, dirBase) || !strings.Contains(out, "d,e") {
		t.Errorf("base list:\n%s", out)
	}
	if out := c.ok("repo", "check", "demo"); strings.Count(out, "ok") != 2 {
		t.Errorf("repo check:\n%s", out)
	}

	// Bases in use are kept; unused ones go.
	c.fails(1, []string{"drop them first"}, "base", "rm", "demo", dirBase)
	c.ok("drop", "d")
	c.ok("drop", "e")
	if out := c.ok("base", "prune", "demo", "--dry-run"); out != dirBase {
		t.Errorf("prune --dry-run = %q, want %s", out, dirBase)
	}
	if out := c.ok("base", "prune", "demo"); out != dirBase {
		t.Errorf("prune = %q", out)
	}
	if out := c.ok("base", "list", "demo"); strings.Contains(out, dirBase) {
		t.Errorf("pruned base still listed:\n%s", out)
	}

	// An unpinned base is a failure for check and repaired by --fix.
	mirror := filepath.Join(tmp, "home", "repos", "demo", "objects.git")
	for _, ref := range strings.Fields(git(t, mirror, "for-each-ref", "--format=%(refname)", "refs/tingly/")) {
		git(t, mirror, "update-ref", "-d", ref)
	}
	c.fails(1, []string{"false", "problem(s) found"}, "repo", "check", "demo")
	c.ok("repo", "check", "demo", "--fix")
	c.ok("repo", "check", "demo")

	c.fails(1, []string{"still has sessions"}, "repo", "remove", "demo")
	c.ok("repo", "remove", "demo", "--force")
	if out := c.ok("list"); strings.Contains(out, "\n") {
		t.Errorf("sessions survived repo remove --force:\n%s", out)
	}
}
