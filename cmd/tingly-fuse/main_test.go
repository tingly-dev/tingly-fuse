package main

// CLI harness: runs this test binary as the tingly-fuse command, so flag
// parsing, argument handling, exit codes and output are covered end to end.
// The manager tests cover the behaviour behind each command.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	c.fails(1, []string{"git add", "base list"}, "repo")
	c.fails(1, []string{"git push"}, "push", "x")
	c.fails(1, []string{"usage: tingly-fuse git"}, "git")
	c.fails(1, []string{"usage: tingly-fuse git spawn"}, "git", "spawn")
	c.fails(1, []string{"usage: tingly-fuse share"}, "share")
	c.fails(1, []string{"usage: tingly-fuse remove"}, "remove")
	c.fails(1, []string{"usage: tingly-fuse base"}, "base")
	c.fails(1, []string{"usage: tingly-fuse base"}, "base", "rm", "demo")
	c.fails(1, []string{"usage: tingly-fuse spawn"}, "spawn")
	c.fails(1, []string{"usage: tingly-fuse exec"}, "exec", "x")
	c.fails(1, []string{"usage: tingly-fuse drop"}, "drop")
	c.fails(1, []string{"not registered"}, "spawn", "demo")
	c.fails(1, []string{"not registered"}, "git", "spawn", "demo")
	if out := c.ok("help"); !strings.Contains(out, "base prune") || !strings.Contains(out, "git check") {
		t.Errorf("help lacks the base and check commands:\n%s", out)
	}

	c.ok("git", "add", "demo", upstream)
	c.fails(1, []string{"already registered"}, "git", "add", "demo", upstream)
	if out := c.ok("base", "list"); !strings.Contains(out, "demo") || !strings.Contains(out, "SESSIONS") {
		t.Errorf("base list:\n%s", out)
	}
	c.ok("git", "fetch", "demo")
	c.fails(1, []string{"usage: tingly-fuse git add"}, "git", "add", "shared", upstream, "extra")
	c.ok("git", "add", "shared", upstream, "--shared-git")
	if out := c.ok("base", "list"); !strings.Contains(out, "git (shared)") || !regexp.MustCompile(`demo +git +`).MatchString(out) {
		t.Errorf("base list does not tell shared and private git apart:\n%s", out)
	}
	c.ok("git", "spawn", "shared", "--id", "s1", "--backend", "copy")
	c.ok("git", "spawn", "shared", "--id", "s2", "--backend", "copy")
	c.ok("exec", "s1", "--", "git", "commit", "--quiet", "--allow-empty", "-m", "from-s1")
	if out := c.ok("exec", "s2", "--", "git", "log", "-1", "--format=%s", "agent/s1"); strings.TrimSpace(out) != "from-s1" {
		t.Errorf("s2 does not see s1's branch: %q", out)
	}
	c.ok("drop", "s1")
	c.ok("drop", "s2")
	c.ok("remove", "shared")

	c.fails(1, []string{"--shared-git"}, "share", "demo", ".git")
	c.fails(1, []string{"usage: tingly-fuse unshare"}, "unshare", "demo")
	if out := c.ok("share", "demo", "cache", "tmp"); out != "cache\ntmp" {
		t.Errorf("share output: %q", out)
	}
	if out := c.ok("unshare", "demo", "tmp"); out != "cache" {
		t.Errorf("unshare output: %q", out)
	}
	c.ok("git", "spawn", "demo", "--id", "c1", "--backend", "copy")
	c.ok("git", "spawn", "demo", "--id", "c2", "--backend", "copy")
	c.ok("exec", "c1", "--", "sh", "-c", "echo hi > cache/f")
	if out := c.ok("exec", "c2", "--", "cat", "cache/f"); strings.TrimSpace(out) != "hi" {
		t.Errorf("c2 does not see c1's shared cache: %q", out)
	}
	c.ok("drop", "c1")
	c.ok("drop", "c2")
	c.ok("unshare", "demo", "cache")
	if out := c.ok("share", "demo"); out != "" {
		t.Errorf("shares left: %q", out)
	}

	// Shares set at add (repeatable and comma-separated) and per spawn.
	c.ok("git", "add", "multi", upstream, "--share", "cache,out", "--share", "logs")
	if out := c.ok("share", "multi"); out != "cache\nlogs\nout" {
		t.Errorf("shares from git add: %q", out)
	}
	c.fails(1, []string{"overlap"}, "git", "spawn", "multi", "--id", "m0", "--backend", "copy", "--share", "cache/x")
	c.ok("git", "spawn", "multi", "--id", "m1", "--backend", "copy", "--share", "tmp")
	c.ok("git", "spawn", "multi", "--id", "m2", "--backend", "copy", "--share", "tmp")
	c.ok("exec", "m1", "--", "sh", "-c", "echo t > tmp/t && echo l > logs/l")
	if out := c.ok("exec", "m2", "--", "cat", "tmp/t", "logs/l"); strings.TrimSpace(out) != "t\nl" {
		t.Errorf("m2 sees %q", out)
	}
	c.ok("remove", "multi", "--force")

	if out := c.ok("git", "spawn", "demo", "--id", "a", "--branch", "feature/a", "--backend", "copy"); !strings.HasPrefix(out, "a\n") || !strings.Contains(out, "branch: feature/a") {
		t.Errorf("spawn output:\n%s", out)
	}
	c.ok("git", "spawn", "demo", "--id", "b", "--from", "main", "--no-fetch", "--backend", "copy")
	c.fails(1, []string{"mutually exclusive"}, "git", "spawn", "demo", "--from", "main", "--base", "x")
	c.fails(1, []string{"no base"}, "git", "spawn", "demo", "--base", "nope")

	// exec passes stdout through and propagates the exit code.
	if out := c.ok("exec", "a", "--", "sh", "-c", `echo "$TINGLY_SESSION"; cat README.md`); out != "a\nhello" {
		t.Errorf("exec output = %q", out)
	}
	c.fails(7, nil, "exec", "a", "--", "sh", "-c", "exit 7")
	c.ok("exec", "a", "--", "sh", "-c", "echo a > a.txt && git add a.txt && git commit -qm a")
	c.ok("git", "push", "a")
	if got := git(t, upstream, "branch", "--list", "feature/a", "--format=%(refname:short)"); got != "feature/a" {
		t.Errorf("pushed branches = %q", got)
	}

	// A prepared directory becomes a second base.
	prep := filepath.Join(tmp, "prep")
	git(t, tmp, "clone", "--quiet", upstream, prep)
	os.WriteFile(filepath.Join(prep, "prepared.txt"), []byte("prepared\n"), 0o644)
	out := c.ok("git", "spawn", "demo", "--id", "d", "--from-dir", prep, "--backend", "copy")
	var dirBase string
	for _, line := range strings.Split(out, "\n") {
		if b, ok := strings.CutPrefix(line, "base: "); ok {
			dirBase = b
		}
	}
	if !strings.HasPrefix(dirBase, "dir-") {
		t.Fatalf("spawn --from-dir output:\n%s", out)
	}
	c.ok("git", "spawn", "demo", "--id", "e", "--base", dirBase, "--backend", "copy")
	if out := c.ok("list"); strings.Count(out, "\n") != 4 {
		t.Errorf("list:\n%s", out)
	}
	if out := c.ok("base", "list", "demo"); !strings.Contains(out, dirBase) || !strings.Contains(out, "d,e") {
		t.Errorf("base list:\n%s", out)
	}
	if out := c.ok("git", "check", "demo"); strings.Count(out, "ok") != 2 {
		t.Errorf("git check:\n%s", out)
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
	c.fails(1, []string{"false", "problem(s) found"}, "git", "check", "demo")
	c.ok("git", "check", "demo", "--fix")
	c.ok("git", "check", "demo")

	// Any directory: snapshot, spawn, share; git commands refuse it.
	data := filepath.Join(tmp, "data")
	os.MkdirAll(filepath.Join(data, "cache"), 0o755)
	os.WriteFile(filepath.Join(data, "f"), []byte("v1\n"), 0o644)
	c.fails(1, []string{"usage: tingly-fuse base add"}, "base", "add", "data", data, "extra")
	first := c.ok("base", "add", "data", data, "--share", "cache")
	if !strings.HasPrefix(first, "snap-") {
		t.Fatalf("base add printed %q", first)
	}
	c.fails(1, []string{"git spawn"}, "spawn", "demo")
	c.fails(1, []string{"--from-dir"}, "base", "add", "demo", data)
	c.fails(1, []string{"not a git repository"}, "git", "spawn", "data")
	c.fails(1, []string{"not a git repository"}, "git", "check", "data")
	if out := c.ok("spawn", "data", "--id", "p1", "--backend", "copy"); !strings.HasPrefix(out, "p1\n") || strings.Contains(out, "branch:") || !strings.Contains(out, "base: "+first) {
		t.Errorf("spawn output:\n%s", out)
	}
	c.fails(1, []string{"not a git repository"}, "git", "push", "p1")
	c.fails(1, []string{"usage: tingly-fuse spawn"}, "spawn", "data", "extra")
	os.WriteFile(filepath.Join(data, "f"), []byte("v2\n"), 0o644)
	second := c.ok("base", "add", "data", data)
	c.ok("spawn", "data", "--id", "p2", "--backend", "copy")
	c.ok("spawn", "data", "--id", "p3", "--backend", "copy", "--base", first)
	c.ok("exec", "p1", "--", "sh", "-c", "echo c > cache/c")
	if out := c.ok("exec", "p2", "--", "cat", "f", "cache/c"); out != "v2\nc" {
		t.Errorf("p2 sees %q", out)
	}
	if out := c.ok("exec", "p3", "--", "cat", "f"); out != "v1" {
		t.Errorf("p3 sees %q", out)
	}
	if out := c.ok("base", "list"); !regexp.MustCompile(`data +dir +3 +2 +` + regexp.QuoteMeta(data)).MatchString(out) {
		t.Errorf("base list:\n%s", out)
	}
	if out := c.ok("base", "list", "data"); !regexp.MustCompile(second+` +\S+ +true +p2`).MatchString(out) || !regexp.MustCompile(first+` +\S+ +false +p1,p3`).MatchString(out) {
		t.Errorf("base list data:\n%s", out)
	}
	if out := c.ok("list"); !regexp.MustCompile(`p1 +data +- +`).MatchString(out) {
		t.Errorf("list:\n%s", out)
	}
	for _, id := range []string{"p1", "p2", "p3"} {
		c.ok("drop", id)
	}
	if out := c.ok("base", "prune", "data"); out != first {
		t.Errorf("prune data = %q, want only %s", out, first)
	}
	c.fails(1, []string{"base add first"}, "base", "rm", "data", second)
	c.ok("remove", "data")

	c.fails(1, []string{"still has sessions"}, "remove", "demo")
	c.ok("remove", "demo", "--force")
	if out := c.ok("list"); strings.Contains(out, "\n") {
		t.Errorf("sessions survived remove --force:\n%s", out)
	}
}
