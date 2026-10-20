// Package repo manages registered repositories: a shared bare object store
// per repository, and read-only base checkouts per commit that sessions are
// layered on.
//
// Layout under the tingly-fuse home:
//
//	repos/<name>/repo.json      registration (upstream URL)
//	repos/<name>/objects.git    bare mirror, the only place objects live
//	repos/<name>/bases/<sha>/   checkout of <sha>; .git uses alternates
//	                            into objects.git and is never written to
//	repos/<name>/shared.git     with SharedGit only: refs, config and new
//	                            objects every session shares (sharedgit.go)
//	repos/<name>/shares/<path>  the one copy of each shared path (shares.go)
package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/tingly-dev/tingly-fuse/internal/gitx"
)

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Repo is a registered repository.
type Repo struct {
	Name     string `json:"name"`
	Upstream string `json:"upstream"`
	// SharedGit makes every session a worktree of one shared repository
	// instead of giving each its own .git (ADR 0010). Fixed at Add.
	SharedGit bool `json:"shared_git,omitempty"`
	// SharedPaths are paths of the tree every new session shares with the
	// others instead of getting its own copy (ADR 0011).
	SharedPaths []string `json:"shared_paths,omitempty"`

	dir string
}

// Dir is the repository's directory under the tingly-fuse home.
func (r *Repo) Dir() string { return r.dir }

// ObjectsDir is the bare repository holding all objects.
func (r *Repo) ObjectsDir() string { return filepath.Join(r.dir, "objects.git") }

// BaseDir is where the base checkout for sha lives.
func (r *Repo) BaseDir(sha string) string { return filepath.Join(r.dir, "bases", sha) }

func reposDir(home string) string { return filepath.Join(home, "repos") }

// AddOptions are the settings a repository is registered with.
type AddOptions struct {
	SharedGit   bool
	SharedPaths []string // see SetShares
}

// Add registers upstream under name and mirrors it into a bare object store.
func Add(ctx context.Context, home, name, upstream string, opts AddOptions) (*Repo, error) {
	if !validName.MatchString(name) {
		return nil, fmt.Errorf("invalid repo name %q", name)
	}
	shares, err := MergeSharePaths(nil, opts.SharedPaths)
	if err != nil {
		return nil, err
	}
	r := &Repo{Name: name, Upstream: upstream, SharedGit: opts.SharedGit, SharedPaths: shares, dir: filepath.Join(reposDir(home), name)}
	if _, err := os.Stat(r.dir); err == nil {
		return nil, fmt.Errorf("repo %q already registered", name)
	}
	if err := os.MkdirAll(filepath.Join(r.dir, "bases"), 0o755); err != nil {
		return nil, err
	}
	cleanup := func() { os.RemoveAll(r.dir) }
	if _, err := gitx.Run(ctx, r.dir, "clone", "--quiet", "--bare", upstream, r.ObjectsDir()); err != nil {
		cleanup()
		return nil, err
	}
	// A bare clone has no fetch refspec; mirror branches so later fetches
	// keep refs/heads current.
	if _, err := gitx.Run(ctx, r.ObjectsDir(), "config", "remote.origin.fetch", "+refs/heads/*:refs/heads/*"); err != nil {
		cleanup()
		return nil, err
	}
	if r.SharedGit {
		if err := r.initShared(ctx); err != nil {
			cleanup()
			return nil, err
		}
	}
	if err := r.save(); err != nil {
		cleanup()
		return nil, err
	}
	return r, nil
}

