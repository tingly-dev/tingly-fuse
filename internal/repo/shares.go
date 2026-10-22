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
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
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
		return "", fmt.Errorf("cannot share %q: sessions need their own HEAD and index; register a git repository with git clone --shared-git to share branches and commits", p)
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
	var out []string
	if remove {
		drop := map[string]bool{}
		for _, p := range paths {
			c, err := cleanSharePath(p)
			if err != nil {
				return nil, err
			}
			if !slices.Contains(r.SharedPaths, c) {
				return nil, fmt.Errorf("%q is not shared", p)
			}
			drop[c] = true
		}
		for _, p := range r.SharedPaths {
			if !drop[p] {
				out = append(out, p)
			}
		}
	} else if out, err = MergeSharePaths(r.SharedPaths, paths); err != nil {
		return nil, err
	}
	r.SharedPaths = out
	return r, r.save()
}

// MergeSharePaths validates paths and returns them together with have,
// sorted and without duplicates. Two paths where one is inside the other
// are refused: a bind mount or symlink for the inner one would land in the
// outer one's shared copy.
func MergeSharePaths(have, paths []string) ([]string, error) {
	set := map[string]bool{}
	for _, p := range have {
		set[p] = true
	}
	for _, p := range paths {
		c, err := cleanSharePath(p)
		if err != nil {
			return nil, err
		}
		set[c] = true
	}
	out := slices.Sorted(maps.Keys(set))
	for i := 1; i < len(out); i++ {
		if strings.HasPrefix(out[i], out[i-1]+"/") {
			return nil, fmt.Errorf("shared paths %s and %s overlap", out[i-1], out[i])
		}
	}
	return out, nil
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
//
// Shared copies nest on disk (shares/a/b is inside shares/a), so a path
// is refused when it is inside, or contains, a path that already has a
// shared copy, even one no longer shared: they would see each other's
// content.
func (r *Repo) EnsureShares(ctx context.Context, baseDir string, paths []string) ([]Share, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	roots, err := r.shareRoots()
	if err != nil {
		return nil, err
	}
	if err := checkRoots(roots, paths); err != nil {
		return nil, err
	}
	var missing []string
	for _, p := range paths {
		if !slices.Contains(roots, p) {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		unlock, err := flock(filepath.Join(r.dir, ".shares.lock"), true)
		if err != nil {
			return nil, err
		}
		defer unlock()
		if roots, err = r.shareRoots(); err != nil {
			return nil, err
		}
		if err := checkRoots(roots, paths); err != nil {
			return nil, err
		}
		for _, p := range missing {
			if slices.Contains(roots, p) {
				continue // seeded by a spawn we waited for
			}
			if err := r.seedShare(ctx, baseDir, p, r.ShareSource(p)); err != nil {
				return nil, fmt.Errorf("seed shared path %s: %w", p, err)
			}
			roots = append(roots, p)
			if err := r.saveShareRoots(roots); err != nil {
				return nil, err
			}
		}
	}
	// A dir repository's base is not interpreted, so nothing in it counts
	// as tracked, even when it happens to hold a .git.
	var tracked []string
	if r.IsGit() {
		// Literal pathspecs: a shared path is a name, not a pattern.
		files, err := gitx.Run(ctx, baseDir, "--literal-pathspecs", "ls-files", "-z", "--")
		if err != nil {
			return nil, err
		}
		tracked = strings.Split(files, "\x00")
	}
	var out []Share
	for _, p := range paths {
		s := Share{Path: p, Source: r.ShareSource(p)}
		s.Tracked = slices.ContainsFunc(tracked, func(f string) bool { return f == p || strings.HasPrefix(f, p+"/") })
		out = append(out, s)
	}
	return out, nil
}

// checkRoots refuses paths nested in, or containing, an existing copy.
func checkRoots(roots, paths []string) error {
	for _, p := range paths {
		for _, q := range roots {
			if strings.HasPrefix(p, q+"/") || strings.HasPrefix(q, p+"/") {
				return fmt.Errorf("shared path %s overlaps %s, which already has a shared copy under %s", p, q, "shares/")
			}
		}
	}
	return nil
}

// shareRoots lists the paths that have a shared copy, seeded or not still
// shared, from shares/index.json.
func (r *Repo) shareRoots() ([]string, error) {
	data, err := os.ReadFile(filepath.Join(r.dir, "shares", "index.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var roots []string
	if err := json.Unmarshal(data, &roots); err != nil {
		return nil, fmt.Errorf("read shares/index.json: %w", err)
	}
	return roots, nil
}

func (r *Repo) saveShareRoots(roots []string) error {
	data, _ := json.MarshalIndent(roots, "", "  ")
	tmp := filepath.Join(r.dir, "shares", "index.json.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(r.dir, "shares", "index.json"))
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
