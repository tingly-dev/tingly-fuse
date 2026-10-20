package repo

// Shared paths (ADR 0011): directories or files of the tree that every
// session of the repository sees with the same content, live. Each one is
// kept once on the host, under shares/<path>, and each backend puts it at
// <path> in the view (overlay: a bind mount; copy and clone: a symlink).
// A share is seeded from the first base that is spawned with it; after
// that the base's copy of the path is ignored.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tingly-dev/tingly-fuse/internal/gitx"
)

// Share is a shared path with where it lives on the host.
type Share struct {
	Path    string // slash-separated, relative to the tree root
	Source  string // shares/<path>
	Tracked bool   // git tracks something at Path in the base
}

// ShareSource is where shared path p lives on the host.
func (r *Repo) ShareSource(p string) string {
	return filepath.Join(r.dir, "shares", filepath.FromSlash(p))
}

// cleanSharePath validates a path given to share and returns its canonical
// form.
func cleanSharePath(p string) (string, error) {
	c := path.Clean(filepath.ToSlash(p))
	switch {
	case p == "" || c == "." || path.IsAbs(c) || c == ".." || strings.HasPrefix(c, "../"):
		return "", fmt.Errorf("shared path %q must be relative and inside the tree", p)
	case c == ".git" || strings.HasPrefix(c, ".git/"):
		return "", fmt.Errorf("cannot share %q: sessions need their own HEAD and index; register the repository with repo add --shared-git to share branches and commits", p)
	}
	return c, nil
}

// SetShares adds (or with remove, removes) shared paths of repository
// name. Sessions keep the shares they were spawned with; the change applies
// to sessions spawned after it. Removing a path keeps its shared content
// under shares/ until the repository is removed.
func SetShares(home, name string, paths []string, remove bool) (*Repo, error) {
	r, err := Open(home, name)
	if err != nil {
		return nil, err
	}
	unlock, err := flock(filepath.Join(r.dir, ".repo.lock"), true)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if r, err = Open(home, name); err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, p := range r.SharedPaths {
		set[p] = true
	}
	for _, p := range paths {
		c, err := cleanSharePath(p)
		if err != nil {
			return nil, err
		}
		if remove && !set[c] {
			return nil, fmt.Errorf("%q is not shared", p)
		}
		set[c] = !remove
	}
	var out []string
	for p, on := range set {
		if on {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	for i := 1; i < len(out); i++ {
		if strings.HasPrefix(out[i], out[i-1]+"/") {
			return nil, fmt.Errorf("shared paths %s and %s overlap", out[i-1], out[i])
		}
	}
	r.SharedPaths = out
	return r, r.save()
}

func (r *Repo) save() error {
	data, _ := json.MarshalIndent(r, "", "  ")
	tmp := filepath.Join(r.dir, "repo.json.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(r.dir, "repo.json"))
}

// EnsureShares makes sure every path in paths has its shared copy, seeding
// a missing one from baseDir: a copy of the base's directory or file there,
// or an empty directory when the base has nothing at that path.
func (r *Repo) EnsureShares(ctx context.Context, baseDir string, paths []string) ([]Share, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	unlock, err := flock(filepath.Join(r.dir, ".shares.lock"), true)
	if err != nil {
		return nil, err
	}
	defer unlock()
	var out []Share
	for _, p := range paths {
		src := r.ShareSource(p)
		if _, err := os.Lstat(src); errors.Is(err, os.ErrNotExist) {
			if err := r.seedShare(ctx, baseDir, p, src); err != nil {
				return nil, fmt.Errorf("seed shared path %s: %w", p, err)
			}
		} else if err != nil {
			return nil, err
		}
		tracked, err := gitx.Run(ctx, baseDir, "ls-files", "--", p)
		if err != nil {
			return nil, err
		}
		out = append(out, Share{Path: p, Source: src, Tracked: tracked != ""})
	}
	return out, nil
}

func (r *Repo) seedShare(ctx context.Context, baseDir, p, src string) error {
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		return err
	}
	from := filepath.Join(baseDir, filepath.FromSlash(p))
	fi, err := os.Lstat(from)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return os.Mkdir(src, 0o755)
	case err != nil:
		return err
	case fi.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("it is a symlink in the base")
	}
	// Copy next to the destination and rename, so a failed copy never
	// leaves a half-seeded share that later spawns would take as complete.
	tmp, err := os.MkdirTemp(filepath.Dir(src), ".tmp-share-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := reflinkCopy(ctx, from, filepath.Join(tmp, "x")); err != nil {
		return err
	}
	return os.Rename(filepath.Join(tmp, "x"), src)
}
