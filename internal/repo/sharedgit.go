package repo

// Shared git (ADR 0010): with it on, every session of the repository is a
// linked worktree of one repository, shared.git. Refs, config and the
// objects sessions write live there, so a commit in one session is visible
// in all of them at once; each session keeps its own HEAD, index and
// submodule gitdirs in shared.git/worktrees/<id>, and its view's .git is a
// symlink to that directory.
//
// The worktree entries are what make this safe: git gc and the
// "branch already checked out" checks look at every entry under
// worktrees/, so one session's gc never collects another session's staged
// blobs. Each entry is locked, because under the overlay backend its path
// (the same /workspace/<repo> for every session) does not exist outside the
// session, and git worktree prune would otherwise delete it.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/tingly-dev/tingly-fuse/internal/gitx"
)

// sharedPrefix is where the mirror keeps a copy of shared.git's refs, so
// gc in the mirror never collects objects that only shared.git's refs reach
// (a branch upstream deleted that a session had fetched, for one).
const sharedPrefix = "refs/tingly/shared/"

// SharedDir is the repository every session shares when SharedGit is set.
func (r *Repo) SharedDir() string { return filepath.Join(r.dir, "shared.git") }

// SessionGitDir is session id's own part of shared.git.
func (r *Repo) SessionGitDir(id string) string {
	return filepath.Join(r.SharedDir(), "worktrees", id)
}

// initShared creates shared.git: no objects of its own beyond what sessions
// write, the mirror as alternates, origin at the real upstream.
func (r *Repo) initShared(ctx context.Context) error {
	dir := r.SharedDir()
	if _, err := gitx.Run(ctx, r.dir, "init", "--quiet", "--bare", dir); err != nil {
		return err
	}
	alt := filepath.Join(r.ObjectsDir(), "objects") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "objects", "info", "alternates"), []byte(alt), 0o644); err != nil {
		return err
	}
	for _, kv := range [][2]string{
		{"remote.origin.url", r.Upstream},
		{"remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"},
		// Bare repositories default to no reflogs; sessions want them.
		{"core.logAllRefUpdates", "true"},
		// Submodule settings differ per base, so they go in each session's
		// config.worktree, not in the shared config.
		{"core.repositoryFormatVersion", "1"},
		{"extensions.worktreeConfig", "true"},
	} {
		if _, err := gitx.Run(ctx, dir, "config", kv[0], kv[1]); err != nil {
			return err
		}
	}
	// With worktreeConfig on, a core.bare in the common config would apply
	// to every session too; it belongs to shared.git's own worktree config.
	if _, err := gitx.Run(ctx, dir, "config", "--unset", "core.bare"); err != nil {
		return err
	}
	_, err := gitx.Run(ctx, dir, "config", "--worktree", "core.bare", "true")
	return err
}

// AddSessionGitDir registers session id as a worktree of shared.git whose
// tree is at viewPath, starting from the base checkout baseDir: detached at
// the base's HEAD, with the base's index and submodule gitdirs. It returns
// the directory the view's .git must point to. The base's local refs, stash
// and config other than submodule settings are not carried over: refs and
// config come from shared.git.
func (r *Repo) AddSessionGitDir(ctx context.Context, id, baseDir, viewPath string) (_ string, err error) {
	wt := r.SessionGitDir(id)
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		return "", err
	}
	if err := os.Mkdir(wt, 0o755); err != nil {
		return "", fmt.Errorf("session git dir: %w", err)
	}
	defer func() {
		if err != nil {
			removeAll(wt)
		}
	}()
	head, err := gitx.Run(ctx, baseDir, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", err
	}
	src := filepath.Join(baseDir, ".git")
	for name, data := range map[string]string{
		"HEAD":      head,
		"commondir": r.SharedDir(),
		"gitdir":    filepath.Join(viewPath, ".git"),
		"locked":    "tingly-fuse session " + id,
	} {
		if err := os.WriteFile(filepath.Join(wt, name), []byte(data+"\n"), 0o644); err != nil {
			return "", err
		}
	}
	if err := copyFile(filepath.Join(src, "index"), filepath.Join(wt, "index")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.Chmod(filepath.Join(wt, "index"), 0o644); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(src, "modules")); err == nil {
		if err := reflinkCopy(ctx, filepath.Join(src, "modules"), filepath.Join(wt, "modules")); err != nil {
			return "", err
		}
		if err := rebaseModuleWorktrees(ctx, src, wt, baseDir, viewPath); err != nil {
			return "", err
		}
	}
	subs, err := gitx.Run(ctx, "/", "config", "--file", filepath.Join(src, "config"), "--null", "--get-regexp", `^submodule\.`)
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) { // 1: no submodules
		return "", err
	}
	for _, entry := range strings.Split(subs, "\x00") {
		key, val, ok := strings.Cut(entry, "\n")
		if !ok {
			continue
		}
		if _, err := gitx.Run(ctx, "/", "config", "--file", filepath.Join(wt, "config.worktree"), "--add", key, val); err != nil {
			return "", err
		}
	}
	return wt, nil
}

// rebaseModuleWorktrees points each copied submodule gitdir's core.worktree
// at the submodule in the view. The base's values are relative to the
// gitdir's place in the base; git resolves them from the copy in
// shared.git, where they lead nowhere.
func rebaseModuleWorktrees(ctx context.Context, srcGitDir, wt, baseDir, viewPath string) error {
	return filepath.WalkDir(filepath.Join(wt, "modules"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "config" {
			return err
		}
		gitdir := filepath.Dir(path)
		if _, err := os.Stat(filepath.Join(gitdir, "HEAD")); err != nil {
			return nil // not a gitdir, just a file named config
		}
		// Run from / with --file: inside the gitdir, git would first try to
		// enter the very core.worktree that is broken.
		val, _ := gitx.Run(ctx, "/", "config", "--file", path, "core.worktree")
		if val == "" || filepath.IsAbs(val) {
			return nil
		}
		rel, err := filepath.Rel(wt, gitdir)
		if err != nil {
			return err
		}
		inBase, err := filepath.Rel(baseDir, filepath.Join(srcGitDir, rel, val))
		if err != nil || inBase == ".." || strings.HasPrefix(inBase, ".."+string(filepath.Separator)) {
			return fmt.Errorf("submodule gitdir %s: core.worktree %q is outside the base", rel, val)
		}
		_, err = gitx.Run(ctx, "/", "config", "--file", path, "core.worktree", filepath.Join(viewPath, inBase))
		return err
	})
}

// RemoveSessionGitDir deletes session id's worktree entry. Its branch and
// commits stay in shared.git, where every other session still sees them.
func (r *Repo) RemoveSessionGitDir(id string) error {
	if id == "" || strings.ContainsAny(id, `/\`) || strings.HasPrefix(id, ".") {
		return fmt.Errorf("invalid session id %q", id)
	}
	return removeAll(r.SessionGitDir(id))
}

// syncSharedRefs copies shared.git's refs into the mirror under
// refs/tingly/shared/, with the few objects only shared.git has. Run before
// every fetch into the mirror, whose auto gc would otherwise count only
// upstream's refs and the base pins.
func (r *Repo) syncSharedRefs(ctx context.Context) error {
	return withMirrorLock(r.ObjectsDir(), func() error {
		_, err := gitx.Run(ctx, r.ObjectsDir(), "fetch", "--quiet", "--prune", "--no-auto-gc", r.SharedDir(), "+refs/*:"+sharedPrefix+"*")
		return err
	})
}
