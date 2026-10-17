package repo

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/tingly-dev/tingly-fuse/internal/gitx"
)

// SnapshotDir turns an already prepared checkout (dependencies installed,
// build outputs present) into a base and returns its id and HEAD commit.
//
// The directory is copied with reflinks where the filesystem has them, so the
// source can keep changing afterwards without touching the base. Objects the
// copy holds (including commits and blobs that were never pushed) move into
// the shared mirrors and the copy's .git is rewritten to use alternates, the
// same shape EnsureBase produces. Contents are taken as they are, uncommitted
// changes included.
func (r *Repo) SnapshotDir(ctx context.Context, src string) (id, head string, err error) {
	src, err = filepath.Abs(src)
	if err != nil {
		return "", "", err
	}
	if fi, err := os.Stat(filepath.Join(src, ".git")); err != nil || !fi.IsDir() {
		return "", "", fmt.Errorf("%s: .git must be a directory (a plain clone, not a worktree)", src)
	}
	head, err = gitx.Run(ctx, src, "rev-parse", "HEAD")
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", src, err)
	}
	id = fmt.Sprintf("dir-%s-%s", head[:12], time.Now().UTC().Format("20060102T150405"))

	bases := filepath.Join(r.dir, "bases")
	if err := os.MkdirAll(bases, 0o755); err != nil {
		return "", "", err
	}
	tmp, err := os.MkdirTemp(bases, ".tmp-"+id+"-")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(tmp)
	work := filepath.Join(tmp, "tree")
	if err := reflinkCopy(ctx, src, work); err != nil {
		return "", "", fmt.Errorf("snapshot %s: %w", src, err)
	}
	if err := r.absorbGitDir(ctx, filepath.Join(work, ".git"), r.ObjectsDir()); err != nil {
		return "", "", err
	}
	if err := r.absorbModules(ctx, filepath.Join(work, ".git", "modules")); err != nil {
		return "", "", err
	}
	if _, err := gitx.Run(ctx, work, "remote", "set-url", "origin", r.Upstream); err != nil {
		// no origin in the source: add one so push works
		if _, err := gitx.Run(ctx, work, "remote", "add", "origin", r.Upstream); err != nil {
			return "", "", err
		}
	}
	if err := os.Rename(work, r.BaseDir(id)); err != nil {
		return "", "", err
	}
	return id, head, nil
}

