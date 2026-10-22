package repo

// Plain directories (ADR 0013): a dir repository is any directory, git or
// not. Each base is a reflink snapshot of it, taken as it is; nothing in it
// is interpreted, a .git included.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DirOptions are the settings a dir repository is registered with.
type DirOptions struct {
	SharedPaths []string // see SetShares
}

// AddDir snapshots directory src as a base of dir repository name and makes
// it the base new sessions start from, registering name first if it is not
// registered yet. Sessions already spawned keep their base. It returns the
// repository and the new base's id.
func AddDir(ctx context.Context, home, name, src string, opts DirOptions) (*Repo, string, error) {
	if !validName.MatchString(name) {
		return nil, "", fmt.Errorf("invalid repo name %q", name)
	}
	src, err := filepath.Abs(src)
	if err != nil {
		return nil, "", err
	}
	if fi, err := os.Stat(src); err != nil {
		return nil, "", err
	} else if !fi.IsDir() {
		return nil, "", fmt.Errorf("%s is not a directory", src)
	}
	shares, err := MergeSharePaths(nil, opts.SharedPaths)
	if err != nil {
		return nil, "", err
	}
	dir := filepath.Join(reposDir(home), name)
	if err := os.MkdirAll(reposDir(home), 0o755); err != nil {
		return nil, "", err
	}
	r := &Repo{Name: name, Kind: KindDir, SharedPaths: shares, dir: dir}
	created := false
	switch err := os.Mkdir(dir, 0o755); {
	case err == nil:
		created = true
		if err := os.Mkdir(filepath.Join(dir, "bases"), 0o755); err != nil {
			os.RemoveAll(dir)
			return nil, "", err
		}
	case !errors.Is(err, os.ErrExist):
		return nil, "", err
	}
	unlock, err := flock(filepath.Join(dir, ".repo.lock"), true)
	if err != nil {
		if created {
			os.RemoveAll(dir)
		}
		return nil, "", err
	}
	defer unlock()
	if !created {
		if r, err = Open(home, name); err != nil {
			return nil, "", err
		}
		if r.IsGit() {
			return nil, "", fmt.Errorf("repo %q is a git repository; snapshot a checkout of it with git worktree add --from-dir", name)
		}
		if len(opts.SharedPaths) > 0 {
			return nil, "", fmt.Errorf("repo %q is already registered; change its shared paths with share and unshare", name)
		}
	}
	// Until the snapshot is current, nothing refers to it: keep base prune
	// out the way spawn does.
	unlockBases, err := r.LockBases(false)
	if err != nil {
		if created {
			os.RemoveAll(dir)
		}
		return nil, "", err
	}
	defer unlockBases()
	id, err := r.snapshotPlain(ctx, src)
	if err != nil {
		if created {
			os.RemoveAll(dir)
		}
		return nil, "", err
	}
	r.Source, r.Current = src, id
	if err := r.save(); err != nil {
		if created {
			os.RemoveAll(dir)
		}
		return nil, "", err
	}
	return r, id, nil
}

// snapshotPlain copies src into a new base and returns its id.
func (r *Repo) snapshotPlain(ctx context.Context, src string) (string, error) {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := "snap-" + time.Now().UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b)
	tmp, err := os.MkdirTemp(filepath.Join(r.dir, "bases"), ".tmp-"+id+"-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	work := filepath.Join(tmp, "tree")
	if err := reflinkCopy(ctx, src, work); err != nil {
		return "", fmt.Errorf("snapshot %s: %w", src, err)
	}
	return id, os.Rename(work, r.BaseDir(id))
}