// Open loads a registered repository.
func Open(home, name string) (*Repo, error) {
	dir := filepath.Join(reposDir(home), name)
	data, err := os.ReadFile(filepath.Join(dir, "repo.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("repo %q is not registered", name)
	} else if err != nil {
		return nil, err
	}
	r := &Repo{dir: dir}
	if err := json.Unmarshal(data, r); err != nil {
		return nil, fmt.Errorf("read repo %q: %w", name, err)
	}
	return r, nil
}

// List returns all registered repositories.
func List(home string) ([]*Repo, error) {
	entries, err := os.ReadDir(reposDir(home))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var out []*Repo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		r, err := Open(home, e.Name())
		if err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// Remove deletes a registered repository: its object store, submodule
// mirrors and every base checkout. Callers must drop sessions first; a view
// layered on a removed base stops working.
func Remove(home, name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("invalid repo name %q", name)
	}
	r, err := Open(home, name)
	if err != nil {
		return err
	}
	return os.RemoveAll(r.dir)
}

// Bases lists the commits that have a base checkout.
func (r *Repo) Bases() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(r.dir, "bases"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".tmp-") {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// Fetch updates the object store from upstream.
// Concurrent calls share one fetch: a call that waited on another one
// started after it does not fetch again.
func (r *Repo) Fetch(ctx context.Context) error {
	if r.SharedGit {
		if err := r.syncSharedRefs(ctx); err != nil {
			return fmt.Errorf("sync shared refs into mirror: %w", err)
		}
	}
	return fetchMirror(ctx, r.ObjectsDir(), time.Now())
}

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Resolve turns a revision into a commit sha. Unless rev is a full commit id
// the mirror already has, it fetches first, so a branch name means what it
// means upstream now and not when the mirror was last updated. With fetch
// unset it only fetches when rev is unknown locally.
func (r *Repo) Resolve(ctx context.Context, rev string, fetch bool) (string, error) {
	local := func() (string, error) {
		return gitx.Run(ctx, r.ObjectsDir(), "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	}
	fetched := false
	if fetch && !fullSHA.MatchString(rev) {
		if err := r.Fetch(ctx); err != nil {
			return "", fmt.Errorf("resolve %q: %w (pass --no-fetch to use the local mirror as is)", rev, err)
		}
		fetched = true
	}
	if sha, err := local(); err == nil || fetched {
		if err != nil {
			return "", fmt.Errorf("unknown revision %q", rev)
		}
		return sha, nil
	}
	if ferr := r.Fetch(ctx); ferr != nil {
		return "", fmt.Errorf("resolve %q: %w", rev, ferr)
	}
	sha, err := local()
	if err != nil {
		return "", fmt.Errorf("unknown revision %q", rev)
	}
	return sha, nil
}

// EnsureBase materializes the base checkout for sha if it does not exist
// yet and returns its path. Concurrent callers are safe: each builds in a
// private temp dir and the first rename wins.
func (r *Repo) EnsureBase(ctx context.Context, sha string) (string, error) {
	final := r.BaseDir(sha)
	if _, err := os.Stat(final); err == nil {
		return final, nil
	}
	tmp, err := os.MkdirTemp(filepath.Join(r.dir, "bases"), ".tmp-"+sha[:12]+"-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp) // no-op after a successful rename
	work := filepath.Join(tmp, "tree")
	// --shared writes objects/info/alternates pointing at objects.git, so the
	// base (and every view layered on it) holds no objects of its own. This
	// keeps git from freshening pack files inside a read-only lower layer.
	if _, err := gitx.Run(ctx, tmp, "clone", "--quiet", "--shared", "--no-checkout", r.ObjectsDir(), work); err != nil {
		return "", err
	}
	if _, err := gitx.Run(ctx, work, "checkout", "--quiet", "--detach", sha); err != nil {
		return "", err
	}
	// Sessions push to the real upstream, not to the local mirror. This must
	// precede submodule init, which resolves relative URLs against origin.
	if _, err := gitx.Run(ctx, work, "remote", "set-url", "origin", r.Upstream); err != nil {
		return "", err
	}
	if err := r.initSubmodules(ctx, work); err != nil {
		return "", err
	}
	if err := os.Rename(work, final); err != nil {
		if _, statErr := os.Stat(final); statErr == nil {
			return final, nil // another spawn won the race
		}
		return "", err
	}
	return final, nil
}