func reflinkCopy(ctx context.Context, src, dst string) error {
	args := []string{"-a", "--reflink=auto", src, dst}
	if runtime.GOOS == "darwin" {
		args = []string{"-cRp", src, dst}
	}
	if out, err := exec.CommandContext(ctx, "cp", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("cp: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// absorbModules handles the gitdirs of submodules under .git/modules: each
// one's objects go to the per-URL module mirror.
func (r *Repo) absorbModules(ctx context.Context, modules string) error {
	if _, err := os.Stat(modules); err != nil {
		return nil
	}
	var dirs []string
	err := filepath.WalkDir(modules, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		if _, e := os.Stat(filepath.Join(p, "HEAD")); e == nil {
			if _, e := os.Stat(filepath.Join(p, "objects")); e == nil {
				dirs = append(dirs, p)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, gd := range dirs {
		url, err := gitx.Run(ctx, gd, "config", "--file", filepath.Join(gd, "config"), "remote.origin.url")
		if err != nil || url == "" {
			continue // no way to name a mirror; leave its objects where they are
		}
		mirror, err := r.ensureEmptyModuleMirror(ctx, url)
		if err != nil {
			return err
		}
		if err := r.absorbGitDir(ctx, gd, mirror); err != nil {
			return err
		}
	}
	return nil
}

// ensureEmptyModuleMirror creates a module mirror without cloning, so the
// objects found in a prepared directory seed it. ensureModuleMirror later
// fetches into it with the same refspec a --mirror clone would have used.
func (r *Repo) ensureEmptyModuleMirror(ctx context.Context, url string) (string, error) {
	mirror := r.moduleMirror(url)
	if _, err := os.Stat(mirror); err == nil {
		return mirror, nil
	}
	if err := os.MkdirAll(r.ModulesDir(), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(r.ModulesDir(), ".tmp-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	m := filepath.Join(tmp, "m.git")
	for _, args := range [][]string{
		{"init", "--quiet", "--bare", m},
		{"-C", m, "remote", "add", "origin", url},
		{"-C", m, "config", "remote.origin.fetch", "+refs/*:refs/*"},
	} {
		if _, err := gitx.Run(ctx, tmp, args...); err != nil {
			return "", err
		}
	}
	if err := os.Rename(m, mirror); err != nil {
		if _, statErr := os.Stat(mirror); statErr != nil {
			return "", err
		}
	}
	return mirror, nil
}

// absorbGitDir moves the objects of gitdir, and of every repository its
// alternates chain reaches, into the bare repository mirror, then points
// gitdir at mirror through alternates and leaves it with no objects of its own.
func (r *Repo) absorbGitDir(ctx context.Context, gitdir, mirror string) error {
	own := filepath.Join(gitdir, "objects")
	dest := filepath.Join(mirror, "objects")

	var sources []string
	seen := map[string]bool{}
	var walk func(objects string, external bool) error
	walk = func(objects string, external bool) error {
		if seen[objects] {
			return nil
		}
		seen[objects] = true
		sources = append(sources, objects)
		data, err := os.ReadFile(filepath.Join(objects, "info", "alternates"))
		if os.IsNotExist(err) {
			return nil
		} else if err != nil {
			return err
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if !filepath.IsAbs(line) {
				line = filepath.Join(objects, line)
			}
			if filepath.Clean(line) == filepath.Clean(dest) {
				continue
			}
			if err := walk(filepath.Clean(line), true); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(own, false); err != nil {
		return err
	}
	for i, objects := range sources {
		// The first source is the copy itself and can be moved; the rest
		// belong to someone else and are only read.
		if err := mergeObjects(objects, dest, i == 0); err != nil {
			return fmt.Errorf("absorb objects from %s: %w", objects, err)
		}
	}
	// The copy's own objects now live in the mirror; remove what is left.
	entries, err := os.ReadDir(own)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(own, e.Name())); err != nil {
			return err
		}
	}
	for _, d := range []string{"info", "pack"} {
		if err := os.MkdirAll(filepath.Join(own, d), 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(own, "info", "alternates"), []byte(dest+"\n"), 0o644)
}

// mergeObjects copies loose objects and packs from one objects directory
// into another, skipping what already exists. Packs go in before their
// indexes so a reader never sees an index without its pack. With move set,
// files are renamed (falling back to copying across filesystems).
func mergeObjects(from, to string, move bool) error {
	place := func(src, dst string) error {
		if _, err := os.Lstat(dst); err == nil {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if move {
			if err := os.Rename(src, dst); err == nil {
				return nil
			}
		}
		return copyFile(src, dst)
	}
	entries, err := os.ReadDir(from)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() && len(name) == 2 {
			files, err := os.ReadDir(filepath.Join(from, name))
			if err != nil {
				return err
			}
			for _, f := range files {
				if err := place(filepath.Join(from, name, f.Name()), filepath.Join(to, name, f.Name())); err != nil {
					return err
				}
			}
		}
	}
	packs, err := os.ReadDir(filepath.Join(from, "pack"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var idx []string
	for _, p := range packs {
		n := p.Name()
		switch {
		case strings.HasSuffix(n, ".idx"):
			idx = append(idx, n)
		case strings.HasSuffix(n, ".pack"), strings.HasSuffix(n, ".rev"), strings.HasSuffix(n, ".bitmap"), strings.HasSuffix(n, ".promisor"):
			if err := place(filepath.Join(from, "pack", n), filepath.Join(to, "pack", n)); err != nil {
				return err
			}
		}
	}
	for _, n := range idx {
		if err := place(filepath.Join(from, "pack", n), filepath.Join(to, "pack", n)); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o444)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// ReadHead returns the commit a base checkout is at.
func (r *Repo) ReadHead(ctx context.Context, base string) (string, error) {
	return gitx.Run(ctx, r.BaseDir(base), "rev-parse", "HEAD")
}
